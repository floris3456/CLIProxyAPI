package usage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func sourceRecord(t *DeliveryTicket, id string) Record {
	return Record{Provider: "claude", AuthID: t.source.state.scope.AuthID, AuthIndex: t.source.state.scope.AuthIndex, RequestID: id, RequestedAt: t.source.start}
}
func sourceCheck(m *Manager, id string) pluginapi.HostUsageCheckpointResponse {
	return m.SourceCheckpoint("plugin:pacer", sourceScope(id, "index-"+id))
}
func sourceBarrier(t *testing.T, m *Manager, g *deliveryGate) {
	t.Helper()
	m.Publish(context.Background(), Record{Provider: "other", RequestID: "barrier"})
	enteredDelivery(t, g, "barrier")
}

func TestPacerSourceCheckpointKnownWindowWaitsForPhysicalAndAck(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	ticket, start := m.BeginClaudeSource(nil, "a", "index-a", true)
	clock.Add(int64(time.Second))
	ticket.MarkSourceDispatch(true)
	r := sourceRecord(ticket, "stream")
	m.PublishDelivery(context.Background(), r, ticket, true)
	enteredDelivery(t, g, "stream")
	clock.Add(int64(time.Hour))
	s := sourceCheck(m, "a")
	if s.Coverage != "known" || s.Active != 1 || s.Pending != 1 {
		t.Fatalf("live=%+v", s)
	}
	assertDeliveryTime(t, s.Through, start)
	ticket.Finish()
	s = sourceCheck(m, "a")
	if s.Active != 0 || s.Pending != 1 {
		t.Fatalf("unacked=%+v", s)
	}
	assertDeliveryTime(t, s.Through, start)
	g.release <- nil
	sourceBarrier(t, m, g)
	s = sourceCheck(m, "a")
	if s.Coverage != "known" || s.Active+s.Pending != 0 || s.GapCount != 0 {
		t.Fatalf("checkpoint=%+v", s)
	}
	assertDeliveryTime(t, s.Through, time.Unix(0, clock.Load()).UTC())
	clock.Add(int64(time.Second))
	newer := sourceCheck(m, "a")
	if newer.Sequence <= s.Sequence || !newer.Through.After(*s.Through) {
		t.Fatal("reordered idle proofs cannot be distinguished")
	}
	g.release <- nil
}

func TestPacerSourceCheckpointLateUntrackedInvalidatesOnlyMatchingAccount(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	for _, id := range []string{"a", "b"} {
		ticket, _ := m.BeginClaudeSource(nil, id, "index-"+id, true)
		ticket.MarkSourceDispatch(true)
		m.PublishDelivery(context.Background(), sourceRecord(ticket, id), ticket, true)
		ticket.Finish()
		enteredDelivery(t, g, id)
		g.release <- nil
	}
	sourceBarrier(t, m, g)
	clock.Add(int64(time.Hour))
	aBefore, bBefore := sourceCheck(m, "a"), sourceCheck(m, "b")
	if aBefore.Coverage != "known" || bBefore.Coverage != "known" {
		t.Fatal("scoped proof unavailable before gap")
	}
	old := aBefore.From.Add(-time.Hour)
	// A forged marker cannot turn a direct/late publication into a tracked source.
	m.Publish(context.Background(), Record{Provider: "claude", AuthID: "a", AuthIndex: "index-a", RequestID: "late-old", RequestedAt: old, SourceEvidence: &pluginapi.UsageSourceEvidence{Tracked: true, Scope: sourceScope("a", "index-a")}})
	a, b := sourceCheck(m, "a"), sourceCheck(m, "b")
	if a.Coverage != "unknown" || a.Through != nil || a.GapCount != 1 || a.Sequence <= aBefore.Sequence {
		t.Fatalf("late gap=%+v", a)
	}
	assertDeliveryTime(t, a.InvalidFrom, old)
	assertDeliveryTime(t, a.Gaps[0].From, old)
	if a.Gaps[0].Through != nil || b.Coverage != "known" || b.GapCount != 0 {
		t.Fatalf("scope leak: a=%+v b=%+v", a, b)
	}
	g.release <- nil
	select {
	case record := <-g.entered:
		if record.RequestID != "late-old" || record.SourceEvidence == nil || record.SourceEvidence.Tracked || record.SourceEvidence.Gap == nil {
			t.Fatalf("gap evidence=%+v", record.SourceEvidence)
		}
		assertDeliveryTime(t, record.SourceEvidence.Gap.From, old)
	case <-time.After(5 * time.Second):
		t.Fatal("gap record not dispatched")
	}
	g.release <- nil
	// Unrelated Codex publication cannot invalidate Claude b.
	m.Publish(context.Background(), Record{Provider: "codex", AuthID: "codex-account", AuthIndex: "codex-index", RequestID: "codex"})
	enteredDelivery(t, g, "codex")
	if sourceCheck(m, "b").Coverage != "known" {
		t.Fatal("other provider poisoned proven account")
	}
	g.release <- nil
}

