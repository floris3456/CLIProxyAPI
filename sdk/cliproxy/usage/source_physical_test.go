package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func physicalManager(t *testing.T) (*Manager, *atomic.Int64, *deliveryGate) {
	t.Helper()
	// Same private construction path as the process singleton, with no public
	// authority setter. These are state-machine fixtures, not native TLS proof.
	m := newRuntimeUsageManager()
	clock := &atomic.Int64{}
	clock.Store(time.Date(2030, 1, 1, 0, 0, 0, 137, time.UTC).UnixNano())
	m.mu.Lock()
	m.deliveryLocked().now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	m.mu.Unlock()
	m.Start(context.Background())
	t.Cleanup(func() {
		m.Stop()
		select {
		case <-m.done:
		case <-time.After(5 * time.Second):
			t.Error("dispatcher not closed")
		}
	})
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	return m, clock, g
}
func physicalStart(m *Manager, id string) (*DeliveryTicket, *SourceDispatch) {
	ticket, _ := m.BeginClaudeSource(nil, id, "index-"+id, true)
	ticket.MarkSourceDispatch(true)
	return ticket, ticket.beginPhysicalDispatch(sourceScope(id, "index-"+id), true, true)
}
func physicalFinish(t *testing.T, m *Manager, g *deliveryGate, ticket *DeliveryTicket, guard *SourceDispatch, id string) {
	t.Helper()
	guard.RoundTripReturned(true, false)
	guard.RequestBodyClosed(false)
	guard.ResponseBodyClosed(false)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, id), ticket, true)
	ticket.Finish()
	enteredDelivery(t, g, id)
	g.release <- nil
	sourceBarrier(t, m, g)
}
func requirePhysicalUnknown(t *testing.T, s pluginapi.HostUsageCheckpointResponse, reason string) {
	t.Helper()
	if s.Physical == nil || s.Physical.Coverage != "unknown" || s.Physical.ThroughCut != nil || reason != "" && s.Physical.Reason != reason {
		t.Fatalf("unsafe physical proof: %+v", s.Physical)
	}
}

func TestPacerPhysicalRequestHorizonCannotCloseAsyncTransportLifetime(t *testing.T) {
	m, clock, g := physicalManager(t)
	ticket, guard := physicalStart(m, "a")
	guard.RoundTripReturned(true, false)
	guard.ResponseBodyClosed(false)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, "early-usage"), ticket, true)
	enteredDelivery(t, g, "early-usage")
	g.release <- nil
	sourceBarrier(t, m, g)
	clock.Add(int64(time.Hour))
	ticket.Finish()
	s := sourceCheck(m, "a")
	// Regression: ace073's request frontier can advance while an asynchronous
	// transport request body remains open. It is not a physical carry-in proof.
	if s.Coverage != "known" || s.Active+s.Pending != 0 {
		t.Fatal(s)
	}
	requirePhysicalUnknown(t, s, "outstanding_admission_or_producer")
	if s.Physical.Outstanding != 1 || s.Physical.LastProducerClosedAt == nil {
		t.Fatal(s.Physical)
	}
	clock.Add(int64(time.Second))
	guard.RequestBodyClosed(false)
	s = sourceCheck(m, "a")
	requirePhysicalUnknown(t, s, "first_quiescent_cut")
	if s.Physical.FromCut == nil || !s.Physical.FromCut.At.After(*s.Physical.LastProducerClosedAt) {
		t.Fatal(s.Physical)
	}
	clock.Add(int64(time.Second))
	s = sourceCheck(m, "a")
	if s.Physical.Coverage != "known" || s.Physical.FromCut.Sequence >= s.Physical.ThroughCut.Sequence {
		t.Fatal(s.Physical)
	}
	g.release <- nil
}

