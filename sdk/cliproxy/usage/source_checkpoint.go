package usage

import (
	"container/heap"
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxSourceAccounts = 1024
const maxSourceFlights = 8192
const maxSourceGaps = 16

type sourceAccount struct{ id, index string }
type sourceFailures struct {
	count     uint64
	gaps      []pluginapi.UsageDeliveryGap
	from      *time.Time
	unbounded bool
}
type sourceScopeState struct {
	physical         physicalScopeState
	scope            pluginapi.UsageSourceScope
	from             time.Time
	enrolledSequence uint64
	active, pending  deliveryHeap
	failures         sourceFailures
	sinks            map[string]*sourceFailures
}
type sourceProofState struct {
	physicalGaps uint64
	accounts     map[sourceAccount]*sourceScopeState
	unknown      map[sourceAccount]*sourceFailures
	global       sourceFailures
	lastClock    time.Time
	flights      int
}
type sourceFlight struct {
	physical                               physicalFlight
	state                                  *sourceScopeState
	active, pending                        *deliveryEntry
	start, dispatch                        time.Time
	claimed, published, finished, released bool
}
type sourceAdmissionKey struct{}

func sourceScope(id, index string) pluginapi.UsageSourceScope {
	return pluginapi.UsageSourceScope{Provider: "claude", AuthID: id, AuthIndex: index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
}
func validSourceScope(s pluginapi.UsageSourceScope) bool {
	return s == sourceScope(s.AuthID, s.AuthIndex) && s.AuthID != "" && s.AuthIndex != "" && len(s.AuthID) <= 512 && len(s.AuthIndex) <= 128
}
func (m *Manager) sourceProofLocked() *sourceProofState {
	d := m.deliveryLocked()
	if d.proof == nil {
		d.proof = &sourceProofState{accounts: make(map[sourceAccount]*sourceScopeState), unknown: make(map[sourceAccount]*sourceFailures)}
	}
	return d.proof
}
func timeCopy(v time.Time) *time.Time {
	if v.IsZero() {
		return nil
	}
	v = v.Round(0).UTC()
	return &v
}
func cloneGap(g pluginapi.UsageDeliveryGap) pluginapi.UsageDeliveryGap {
	if g.From != nil {
		g.From = timeCopy(*g.From)
	}
	if g.Through != nil {
		g.Through = timeCopy(*g.Through)
	}
	return g
}
func CloneSourceEvidence(e *pluginapi.UsageSourceEvidence) *pluginapi.UsageSourceEvidence {
	if e == nil {
		return nil
	}
	v := *e
	if e.Physical != nil {
		p := *e.Physical
		v.Physical = &p
	}
	if e.UpstreamStartedAt != nil {
		v.UpstreamStartedAt = timeCopy(*e.UpstreamStartedAt)
	}
	if e.Gap != nil {
		g := cloneGap(*e.Gap)
		v.Gap = &g
	}
	return &v
}
func (m *Manager) addSourceGapLocked(f *sourceFailures, reason string, from time.Time) pluginapi.UsageDeliveryGap {
	d := m.deliveryLocked()
	d.sequence++
	g := pluginapi.UsageDeliveryGap{Sequence: d.sequence, Reason: reason, From: timeCopy(from), ObservedAt: d.now().Round(0).UTC()}
	recordSourceGap(f, g)
	return g
}
func recordSourceGap(f *sourceFailures, g pluginapi.UsageDeliveryGap) {
	f.count++
	if g.From == nil {
		f.unbounded = true
	} else if f.from == nil || g.From.Before(*f.from) {
		f.from = timeCopy(*g.From)
	}
	if len(f.gaps) < maxSourceGaps {
		f.gaps = append(f.gaps, g)
	}
}
func (m *Manager) sourceClockLocked() time.Time {
	d := m.deliveryLocked()
	p := m.sourceProofLocked()
	// The RPC proof is expressed in serialized wall-clock event time. Comparing
	// Go monotonic components here would conceal a wall-clock rollback.
	now := d.now().Round(0).UTC()
	if !p.lastClock.IsZero() && now.Before(p.lastClock) {
		m.addSourceGapLocked(&p.global, "clock_regression", now)
	}
	if now.After(p.lastClock) {
		p.lastClock = now
	}
	return now
}
func accountMatches(a, b sourceAccount) bool {
	return (a.id == "" || a.id == b.id) && (a.index == "" || a.index == b.index)
}
func (m *Manager) sourceAccountLocked(id, index string, from time.Time) *sourceScopeState {
	p := m.sourceProofLocked()
	key := sourceAccount{id, index}
	if s := p.accounts[key]; s != nil {
		return s
	}
	if len(p.accounts) >= maxSourceAccounts {
		m.addSourceGapLocked(&p.global, "account_tracking_overflow", time.Time{})
		return nil
	}
	s := &sourceScopeState{scope: sourceScope(id, index), from: from, enrolledSequence: m.deliveryLocked().sequence, sinks: make(map[string]*sourceFailures)}
	p.accounts[key] = s
	for unknown, f := range p.unknown {
		if accountMatches(unknown, key) {
			s.failures.count += f.count
			s.failures.unbounded = s.failures.unbounded || f.unbounded
			if f.from != nil && (s.failures.from == nil || f.from.Before(*s.failures.from)) {
				s.failures.from = timeCopy(*f.from)
			}
			for _, g := range f.gaps {
				if len(s.failures.gaps) < maxSourceGaps {
					s.failures.gaps = append(s.failures.gaps, g)
				}
			}
		}
	}
	return s
}

// NoteSourceGap quarantines matching credential tuples before unsupported
// dispatch, or on untracked publication. Missing identity is a provider-wide gap.
func (m *Manager) NoteSourceGap(provider, id, index, reason string, from time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noteSourceGapLocked(provider, id, index, reason, from)
}
func (m *Manager) noteSourceGapLocked(provider, id, index, reason string, from time.Time) *pluginapi.UsageDeliveryGap {
	p := m.sourceProofLocked()
	if from.After(m.deliveryLocked().now().Round(0).UTC()) {
		from = time.Time{}
	}
	key := sourceAccount{id, index}
	if provider != "claude" {
		matched := false
		for k := range p.accounts {
			if (id != "" || index != "") && accountMatches(key, k) {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
	}
	if id == "" && index == "" {
		if provider != "claude" {
			return nil
		}
		g := m.addSourceGapLocked(&p.global, reason, from)
		return &g
	}
	if len(id) > 512 || len(index) > 128 {
		g := m.addSourceGapLocked(&p.global, "invalid_account_identity", time.Time{})
		return &g
	}
	f := p.unknown[key]
	if f == nil {
		if len(p.unknown) >= maxSourceAccounts {
			g := m.addSourceGapLocked(&p.global, "unknown_account_overflow", time.Time{})
			return &g
		}
		f = &sourceFailures{}
		p.unknown[key] = f
	}
	g := m.addSourceGapLocked(f, reason, from)
	for k, s := range p.accounts {
		if accountMatches(key, k) {
			recordSourceGap(&s.failures, g)
		}
	}
	return &g
}

// BeginClaudeSource is reserved for host built-in Claude admission/reporters.
// Context reuse claims an admission only once; retry attempts need fresh tickets.
func (m *Manager) BeginClaudeSource(ctx context.Context, id, index string, claim bool) (*DeliveryTicket, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deliveryLocked()
	d.sequence++
	now := m.sourceClockLocked()
	if ctx != nil {
		if t, ok := ctx.Value(sourceAdmissionKey{}).(*DeliveryTicket); ok && t != nil && t.manager == m && t.source != nil && t.source.state.scope == sourceScope(id, index) && !t.source.claimed && !t.finished {
			t.source.claimed = claim
			return t, t.source.start
		}
	}
	if !validSourceScope(sourceScope(id, index)) {
		m.noteSourceGapLocked("claude", id, index, "missing_account_identity", now)
		return nil, now
	}
	s := m.sourceAccountLocked(id, index, now)
	if s == nil {
		return nil, now
	}
	p := m.sourceProofLocked()
	if p.flights >= maxSourceFlights || m.closed {
		m.addSourceGapLocked(&s.failures, "source_tracking_overflow_or_closed", now)
		return nil, now
	}
	f := &sourceFlight{state: s, start: now, claimed: claim, active: &deliveryEntry{start: now}}
	f.physical.admission = d.sequence
	s.physical.outstanding++
	heap.Push(&s.active, f.active)
	p.flights++
	// The existing diagnostic reporter and scoped proof share one ticket/lock.
	t := &DeliveryTicket{manager: m, provider: 0, source: f}
	source := &d.sources[0]
	source.diag.Started++
	if source.reporters.Len() < maxDeliveryTracking {
		t.entry = &deliveryEntry{start: now}
		heap.Push(&source.reporters, t.entry)
		source.diag.OpenReporters++
	} else {
		source.diag.TrackingOverflows++
		// The scoped active/pending guards above remain fully tracked. An
		// unrelated legacy diagnostic guard must not poison this account's proof.
	}
	return t, now
}
func WithSourceAdmission(ctx context.Context, t *DeliveryTicket) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sourceAdmissionKey{}, t)
}
func (t *DeliveryTicket) ReleaseUnclaimed() {
	if t == nil || t.manager == nil {
		return
	}
	m := t.manager
	m.mu.Lock()
	if f := t.source; f != nil && !f.claimed && !f.finished {
		m.addSourceGapLocked(&f.state.failures, "admission_without_reporter", f.start)
		f.published = true
		// The explicit gap replaces the absent record for this abandoned
		// admission. Retire both guard sets without enqueuing phantom usage.
		t.published = true
	}
	claimed := t.source != nil && t.source.claimed
	m.mu.Unlock()
	if !claimed {
		t.Finish()
	}
}

// MarkSourceDispatch is called immediately before the real RoundTrip, without
// requiring cache policy. A redirect/repeated send is not one physical attempt.
func (t *DeliveryTicket) MarkSourceDispatch(firstParty bool) {
	if t == nil || t.manager == nil {
		return
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	f := t.source
	if f == nil {
		return
	}
	now := m.sourceClockLocked()
	m.deliveryLocked().sequence++
	if !firstParty {
		m.addSourceGapLocked(&f.state.failures, "unsupported_dispatch", f.start)
	}
	if !f.dispatch.IsZero() {
		m.addSourceGapLocked(&f.state.failures, "multiple_dispatch", f.start)
		return
	}
	f.dispatch = now
}

// FailSource marks an interval whose billed usage may be incomplete. This is
// deliberately sticky for the affected account/source incarnation.
func (t *DeliveryTicket) FailSource(reason string) {
	if t == nil || t.manager == nil {
		return
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if f := t.source; f != nil {
		m.addSourceGapLocked(&f.state.failures, reason, f.start)
	}
}

func (m *Manager) sourceFinishedLocked(t *DeliveryTicket) {
	f := t.source
	if f == nil || f.finished {
		return
	}
	f.finished = true
	m.physicalFinishedLocked(f)
	if f.published && f.active != nil {
		heap.Remove(&f.state.active, f.active.index)
		f.active = nil
		m.releaseSourceFlightLocked(f)
	}
}
func (m *Manager) sourcePublishLocked(r *Record, t *DeliveryTicket, final bool) *sourceFlight {
	r.SourceEvidence = nil
	var f *sourceFlight
	if t != nil && t.manager == m {
		f = t.source
	}
	if f == nil || !final || f.published || r.AuthID != f.state.scope.AuthID || r.AuthIndex != f.state.scope.AuthIndex || r.Provider != "claude" || !r.RequestedAt.Equal(f.start) {
		g := m.noteSourceGapLocked(r.Provider, r.AuthID, r.AuthIndex, "untracked_publication", r.RequestedAt)
		if g != nil {
			r.SourceEvidence = &pluginapi.UsageSourceEvidence{Scope: sourceScope(r.AuthID, r.AuthIndex), ManagerIncarnationID: m.deliveryLocked().incarnation, RequestedAt: r.RequestedAt, FinalUsageAt: m.deliveryLocked().now(), Gap: g}
		}
		return nil
	}
	now := m.sourceClockLocked()
	if r.Failed {
		m.addSourceGapLocked(&f.state.failures, "failed_source_attempt", f.start)
	}
	e := &pluginapi.UsageSourceEvidence{Scope: f.state.scope, ManagerIncarnationID: m.deliveryLocked().incarnation, Tracked: true, RequestedAt: f.start, UpstreamStartedAt: timeCopy(f.dispatch), FinalUsageAt: now}
	if f.physical.dispatch != nil {
		physical := *f.physical.dispatch
		e.Physical = &physical
	}
	r.SourceEvidence = e
	if len(f.state.failures.gaps) > 0 {
		g := cloneGap(f.state.failures.gaps[len(f.state.failures.gaps)-1])
		e.Gap = &g
	}
	if m.closed {
		g := m.addSourceGapLocked(&f.state.failures, "publish_rejected", f.start)
		e.Gap = &g
		return nil
	}
	if f.state.pending.Len() >= maxSourceFlights {
		g := m.addSourceGapLocked(&f.state.failures, "pending_tracking_overflow", f.start)
		e.Gap = &g
	} else {
		f.pending = &deliveryEntry{start: f.start}
		heap.Push(&f.state.pending, f.pending)
	}
	f.published = true
	m.physicalReleaseLocked(f)
	if f.finished && f.active != nil {
		heap.Remove(&f.state.active, f.active.index)
		f.active = nil
		m.releaseSourceFlightLocked(f)
	}
	return f
}
func (m *Manager) sourceDequeuedLocked(f *sourceFlight) {
	if f != nil && f.pending != nil {
		heap.Remove(&f.state.pending, f.pending.index)
		f.pending = nil
		m.releaseSourceFlightLocked(f)
	}
}
func (m *Manager) releaseSourceFlightLocked(f *sourceFlight) {
	if f != nil && !f.released && f.active == nil && f.pending == nil && f.physical.closed {
		f.released = true
		m.sourceProofLocked().flights--
	}
}
func (m *Manager) sourceFailureLocked(f *sourceFlight, index int, reason string, from time.Time) {
	if f == nil {
		return
	}
	if index < 0 {
		m.addSourceGapLocked(&f.state.failures, reason, from)
		return
	}
	sink := m.deliveryLocked().sinks[index]
	if sink == nil {
		return
	}
	// Failure histories are bounded by incarnation. Only the current sink's
	// history applies to its new From; consumer registration markers remain required.
	key := sink.incarnation
	if len(f.state.sinks) >= maxDeliverySinks {
		m.addSourceGapLocked(&f.state.failures, "sink_scope_tracking_overflow", from)
		return
	}
	gaps := f.state.sinks[key]
	if gaps == nil {
		gaps = &sourceFailures{}
		f.state.sinks[key] = gaps
	}
	m.addSourceGapLocked(gaps, reason, from)
}
func (m *Manager) sourceAcknowledgedLocked(f *sourceFlight, index int, incarnation string, from time.Time, err error) {
	if f == nil {
		return
	}
	sink := m.deliveryLocked().sinks[index]
	if sink == nil {
		return
	}
	reason := ""
	switch {
	case sink.incarnation != incarnation:
		reason = "stale_callback"
	case errors.Is(err, ErrDeliveryMissing):
		reason = "sink_missing"
	case errors.Is(err, ErrDeliveryFused):
		reason = "sink_fused"
	case errors.Is(err, ErrDeliveryUnacknowledged):
		reason = "unacknowledged_callback"
	case err != nil:
		reason = "callback_error"
	}
	if reason != "" {
		m.sourceFailureLocked(f, index, reason, from)
	}
}

func (m *Manager) SourceCheckpoint(name string, scope pluginapi.UsageSourceScope, expected ...Plugin) pluginapi.HostUsageCheckpointResponse {
	out := pluginapi.HostUsageCheckpointResponse{Scope: scope, Coverage: "unsupported", Reason: "unsupported_source", Gaps: []pluginapi.UsageDeliveryGap{}}
	out.Physical = unknownPhysical("unsupported_source")
	if m == nil || !validSourceScope(scope) {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.sourceClockLocked()
	d := m.deliveryLocked()
	// Issuance has an ordered sequence too: two idle proofs can have different
	// wall-clock bounds and must be distinguishable if RPC responses reorder.
	d.sequence++
	p := m.sourceProofLocked()
	out.ManagerIncarnationID = d.incarnation
	out.Sequence = d.sequence
	out.Coverage = "unknown"
	// Bind every early refusal to the physical extension too.
	defer func() {
		if out.Physical.Reason == "unsupported_source" {
			out.Physical.Reason = out.Reason
		}
	}()
	i, ok := m.named[name]
	if !ok {
		out.Reason = "sink_not_registered"
		return out
	}
	sink := d.sinks[i]
	if sink == nil {
		out.Reason = "sink_tracking_unavailable"
		return out
	}
	if len(expected) > 0 {
		if expected[0] == nil || i >= len(m.plugins) || !reflect.ValueOf(expected[0]).Comparable() || !reflect.ValueOf(m.plugins[i]).Comparable() || m.plugins[i] != expected[0] {
			out.Reason = "sink_registration_changed"
			return out
		}
	}
	out.IncarnationID = sink.incarnation
	s := p.accounts[sourceAccount{scope.AuthID, scope.AuthIndex}]
	if s == nil {
		out.Reason = "source_not_enrolled"
		return out
	}
	from := s.from
	registrationFrom := sink.registeredAt
	if s.enrolledSequence < sink.registrationSequence {
		// Exclude pre-registration events even when a coarse/fake wall clock
		// assigns them the exact same timestamp as the new registration.
		registrationFrom = registrationFrom.Add(time.Nanosecond)
	}
	if registrationFrom.After(from) {
		from = registrationFrom
	}
	out.From = timeCopy(from)
	out.Active = uint64(s.active.Len())
	out.Pending = uint64(s.pending.Len())
	unbounded := false
	for _, f := range []*sourceFailures{&p.global, &s.failures, s.sinks[sink.incarnation]} {
		if f == nil {
			continue
		}
		out.GapCount += f.count
		unbounded = unbounded || f.unbounded
		if f.from != nil && (out.InvalidFrom == nil || f.from.Before(*out.InvalidFrom)) {
			out.InvalidFrom = timeCopy(*f.from)
		}
		for _, g := range f.gaps {
			if len(out.Gaps) < maxSourceGaps {
				out.Gaps = append(out.Gaps, cloneGap(g))
			}
		}
	}
	out.GapsTruncated = out.GapCount > uint64(len(out.Gaps))
	if unbounded {
		out.InvalidFrom = nil
	}
	out.Physical = m.physicalCheckpointLocked(s, i, sink, out, now)
	if out.GapCount > 0 {
		out.Reason = "delivery_gap"
		return out
	}
	if m.closed {
		out.Reason = "manager_closed"
		return out
	}
	through := now
	for _, h := range []deliveryHeap{s.active, s.pending} {
		if at := h.earliest(); at != nil && at.Before(through) {
			through = *at
		}
	}
	if through.Before(from) {
		out.Reason = "pre_window_outstanding"
		return out
	}
	out.Coverage = "known"
	out.Reason = ""
	out.Through = timeCopy(through)
	return out
}

// NamedPlugin captures a registration identity for a subsequent pinned snapshot.
func (m *Manager) NamedPlugin(name string) Plugin {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.named[name]
	if !ok || i >= len(m.plugins) {
		return nil
	}
	return m.plugins[i]
}