func TestPacerSourceCheckpointAtomicAdmissionClaimFinishAndPublish(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	admitted, start := m.BeginClaudeSource(nil, "a", "index-a", false)
	claimed, reporterStart := m.BeginClaudeSource(WithSourceAdmission(context.Background(), admitted), "a", "index-a", true)
	if admitted != claimed || !start.Equal(reporterStart) {
		t.Fatal("admission did not transfer immutable start")
	}
	admitted.ReleaseUnclaimed()
	goStart := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-goStart; claimed.Finish() }()
	go func() {
		defer wg.Done()
		<-goStart
		m.PublishDelivery(context.Background(), sourceRecord(claimed, "final"), claimed, true)
	}()
	close(goStart)
	for range 1000 {
		s := sourceCheck(m, "a")
		if s.Coverage != "known" || s.Active+s.Pending == 0 {
			t.Fatalf("admission/publication gap=%+v", s)
		}
		assertDeliveryTime(t, s.Through, start)
	}
	wg.Wait()
	enteredDelivery(t, g, "final")
	g.release <- nil
}

func TestPacerSourceCheckpointSinkFailureAndReloadCannotCertify(t *testing.T) {
	for _, failure := range []error{errors.New("dummy persistence failure"), ErrDeliveryMissing, ErrDeliveryFused, ErrDeliveryUnacknowledged} {
		t.Run(failure.Error(), func(t *testing.T) {
			m, _ := newDeliveryTestManager(t)
			g := newDeliveryGate(t)
			m.RegisterNamed("plugin:pacer", g)
			ticket, start := m.BeginClaudeSource(nil, "a", "index-a", true)
			m.PublishDelivery(context.Background(), sourceRecord(ticket, "failed"), ticket, true)
			ticket.Finish()
			enteredDelivery(t, g, "failed")
			g.release <- failure
			sourceBarrier(t, m, g)
			s := sourceCheck(m, "a")
			if s.Coverage != "unknown" || s.Through != nil || s.GapCount != 1 {
				t.Fatalf("failed callback=%+v", s)
			}
			assertDeliveryTime(t, s.InvalidFrom, start)
			g.release <- nil
		})
	}
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	ticket, _ := m.BeginClaudeSource(nil, "a", "index-a", true)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, "old-plugin"), ticket, true)
	ticket.Finish()
	enteredDelivery(t, g, "old-plugin")
	old := sourceCheck(m, "a")
	clock.Add(int64(time.Second))
	m.RegisterNamed("plugin:pacer", g)
	newer := sourceCheck(m, "a")
	if old.IncarnationID == newer.IncarnationID || newer.Coverage == "known" {
		t.Fatalf("reload covered older pending=%+v", newer)
	}
	g.release <- nil
	sourceBarrier(t, m, g)
	if s := sourceCheck(m, "a"); s.Coverage != "unknown" || s.Gaps[0].Reason != "stale_callback" {
		t.Fatalf("stale ack=%+v", s)
	}
	g.release <- nil
}

func TestPacerSourceCheckpointUnsupportedBeforeDispatchAndWildcardGap(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	for _, id := range []string{"a", "b"} {
		_, _ = m.BeginClaudeSource(nil, id, "index-"+id, true)
	}
	m.NoteSourceGap("claude", "a", "index-a", "unsupported_executor_admission", time.Time{})
	if sourceCheck(m, "a").Coverage != "unknown" || sourceCheck(m, "b").Coverage != "known" {
		t.Fatal("unsupported admission was not scoped")
	}
	m.NoteSourceGap("claude", "", "", "unattributable_source", time.Time{})
	if s := sourceCheck(m, "b"); s.Coverage != "unknown" || s.InvalidFrom != nil {
		t.Fatalf("missing identity became precise=%+v", s)
	}
	unsupported := m.SourceCheckpoint("plugin:pacer", pluginapi.UsageSourceScope{Provider: "codex", AuthID: "a", AuthIndex: "index-a", Kind: "codex", Version: 1})
	if unsupported.Coverage != "unsupported" || unsupported.Through != nil {
		t.Fatal("unsupported source granted proof")
	}
}

