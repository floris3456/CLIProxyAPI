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

type deliveryGate struct {
	entered chan Record
	release chan error
}

func (g *deliveryGate) HandleUsage(ctx context.Context, r Record) {
	_ = g.HandleUsageAcknowledged(ctx, r)
}
func (g *deliveryGate) HandleUsageAcknowledged(_ context.Context, r Record) error {
	g.entered <- r
	return <-g.release
}

func newDeliveryTestManager(t *testing.T) (*Manager, *atomic.Int64) {
	t.Helper()
	m := NewManager(4)
	clock := &atomic.Int64{}
	clock.Store(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	m.mu.Lock()
	m.deliveryLocked().now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	m.mu.Unlock()
	m.Start(context.Background())
	t.Cleanup(func() {
		m.Stop()
		select {
		case <-m.done:
		case <-time.After(5 * time.Second):
			t.Error("dispatcher did not stop")
		}
	})
	return m, clock
}
func newDeliveryGate(t *testing.T) *deliveryGate {
	t.Helper()
	g := &deliveryGate{entered: make(chan Record, 16), release: make(chan error, 16)}
	t.Cleanup(func() { close(g.release) })
	return g
}
func enteredDelivery(t *testing.T, g *deliveryGate, id string) {
	t.Helper()
	select {
	case r := <-g.entered:
		if r.RequestID != id {
			t.Fatalf("callback %q, want %q", r.RequestID, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not start")
	}
}
func snapshotDelivery(t *testing.T, m *Manager) pluginapi.HostUsageDeliveryResponse {
	t.Helper()
	s := m.DeliverySnapshot("plugin:pacer", "claude")
	if s.Supported || s.Coverage != "unsupported" || s.Through != nil {
		t.Fatalf("unproven coverage: %+v", s)
	}
	return s
}
func assertDeliveryTime(t *testing.T, got *time.Time, want time.Time) {
	t.Helper()
	if got == nil || !got.Equal(want) {
		t.Fatalf("source minimum=%v want %v", got, want)
	}
}

func TestPacerDeliveryQueuedOldRecordAndLiveStream(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	live, start := m.BeginDelivery("claude")
	clock.Add(int64(time.Minute))
	m.RegisterNamed("plugin:pacer", g)
	second, start2 := m.BeginDelivery("claude")
	m.PublishDelivery(context.Background(), Record{Provider: "claude", RequestID: "second", RequestedAt: start2}, second, true)
	second.Finish()
	enteredDelivery(t, g, "second")
	old := start.Add(-time.Hour)
	m.Publish(context.Background(), Record{Provider: "claude", RequestID: "late-old", RequestedAt: old})
	s := snapshotDelivery(t, m)
	if s.Sources.Queued != 1 || s.Sources.Dispatching != 1 || s.Sources.OpenReporters != 1 || s.Delivery.CallbackAcknowledged != 0 {
		t.Fatalf("outstanding=%+v", s)
	}
	assertDeliveryTime(t, s.Sources.EarliestReporterStart, start)
	assertDeliveryTime(t, s.Sources.EarliestPendingStart, old)
	clock.Add(int64(365 * 24 * time.Hour))
	unchanged := snapshotDelivery(t, m)
	if unchanged.Sequence != s.Sequence {
		t.Fatal("reading/time advanced delivery sequence")
	}
	g.release <- nil
	enteredDelivery(t, g, "late-old")
	m.Publish(context.Background(), Record{Provider: "other", RequestID: "barrier", RequestedAt: start2})
	g.release <- nil
	enteredDelivery(t, g, "barrier")
	s = snapshotDelivery(t, m)
	if s.Delivery.CallbackAcknowledged != 2 || s.Sources.UntrackedPublications != 1 || s.Sources.Queued+s.Sources.Dispatching != 0 || s.Delivery.PreRegistrationRecords != 1 {
		t.Fatalf("drained records=%+v", s)
	}
	if s.Sources.OpenReporters != 1 {
		t.Fatal("stream cleared by queue drain")
	}
	// Finish before publish cannot expose a false zero-outstanding interval.
	live.Finish()
	s = snapshotDelivery(t, m)
	if s.Sources.FinishedUnpublished != 1 {
		t.Fatal("missing publication guard disappeared")
	}
	m.PublishDelivery(context.Background(), Record{Provider: "claude", RequestID: "live-final", RequestedAt: start}, live, true)
	s = snapshotDelivery(t, m)
	if s.Sources.OpenReporters != 0 || s.Sources.Queued != 1 {
		t.Fatalf("non-atomic transfer=%+v", s)
	}
	assertDeliveryTime(t, s.Sources.EarliestPendingStart, start)
	g.release <- nil
	enteredDelivery(t, g, "live-final")
	g.release <- nil
}

func TestPacerDeliveryPublicationDoesNotFinishPhysicalProducer(t *testing.T) {
	for _, outcome := range []string{"terminal_stream_event", "canceled", "truncated"} {
		t.Run(outcome, func(t *testing.T) {
			m, _ := newDeliveryTestManager(t)
			g := newDeliveryGate(t)
			m.RegisterNamed("plugin:pacer", g)
			ticket, start := m.BeginDelivery("claude")
			ctx, cancel := context.WithCancel(context.Background())
			if outcome == "canceled" {
				cancel()
			}
			defer cancel()
			m.PublishDelivery(ctx, Record{Provider: "claude", RequestID: outcome, RequestedAt: start, Stream: true, Failed: outcome != "terminal_stream_event"}, ticket, true)
			enteredDelivery(t, g, outcome)
			m.Publish(context.Background(), Record{Provider: "other", RequestID: "barrier", RequestedAt: start})
			g.release <- nil
			enteredDelivery(t, g, "barrier")
			s := snapshotDelivery(t, m)
			if s.Sources.OpenReporters != 1 || s.Delivery.CallbackAcknowledged != 1 {
				t.Fatalf("physical guard=%+v", s)
			}
			ticket.Finish()
			ticket.Finish()
			if s = snapshotDelivery(t, m); s.Sources.OpenReporters != 0 {
				t.Fatalf("finish=%+v", s)
			}
			g.release <- nil
		})
	}
}

func TestPacerDeliveryMissingRecoveryReloadAndStaleAck(t *testing.T) {
	m, clock := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	initial := snapshotDelivery(t, m)
	publish := func(id string) {
		ticket, start := m.BeginDelivery("claude")
		m.PublishDelivery(context.Background(), Record{Provider: "claude", RequestID: id, RequestedAt: start}, ticket, true)
		ticket.Finish()
	}
	publish("missing")
	enteredDelivery(t, g, "missing")
	g.release <- ErrDeliveryMissing
	publish("recovered")
	enteredDelivery(t, g, "recovered")
	if s := snapshotDelivery(t, m); s.Delivery.CallbackMissing != 1 || s.Delivery.CallbackAcknowledged != 0 {
		t.Fatalf("miss erased=%+v", s)
	}
	clock.Add(int64(time.Second))
	m.RegisterNamed("plugin:pacer", g)
	reloaded := snapshotDelivery(t, m)
	if initial.IncarnationID == reloaded.IncarnationID || initial.ManagerIncarnationID != reloaded.ManagerIncarnationID || reloaded.Delivery.RegistrationGaps != 2 {
		t.Fatalf("incarnation=%+v", reloaded)
	}
	g.release <- nil
	publish("new-generation")
	enteredDelivery(t, g, "new-generation")
	s := snapshotDelivery(t, m)
	if s.Delivery.StaleCompletions != 1 || s.Delivery.CallbackAcknowledged != 0 || s.Delivery.LastAcknowledgedSequence != 0 {
		t.Fatalf("old callback acknowledged new generation=%+v", s)
	}
	g.release <- nil
}

func TestPacerDeliveryErrorsCannotBecomeAcknowledged(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	for i, err := range []error{errors.New("dummy persistence failure"), ErrDeliveryFused, ErrDeliveryUnacknowledged} {
		ticket, start := m.BeginDelivery("claude")
		id := []string{"failed", "fused", "legacy"}[i]
		m.PublishDelivery(context.Background(), Record{Provider: "claude", RequestID: id, RequestedAt: start}, ticket, true)
		ticket.Finish()
		enteredDelivery(t, g, id)
		g.release <- err
	}
	m.Publish(context.Background(), Record{Provider: "other", RequestID: "barrier"})
	enteredDelivery(t, g, "barrier")
	s := snapshotDelivery(t, m)
	if s.Delivery.CallbackErrors != 1 || s.Delivery.CallbackFused != 1 || s.Delivery.CallbackUnacknowledged != 1 || s.Delivery.CallbackAcknowledged != 0 {
		t.Fatalf("error accounting=%+v", s)
	}
	g.release <- nil
}

func TestPacerDeliveryNoSinkAndRejectedPublication(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	ticket, start := m.BeginDelivery("claude")
	m.PublishDelivery(context.Background(), Record{Provider: "claude", RequestedAt: start}, ticket, true)
	ticket.Finish()
	m.Stop()
	<-m.done
	s := snapshotDelivery(t, m)
	if s.Sources.MissingSinkRecords != 1 || s.Reason != "sink_not_registered" {
		t.Fatalf("missing sink=%+v", s)
	}
	// Registration after drained history is not retroactive certification.
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	m.Publish(context.Background(), Record{Provider: "claude", RequestedAt: start})
	s = snapshotDelivery(t, m)
	if !s.Closed || s.Sources.RejectedPublications != 1 || s.Sources.MissingSinkRecords != 1 || s.Delivery.RegistrationGaps != 1 {
		t.Fatalf("failed publish=%+v", s)
	}
}

func TestPacerDeliveryConcurrentPublicationNeverLosesOldest(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	ticket, start := m.BeginDelivery("claude")
	goStart := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-goStart; ticket.Finish() }()
	go func() {
		defer wg.Done()
		<-goStart
		m.PublishDelivery(context.Background(), Record{Provider: "claude", RequestID: "concurrent", RequestedAt: start}, ticket, true)
	}()
	close(goStart)
	for i := 0; i < 1000; i++ {
		s := snapshotDelivery(t, m)
		if s.Sources.OpenReporters+s.Sources.Queued+s.Sources.Dispatching == 0 {
			t.Fatal("start->publish->queue gap")
		}
		if s.Sources.EarliestReporterStart != nil {
			assertDeliveryTime(t, s.Sources.EarliestReporterStart, start)
		} else {
			assertDeliveryTime(t, s.Sources.EarliestPendingStart, start)
		}
	}
	wg.Wait()
	enteredDelivery(t, g, "concurrent")
	g.release <- nil
}

func TestPacerDeliveryBoundedMetadataAndUnknownSources(t *testing.T) {
	m, _ := newDeliveryTestManager(t)
	g := newDeliveryGate(t)
	m.RegisterNamed("plugin:pacer", g)
	var tickets []*DeliveryTicket
	for i := 0; i < maxDeliveryTracking+2; i++ {
		ticket, _ := m.BeginDelivery("claude")
		tickets = append(tickets, ticket)
	}
	s := snapshotDelivery(t, m)
	if s.Sources.OpenReporters != maxDeliveryTracking || s.Sources.TrackingOverflows != 2 {
		t.Fatalf("bounded tracking=%+v", s)
	}
	m.Publish(context.Background(), Record{Provider: "claude", RequestID: "unknown-custom", ExecutorType: "CustomExecutor"})
	enteredDelivery(t, g, "unknown-custom")
	s = snapshotDelivery(t, m)
	if s.Sources.UntrackedPublications != 1 || s.Sources.InvalidSourceTimes != 1 {
		t.Fatalf("custom source=%+v", s)
	}
	g.release <- nil
	for _, ticket := range tickets {
		ticket.Finish()
	}
	if s = snapshotDelivery(t, m); s.Sources.FinishedUnpublished != maxDeliveryTracking {
		t.Fatal("unpublished source silently expired")
	}
}