func TestPacerPhysicalAdmissionFinalUsageAndAcknowledgmentAreSeparateGates(t *testing.T) {
	m, clock, g := physicalManager(t)
	ticket, _ := m.BeginClaudeSource(nil, "a", "index-a", false)
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "outstanding_admission_or_producer")
	claimed, _ := m.BeginClaudeSource(WithSourceAdmission(context.Background(), ticket), "a", "index-a", true)
	if claimed != ticket {
		t.Fatal("admission changed")
	}
	guard := ticket.beginPhysicalDispatch(sourceScope("a", "index-a"), true, true)
	guard.RoundTripReturned(true, false)
	guard.RequestBodyClosed(false)
	guard.ResponseBodyClosed(false)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, "terminal"), ticket, true)
	var record Record
	select {
	case record = <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("record missing")
	}
	if record.SourceEvidence.Physical == nil {
		t.Fatal("physical dispatch evidence missing")
	}
	clock.Add(int64(time.Hour))
	s := sourceCheck(m, "a")
	requirePhysicalUnknown(t, s, "outstanding_admission_or_producer")
	if s.Physical.LastProducerClosedAt != nil {
		t.Fatal("final usage fabricated producer close")
	}
	ticket.Finish()
	s = sourceCheck(m, "a")
	requirePhysicalUnknown(t, s, "pending_delivery")
	if !s.Physical.LastProducerClosedAt.After(record.SourceEvidence.FinalUsageAt) {
		t.Fatal("producer clock was not observed independently")
	}
	g.release <- nil
	sourceBarrier(t, m, g)
	s = sourceCheck(m, "a")
	requirePhysicalUnknown(t, s, "first_quiescent_cut")
	clock.Add(int64(time.Second))
	s = sourceCheck(m, "a")
	if s.Physical.Coverage != "known" {
		t.Fatal(s.Physical)
	}
	clone := CloneSourceEvidence(record.SourceEvidence)
	clone.Physical.DispatchSequence = 0
	if record.SourceEvidence.Physical.DispatchSequence == 0 {
		t.Fatal("sink evidence shares mutable dispatch")
	}
	g.release <- nil
}

func TestPacerPhysicalPreRegistrationCarryMustCloseBeforeNewLowerCut(t *testing.T) {
	m, clock, old := physicalManager(t)
	ticket, guard := physicalStart(m, "a")
	guard.RoundTripReturned(true, false)
	guard.RequestBodyClosed(false)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, "old-final"), ticket, true)
	enteredDelivery(t, old, "old-final")
	old.release <- nil
	sourceBarrier(t, m, old)
	// Its only usage was already acknowledged by the old sink. It still owns
	// a live producer and response body during the new sink registration.
	newSink := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", newSink)
	clock.Add(int64(time.Hour))
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "outstanding_admission_or_producer")
	ticket.Finish()
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "outstanding_admission_or_producer")
	clock.Add(int64(time.Second))
	guard.ResponseBodyClosed(false)
	s := sourceCheck(m, "a")
	requirePhysicalUnknown(t, s, "first_quiescent_cut")
	if s.Physical.FromCut.Sequence <= ticket.source.physical.dispatch.DispatchSequence {
		t.Fatal("new sink borrowed old record")
	}
	clock.Add(int64(time.Second))
	s = sourceCheck(m, "a")
	if s.Physical.Coverage != "known" {
		t.Fatal(s.Physical)
	}
	old.release <- nil
}

func TestPacerPhysicalEqualClockFutureDispatchUsesCausalSequence(t *testing.T) {
	m, clock, g := physicalManager(t)
	first, guard := physicalStart(m, "a")
	physicalFinish(t, m, g, first, guard, "first")
	lower := sourceCheck(m, "a").Physical.FromCut
	if lower == nil {
		t.Fatal("no initial cut")
	}
	g.release <- nil
	// No wall-clock advance at all: new dispatch still must sort AFTER the
	// cut. Treating lower.At as a scalar watermark loses this distinction.
	next, g2 := physicalStart(m, "a")
	if next.source.physical.dispatch.DispatchSequence <= lower.Sequence || !next.source.physical.dispatch.StartedAt.Equal(lower.At) {
		t.Fatal("equal-time sequence ordering lost")
	}
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "outstanding_admission_or_producer")
	physicalFinish(t, m, g, next, g2, "next")
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "nonpositive_cut_interval")
	clock.Add(int64(time.Second))
	s := sourceCheck(m, "a")
	if s.Physical.Coverage != "known" || next.source.physical.dispatch.DispatchSequence >= s.Physical.ThroughCut.Sequence {
		t.Fatal(s.Physical)
	}
	g.release <- nil
}

func TestPacerPhysicalSDKManagerNeverManufacturesCreationAuthority(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	ticket, guard := physicalStart(m, "a")
	physicalFinish(t, m, g, ticket, guard, "sdk")
	clock.Add(int64(365 * 24 * time.Hour))
	s := sourceCheck(m, "a")
	if s.Coverage != "known" {
		t.Fatal("request contract changed", s)
	}
	requirePhysicalUnknown(t, s, "admission_authority_unavailable")
	g.release <- nil
}

