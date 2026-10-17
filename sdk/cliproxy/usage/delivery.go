package usage

import (
	"container/heap"
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxDeliveryTracking = 8192
const maxDeliverySinks = 64

var (
	ErrDeliveryUnacknowledged = errors.New("usage sink cannot acknowledge persistence")
	ErrDeliveryMissing        = errors.New("usage sink is missing or replaced")
	ErrDeliveryFused          = errors.New("usage sink is fused")
)

type AcknowledgedPlugin interface {
	HandleUsageAcknowledged(context.Context, Record) error
}

// DeliveryTicket tracks a reporter, not a claim that every provider physical
// attempt has been observed. Only explicit Finish releases the physical guard.
// Publication alone (including terminal SSE usage) never finishes that guard.
type DeliveryTicket struct {
	source    *sourceFlight
	manager   *Manager
	provider  int
	entry     *deliveryEntry
	published bool
	finished  bool
}

type deliveryEntry struct {
	start time.Time
	index int
}

type deliveryHeap []*deliveryEntry

func (h deliveryHeap) Len() int           { return len(h) }
func (h deliveryHeap) Less(i, j int) bool { return h[i].start.Before(h[j].start) }
func (h deliveryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *deliveryHeap) Push(x any)        { e := x.(*deliveryEntry); e.index = len(*h); *h = append(*h, e) }
func (h *deliveryHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	e.index = -1
	return e
}
func (h deliveryHeap) earliest() *time.Time {
	if len(h) == 0 {
		return nil
	}
	v := h[0].start
	return &v
}

type deliverySource struct {
	diag      pluginapi.HostUsageSourceDiagnostics
	reporters deliveryHeap
	pending   deliveryHeap
}

type deliverySink struct {
	incarnation          string
	registeredAt         time.Time
	registrationSequence uint64
	diag                 [3]pluginapi.HostUsageSinkDiagnostics
}

type deliveryState struct {
	proof         *sourceProofState
	incarnation   string
	sequence      uint64
	now           func() time.Time
	sources       [3]deliverySource
	sinks         map[int]*deliverySink
	sinkOverflows uint64
}

func deliveryProvider(provider string) int {
	switch provider {
	case "claude":
		return 0
	case "codex":
		return 1
	default:
		return 2
	}
}

func (m *Manager) deliveryLocked() *deliveryState {
	if m.delivery == nil {
		m.delivery = &deliveryState{incarnation: uuid.NewString(), now: time.Now, sinks: make(map[int]*deliverySink)}
	}
	return m.delivery
}

// BeginDelivery records start under the same lock as publish/dispatch/ack. The
// returned clock is the reporter's actual source RequestedAt, never callback time.
func (m *Manager) BeginDelivery(provider string) (*DeliveryTicket, time.Time) {
	if m == nil {
		return nil, time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deliveryLocked()
	d.sequence++
	now := d.now()
	p := deliveryProvider(provider)
	if p == 2 {
		return nil, now
	}
	s := &d.sources[p]
	s.diag.Started++
	t := &DeliveryTicket{manager: m, provider: p}
	if s.reporters.Len() >= maxDeliveryTracking || m.closed {
		s.diag.TrackingOverflows++
		return t, now
	}
	t.entry = &deliveryEntry{start: now}
	heap.Push(&s.reporters, t.entry)
	s.diag.OpenReporters++
	return t, now
}

// Finish must be called after the producer/network body has stopped. If final
// publication has not happened, the guard remains; timers never collect it.
func (t *DeliveryTicket) Finish() {
	if t == nil || t.manager == nil {
		return
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	d := m.deliveryLocked()
	d.sequence++
	m.sourceFinishedLocked(t)
	if t.entry == nil {
		return
	}
	s := &d.sources[t.provider]
	if t.published {
		heap.Remove(&s.reporters, t.entry.index)
		t.entry = nil
		s.diag.OpenReporters--
	} else {
		s.diag.FinishedUnpublished++
	}
}

// PublishDelivery atomically moves a final publication into the queue before
// releasing any finished reporter guard. Additional records do not finalize it.
func (m *Manager) PublishDelivery(ctx context.Context, record Record, t *DeliveryTicket, final bool) {
	m.publish(ctx, record, t, final)
}

func (m *Manager) deliveryPublishLocked(record Record, t *DeliveryTicket, final bool) (uint64, *deliveryEntry) {
	d := m.deliveryLocked()
	d.sequence++
	seq := d.sequence
	p := deliveryProvider(record.Provider)
	s := &d.sources[p]
	valid := t != nil && t.manager == m && t.provider == p && t.entry != nil && !t.published && record.RequestedAt.Equal(t.entry.start)
	if !valid {
		s.diag.UntrackedPublications++
	}
	if m.closed {
		s.diag.RejectedPublications++
		return seq, nil
	}
	s.diag.Published++
	s.diag.Queued++
	var e *deliveryEntry
	if record.RequestedAt.IsZero() || record.RequestedAt.After(d.now()) {
		s.diag.InvalidSourceTimes++
	} else if s.pending.Len() >= maxDeliveryTracking {
		s.diag.TrackingOverflows++
	} else {
		e = &deliveryEntry{start: record.RequestedAt}
		heap.Push(&s.pending, e)
	}
	if valid && final {
		t.published = true
		if t.finished {
			heap.Remove(&s.reporters, t.entry.index)
			t.entry = nil
			s.diag.OpenReporters--
			s.diag.FinishedUnpublished--
		}
	}
	return seq, e
}

func (m *Manager) registerDeliveryLocked(index int) {
	d := m.deliveryLocked()
	d.sequence++
	s := d.sinks[index]
	if s == nil {
		if len(d.sinks) >= maxDeliverySinks {
			d.sinkOverflows++
			return
		}
		s = &deliverySink{}
		d.sinks[index] = s
	}
	s.incarnation = uuid.NewString()
	s.registeredAt = d.now()
	if d.proof != nil {
		s.registeredAt = m.sourceClockLocked()
	}
	s.registrationSequence = d.sequence
	for i := range s.diag {
		s.diag[i].RegistrationGaps++
		s.diag[i].LastAcknowledgedSequence = 0
	}
}

func (m *Manager) acknowledgeDelivery(index int, incarnation string, provider int, seq uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deliveryLocked()
	d.sequence++
	s := d.sinks[index]
	if s == nil {
		return
	}
	v := &s.diag[provider]
	v.CallbackInFlight--
	stale := s.incarnation != incarnation
	if stale {
		v.StaleCompletions++
	}
	switch {
	case errors.Is(err, ErrDeliveryUnacknowledged):
		v.CallbackUnacknowledged++
	case errors.Is(err, ErrDeliveryMissing):
		v.CallbackMissing++
	case errors.Is(err, ErrDeliveryFused):
		v.CallbackFused++
	case err != nil:
		v.CallbackErrors++
	default:
		if stale {
			return
		}
		v.CallbackAcknowledged++
		if seq > v.LastAcknowledgedSequence {
			v.LastAcknowledgedSequence = seq
		}
	}
}

// DeliverySnapshot never waits for a producer/callback. The diagnostic minima
// are NOT a delivery horizon. Coverage is intentionally unsupported even empty:
// Home/custom executors, duplex successors, redirects and side records are not
// all instrumented at physical request start.
func (m *Manager) DeliverySnapshot(name, provider string) pluginapi.HostUsageDeliveryResponse {
	out := pluginapi.HostUsageDeliveryResponse{Provider: provider, Coverage: "unsupported", Reason: "incomplete_source_lifecycle"}
	if m == nil {
		out.Reason = "manager_unavailable"
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deliveryLocked()
	out.ManagerIncarnationID = d.incarnation
	out.Sequence = d.sequence
	out.Closed = m.closed
	p := deliveryProvider(provider)
	if p == 2 {
		out.Reason = "unsupported_provider"
		return out
	}
	s := &d.sources[p]
	out.Sources = s.diag
	out.Sources.EarliestReporterStart = s.reporters.earliest()
	out.Sources.EarliestPendingStart = s.pending.earliest()
	out.Sources.UnknownProviderPublications = d.sources[2].diag.Published
	out.Sources.TrackingOverflows += d.sinkOverflows
	index, ok := m.named[name]
	if !ok {
		out.Reason = "sink_not_registered"
		return out
	}
	sink := d.sinks[index]
	if sink == nil {
		out.Reason = "sink_tracking_unavailable"
		return out
	}
	out.IncarnationID = sink.incarnation
	at := sink.registeredAt
	out.RegisteredAt = &at
	out.RegistrationSequence = sink.registrationSequence
	out.Delivery = sink.diag[p]
	return out
}
