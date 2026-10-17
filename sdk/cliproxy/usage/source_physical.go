package usage

import (
	"reflect"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type physicalScopeState struct {
	outstanding       uint64
	gaps              uint64
	lastProducerClose time.Time
	cuts              map[int]physicalSinkCut
}
type physicalSinkCut struct {
	incarnation string
	cut         pluginapi.UsagePhysicalCut
}
type physicalFlight struct {
	admission                                       uint64
	dispatch                                        *pluginapi.UsagePhysicalDispatch
	returned, requestClosed, responseClosed, closed bool
}

// SourceDispatch is a host-private lifetime handle. No public record, callback
// watermark or configuration can create a cut. These methods are reserved for
// the built-in transport wrapper; unsupported wrappers are explicitly tainted.
type SourceDispatch struct{ ticket *DeliveryTicket }

func (t *DeliveryTicket) BeginPhysicalDispatch(scope pluginapi.UsageSourceScope, permit any, requestBody bool) *SourceDispatch {
	// Unexported concrete runtime permit, not a caller-controlled bool. The
	// internal helper creates it only at its validated transport boundary.
	pt := reflect.TypeOf(permit)
	qualified := pt != nil && pt.Kind() == reflect.Struct && pt.PkgPath() == "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps" && pt.Name() == "physicalDispatchPermit"
	return t.beginPhysicalDispatch(scope, qualified, requestBody)
}

func (t *DeliveryTicket) beginPhysicalDispatch(scope pluginapi.UsageSourceScope, qualified, requestBody bool) *SourceDispatch {
	if t == nil || t.manager == nil {
		return nil
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	f := t.source
	if f == nil {
		return nil
	}
	if !qualified || scope != f.state.scope {
		f.state.physical.gaps++
	}
	if f.finished || f.physical.closed || f.physical.dispatch != nil {
		f.state.physical.gaps++
		return nil
	}
	now := m.sourceClockLocked()
	d := m.deliveryLocked()
	d.sequence++
	f.physical.dispatch = &pluginapi.UsagePhysicalDispatch{Version: pluginapi.ClaudePhysicalProofVersion, AdmissionSequence: f.physical.admission, DispatchSequence: d.sequence, StartedAt: now}
	f.physical.requestClosed = !requestBody
	return &SourceDispatch{ticket: t}
}

// Negative evidence only. An actual credential different from the frozen
// selection cannot be assigned to some other tuple by guessing.
func (m *Manager) NotePhysicalIdentityGap() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sourceProofLocked().physicalGaps++
	m.deliveryLocked().sequence++
}

func (g *SourceDispatch) change(fn func(*sourceFlight)) {
	if g == nil || g.ticket == nil {
		return
	}
	m := g.ticket.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	f := g.ticket.source
	m.sourceClockLocked()
	m.deliveryLocked().sequence++
	fn(f)
	m.physicalReleaseLocked(f)
}

// RoundTripReturned follows the actual transport return; response body closure
// is separately required. The request body may close asynchronously afterward.
func (g *SourceDispatch) RoundTripReturned(responseBody bool, failed bool) {
	g.change(func(f *sourceFlight) {
		f.physical.returned = true
		f.physical.responseClosed = !responseBody
		if failed {
			f.state.physical.gaps++
		}
	})
}
func (g *SourceDispatch) RequestBodyClosed(failed bool) {
	g.change(func(f *sourceFlight) {
		f.physical.requestClosed = true
		if failed {
			f.state.physical.gaps++
		}
	})
}
func (g *SourceDispatch) ResponseBodyClosed(failed bool) {
	g.change(func(f *sourceFlight) {
		f.physical.responseClosed = true
		if failed {
			f.state.physical.gaps++
		}
	})
}

// A transport-internal rewind can replace the instrumented request body with a
// new lifetime. Do not infer that it sent zero billable bytes or hide that send
// behind one physical observation; the entire physical scope becomes unknown.
func (g *SourceDispatch) RequestBodyReplayed() {
	g.change(func(f *sourceFlight) { f.state.physical.gaps++ })
}

func (m *Manager) physicalFinishedLocked(f *sourceFlight) {
	f.state.physical.lastProducerClose = m.sourceClockLocked()
	if f.physical.dispatch == nil {
		f.state.physical.gaps++
	}
	m.physicalReleaseLocked(f)
}
func (m *Manager) physicalReleaseLocked(f *sourceFlight) {
	if f.physical.closed || !f.finished || !f.published {
		return
	}
	p := &f.physical
	if p.dispatch != nil && (!p.returned || !p.requestClosed || !p.responseClosed) {
		return
	}
	p.closed = true
	f.state.physical.outstanding--
	m.releaseSourceFlightLocked(f)
}

func unknownPhysical(reason string) *pluginapi.UsagePhysicalCheckpoint {
	return &pluginapi.UsagePhysicalCheckpoint{Version: pluginapi.ClaudePhysicalProofVersion, Domain: pluginapi.ClaudePhysicalProofDomain, Coverage: "unknown", Reason: reason}
}

// The caller holds the admission/dispatch/publication/closure/ack lock. These
// cuts are nonblocking and bounded; an absent creation authority cannot be
// replaced by elapsed time, queue emptiness or a manually supplied watermark.
func (m *Manager) physicalCheckpointLocked(s *sourceScopeState, index int, sink *deliverySink, out pluginapi.HostUsageCheckpointResponse, now time.Time) *pluginapi.UsagePhysicalCheckpoint {
	p := unknownPhysical("admission_authority_unavailable")
	p.Outstanding = s.physical.outstanding
	p.Pending = uint64(s.pending.Len())
	p.GapCount = out.GapCount + s.physical.gaps + m.sourceProofLocked().physicalGaps
	p.LastProducerClosedAt = timeCopy(s.physical.lastProducerClose)
	if !m.physicalAuthority {
		return p
	}
	if m.closed {
		p.Reason = "manager_closed"
		return p
	}
	if p.GapCount > 0 {
		p.Reason = "physical_incarnation_gap"
		return p
	}
	if p.Outstanding > 0 {
		p.Reason = "outstanding_admission_or_producer"
		return p
	}
	if p.Pending > 0 {
		p.Reason = "pending_delivery"
		return p
	}
	if s.physical.cuts == nil {
		s.physical.cuts = make(map[int]physicalSinkCut)
	}
	cut := pluginapi.UsagePhysicalCut{Sequence: out.Sequence, At: now}
	old, ok := s.physical.cuts[index]
	if !ok || old.incarnation != sink.incarnation {
		s.physical.cuts[index] = physicalSinkCut{incarnation: sink.incarnation, cut: cut}
		p.FromCut = &cut
		p.Reason = "first_quiescent_cut"
		return p
	}
	from := old.cut
	p.FromCut = &from
	if !now.After(from.At) {
		p.Reason = "nonpositive_cut_interval"
		return p
	}
	p.Coverage = "known"
	p.Reason = ""
	p.ThroughCut = &cut
	return p
}