func TestPacerPhysicalGapsRemainStickyAndAccountQualified(t *testing.T) {
	for _, mode := range []string{"failed", "cancel", "unsupported", "wrong-scope", "body-close", "body-replay", "late-untracked", "clock-rollback", "callback", "fused", "missing", "void"} {
		t.Run(mode, func(t *testing.T) {
			m, clock, g := physicalManager(t)
			ticket, guard := physicalStart(m, "a")
			guard.RoundTripReturned(true, false)
			guard.RequestBodyClosed(false)
			guard.ResponseBodyClosed(mode == "body-close")
			r := sourceRecord(ticket, "bad")
			switch mode {
			case "body-replay":
				guard.RequestBodyReplayed()
			case "failed":
				r.Failed = true
			case "cancel":
				ticket.FailSource("producer_error_or_cancellation")
			case "unsupported":
				m.NoteSourceGap("claude", "a", "index-a", "unsupported_executor_admission", time.Time{})
			case "wrong-scope":
				ticket.beginPhysicalDispatch(sourceScope("b", "index-b"), true, false)
			}
			m.PublishDelivery(context.Background(), r, ticket, true)
			ticket.Finish()
			enteredDelivery(t, g, "bad")
			var failure error
			switch mode {
			case "callback":
				failure = errors.New("dummy persistence error")
			case "fused":
				failure = ErrDeliveryFused
			case "missing":
				failure = ErrDeliveryMissing
			case "void":
				failure = ErrDeliveryUnacknowledged
			}
			g.release <- failure
			sourceBarrier(t, m, g)
			if mode == "late-untracked" {
				m.Publish(context.Background(), Record{Provider: "claude", AuthID: "a", AuthIndex: "index-a", RequestedAt: ticket.source.start.Add(-time.Hour)})
			}
			if mode == "clock-rollback" {
				clock.Add(-int64(time.Second))
			}
			s := sourceCheck(m, "a")
			requirePhysicalUnknown(t, s, "physical_incarnation_gap")
			if s.Physical.GapCount == 0 || s.Physical.InvalidFromSequence != nil {
				t.Fatal("gap silently narrowed to requested time", s.Physical)
			}
			clock.Add(int64(time.Hour))
			requirePhysicalUnknown(t, sourceCheck(m, "a"), "physical_incarnation_gap")
			g.release <- nil
			if mode != "clock-rollback" {
				b, bg := physicalStart(m, "b")
				bg.RoundTripReturned(false, false)
				bg.RequestBodyClosed(false)
				m.PublishDelivery(context.Background(), sourceRecord(b, "b"), b, true)
				b.Finish()
				// The old source can have a queued late gap; the barrier gate's cleanup
				// drains it. Account b must not inherit account a's failure facts.
				if sourceCheck(m, "b").Physical.GapCount != 0 {
					t.Fatal("A gap poisoned B")
				}
			}
		})
	}
}

func TestPacerPhysicalConcurrentCloseFinishPublishCannotExposeCut(t *testing.T) {
	m, clock, g := physicalManager(t)
	ticket, guard := physicalStart(m, "a")
	guard.RoundTripReturned(true, false)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, fn := range []func(){ticket.Finish, func() { guard.RequestBodyClosed(false) }, func() { guard.ResponseBodyClosed(false) }, func() { m.PublishDelivery(context.Background(), sourceRecord(ticket, "race"), ticket, true) }} {
		wg.Add(1)
		go func(f func()) { defer wg.Done(); <-start; f() }(fn)
	}
	close(start)
	for range 200 {
		s := sourceCheck(m, "a")
		requirePhysicalUnknown(t, s, "")
		if s.Physical.FromCut != nil {
			t.Fatal("cut exposed before ack")
		}
	}
	wg.Wait()
	enteredDelivery(t, g, "race")
	g.release <- nil
	sourceBarrier(t, m, g)
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "first_quiescent_cut")
	clock.Add(int64(time.Second))
	if sourceCheck(m, "a").Physical.Coverage != "known" {
		t.Fatal("closed race fixture unavailable")
	}
	g.release <- nil
}

func TestPacerPhysicalPublicSDKCannotAssertDispatchAuthority(t *testing.T) {
	for _, permit := range []any{nil, true, "physicalDispatchPermit", struct{ Qualified bool }{true}} {
		m, clock, g := physicalManager(t)
		ticket, _ := m.BeginClaudeSource(nil, "a", "index-a", true)
		guard := ticket.BeginPhysicalDispatch(sourceScope("a", "index-a"), permit, true)
		physicalFinish(t, m, g, ticket, guard, "untrusted")
		clock.Add(int64(time.Hour))
		s := sourceCheck(m, "a")
		requirePhysicalUnknown(t, s, "physical_incarnation_gap")
		g.release <- nil
	}
}