func TestPacerSourceCheckpointClockRollbackAndBoundedGapInvalidation(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	ticket, start := m.BeginClaudeSource(nil, "a", "index-a", true)
	for i := 0; i < maxSourceGaps+3; i++ {
		m.NoteSourceGap("claude", "a", "index-a", "unsupported", start.Add(-time.Duration(i)*time.Hour))
	}
	s := sourceCheck(m, "a")
	if !s.GapsTruncated || len(s.Gaps) != maxSourceGaps {
		t.Fatalf("gap bound=%+v", s)
	}
	assertDeliveryTime(t, s.InvalidFrom, start.Add(-time.Duration(maxSourceGaps+2)*time.Hour))
	ticket.Finish()
	clock.Add(-int64(time.Second))
	if s = sourceCheck(m, "a"); s.Coverage != "unknown" || s.Through != nil {
		t.Fatal("clock rollback granted horizon")
	}
}

func TestPacerSourceCheckpointTrackingCapCannotCertifyOmittedStarts(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	for range maxSourceFlights + 1 {
		_, _ = m.BeginClaudeSource(nil, "a", "index-a", true)
	}
	s := sourceCheck(m, "a")
	if s.Coverage != "unknown" || s.Through != nil || s.Active != maxSourceFlights || s.GapCount == 0 {
		t.Fatalf("omitted start granted proof=%+v", s)
	}
}

func TestPacerSourceCheckpointFutureUntrackedTimeHasUnknownGapBeginning(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	_, _ = m.BeginClaudeSource(nil, "a", "index-a", true)
	m.Publish(context.Background(), Record{Provider: "claude", AuthID: "a", AuthIndex: "index-a", RequestID: "future", RequestedAt: time.Unix(0, clock.Load()).Add(time.Hour)})
	enteredDelivery(t, g, "future")
	s := sourceCheck(m, "a")
	if s.Coverage != "unknown" || s.GapCount == 0 || s.InvalidFrom != nil || s.Gaps[0].From != nil {
		t.Fatalf("future publisher clock narrowed invalidation: %+v", s)
	}
	g.release <- nil
}

func TestPacerSourceCheckpointUsesSerializedWallClock(t *testing.T) {
	m := NewManager(1)
	defer m.Stop()
	_, start := m.BeginClaudeSource(nil, "a", "index-a", true)
	if start != start.Round(0) || start.Location() != time.UTC {
		t.Fatal("source time retained monotonic/local components")
	}
}

func TestPacerSourceCheckpointRegistrationCannotCertifyEqualTimeHistory(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	oldSink := newDeliveryGate(t)
	newSink := newDeliveryGate(t)
	m.RegisterNamed("other-sink", oldSink)
	ticket, start := m.BeginClaudeSource(nil, "a", "index-a", true)
	m.PublishDelivery(context.Background(), sourceRecord(ticket, "pre-registration"), ticket, true)
	ticket.Finish()
	enteredDelivery(t, oldSink, "pre-registration") // dispatch list captured before Pacer exists
	m.RegisterNamed("plugin:pacer", newSink)        // deliberately same source clock
	oldSink.release <- nil
	m.Publish(context.Background(), Record{Provider: "other", RequestID: "barrier"})
	enteredDelivery(t, oldSink, "barrier")
	clock.Add(int64(time.Second))
	s := sourceCheck(m, "a")
	if s.Coverage != "known" || s.From == nil || !s.From.After(start) {
		t.Fatalf("registration retro-certified same-time record it never received: %+v", s)
	}
	oldSink.release <- nil
	enteredDelivery(t, newSink, "barrier")
	newSink.release <- nil
}

func TestPacerSourceUnclaimedAdmissionEmitsGapWithoutPhantomUsage(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	ticket, _ := m.BeginClaudeSource(nil, "a", "index-a", false)
	ticket.ReleaseUnclaimed()
	s := sourceCheck(m, "a")
	d := m.DeliverySnapshot("plugin:pacer", "claude")
	if s.Coverage != "unknown" || s.Active+s.Pending != 0 || s.Gaps[0].Reason != "admission_without_reporter" || d.Sources.Published != 0 || d.Sources.OpenReporters != 0 {
		t.Fatalf("abandoned admission=%+v diagnostics=%+v", s, d)
	}
	_, _ = m.BeginClaudeSource(nil, "b", "index-b", true)
	if sourceCheck(m, "b").Coverage != "known" {
		t.Fatal("abandoned account poisoned other source")
	}
}