func TestPacerPhysicalCutSnapshotDetachedAndRestartNeverBorrows(t *testing.T) {
	m, clock, g := physicalManager(t)
	ticket, guard := physicalStart(m, "a")
	physicalFinish(t, m, g, ticket, guard, "one")
	lower := sourceCheck(m, "a")
	sequence := lower.Physical.FromCut.Sequence
	lower.Physical.FromCut.Sequence = 0
	clock.Add(int64(time.Second))
	s := sourceCheck(m, "a")
	if s.Physical.Coverage != "known" || s.Physical.FromCut.Sequence != sequence {
		t.Fatal("snapshot mutation reached state", s.Physical)
	}
	newSink := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", newSink)
	reloaded := sourceCheck(m, "a")
	requirePhysicalUnknown(t, reloaded, "first_quiescent_cut")
	if reloaded.IncarnationID == s.IncarnationID || reloaded.Physical.FromCut.Sequence <= sequence {
		t.Fatal("new sink borrowed old cut")
	}
	// A separately constructed SDK manager has no lifetime authority. Old
	// process/manager identities can never be stitched to its empty queue.
	other, otherClock := newDeliveryTestManager(t)
	og := newDeliveryGate(t)
	other.RegisterNamed("plugin:pacer", og)
	_, _ = other.BeginClaudeSource(nil, "a", "index-a", true)
	otherClock.Add(int64(time.Hour))
	newProof := sourceCheck(other, "a")
	requirePhysicalUnknown(t, newProof, "admission_authority_unavailable")
	if newProof.ManagerIncarnationID == s.ManagerIncarnationID {
		t.Fatal("manager identity reused")
	}
	g.release <- nil
}

func TestPacerPhysicalIdentityAmbiguityAndOverflowCannotProduceCuts(t *testing.T) {
	m, clock, g := physicalManager(t)
	for _, id := range []string{"a", "b"} {
		ticket, guard := physicalStart(m, id)
		physicalFinish(t, m, g, ticket, guard, id)
		sourceCheck(m, id)
		g.release <- nil
	}
	m.NotePhysicalIdentityGap()
	clock.Add(int64(time.Hour))
	for _, id := range []string{"a", "b"} {
		requirePhysicalUnknown(t, sourceCheck(m, id), "physical_incarnation_gap")
	}
	n, _, _ := physicalManager(t)
	for range maxSourceFlights + 1 {
		_, _ = n.BeginClaudeSource(nil, "a", "index-a", false)
	}
	s := sourceCheck(n, "a")
	requirePhysicalUnknown(t, s, "physical_incarnation_gap")
	if s.Physical.Outstanding != maxSourceFlights {
		t.Fatal("omitted admission counted as closed", s.Physical)
	}
}

func TestPacerPhysicalLateGapRevokesKnownCutAndSuccessCannotHeal(t *testing.T) {
	m, clock, g := physicalManager(t)
	ticket, guard := physicalStart(m, "a")
	physicalFinish(t, m, g, ticket, guard, "first")
	sourceCheck(m, "a")
	clock.Add(int64(time.Second))
	known := sourceCheck(m, "a")
	if known.Physical.Coverage != "known" {
		t.Fatal(known.Physical)
	}
	m.NoteSourceGap("claude", "a", "index-a", "late_unsupported_dispatch", time.Time{})
	gap := sourceCheck(m, "a")
	requirePhysicalUnknown(t, gap, "physical_incarnation_gap")
	if gap.Sequence <= known.Sequence || gap.Physical.InvalidFromSequence != nil {
		t.Fatal("late revocation does not cover old proof", gap.Physical)
	}
	g.release <- nil
	next, g2 := physicalStart(m, "a")
	physicalFinish(t, m, g, next, g2, "successful-later")
	clock.Add(int64(time.Hour))
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "physical_incarnation_gap")
	g.release <- nil
}

func TestPacerPhysicalStaleCallbackPoisonsReplacementCut(t *testing.T) {
	m, clock, g := physicalManager(t)
	ticket, guard := physicalStart(m, "a")
	guard.RoundTripReturned(false, false)
	guard.RequestBodyClosed(false)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, "old-callback"), ticket, true)
	ticket.Finish()
	enteredDelivery(t, g, "old-callback")
	next := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", next)
	clock.Add(int64(time.Hour))
	requirePhysicalUnknown(t, sourceCheck(m, "a"), "pending_delivery")
	g.release <- nil
	sourceBarrier(t, m, next)
	s := sourceCheck(m, "a")
	requirePhysicalUnknown(t, s, "physical_incarnation_gap")
	if s.Physical.FromCut != nil || s.Physical.GapCount == 0 {
		t.Fatal("stale callback established a replacement cut", s.Physical)
	}
	next.release <- nil
}
