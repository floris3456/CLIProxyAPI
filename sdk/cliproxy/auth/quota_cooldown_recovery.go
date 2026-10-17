package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Provenance is intentionally process-local: generic/restored "quota" flags
// also represent RPM throttles and cannot authorize an early recovery.
type quotaRecoveryProof struct {
	Fingerprint       string
	RegistrationEpoch uint64
	Basis             string
	Windows           []string
	OriginScope       string
	FailedAt          time.Time
}

type quotaRecoveryState struct {
	Status      Status
	Message     string
	Unavailable bool
	RetryAt     time.Time
	Quota       QuotaState
	Error       *Error
	UpdatedAt   time.Time
}

func quotaRecoveryStates(a *Auth) map[string]quotaRecoveryState {
	if a == nil || (a.Provider != "claude" && a.Provider != "codex") {
		return nil
	}
	states := map[string]quotaRecoveryState{"": {a.Status, a.StatusMessage, a.Unavailable, a.NextRetryAfter, cooldownFieldsOf(a.Quota), cloneError(a.LastError), a.UpdatedAt}}
	for model, state := range a.ModelStates {
		if state != nil {
			states[model] = quotaRecoveryState{state.Status, state.StatusMessage, state.Unavailable, state.NextRetryAfter, cooldownFieldsOf(state.Quota), cloneError(state.LastError), state.UpdatedAt}
		}
	}
	return states
}

func (s quotaRecoveryState) fingerprint() string {
	// Observation-only updates and auth aggregate timestamps do not change the
	// original failure classification. CAS separately includes auth.Generation.
	s.UpdatedAt = time.Time{}
	return quotaRecoveryDigest(s)
}

func quotaRecoveryDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s quotaRecoveryState) blocked(now time.Time) bool {
	blocked, _, _ := availabilityBlock(s.Unavailable, s.Quota.Exceeded, s.RetryAt, s.Quota.NextRecoverAt, now)
	return blocked || s.Status == StatusDisabled
}

// Called only under m.mu, after an actual result mutation. The prior restriction
// must be absent or proven to have exactly the same quota scope; mixed backoff
// cannot be disentangled safely and never becomes clearable.
func (m *Manager) trackQuotaRecoveryLocked(a *Auth, model string, result Result, headers http.Header, before map[string]quotaRecoveryState, now time.Time) {
	if before == nil {
		return
	}
	if m.quotaRecoveryProofs == nil {
		m.quotaRecoveryProofs = make(map[string]map[string]quotaRecoveryProof)
	}
	previous := m.quotaRecoveryProofs[a.ID]
	current := quotaRecoveryStates(a)
	proofs := make(map[string]quotaRecoveryProof)
	for key, proof := range previous {
		if state, ok := current[key]; ok && proof.RegistrationEpoch == a.RegistrationEpoch && proof.Fingerprint == state.fingerprint() {
			proofs[key] = proof
		}
	}
	if !result.Success && statusCodeFromResult(result.Error) == http.StatusTooManyRequests {
		basis, windows := quotaFailureClassification(a.Provider, result.Error, headers)
		origin := "model"
		if result.CredentialScope || model == "" {
			origin = "credential"
		}
		for key, state := range current {
			if key != model && !(result.CredentialScope && model != "") {
				continue
			}
			delete(proofs, key) // Even an identical later RPM 429 invalidates proof.
			if key != "" && len(before) == 1 && before[""].blocked(now) {
				continue
			}
			if basis == "" || result.Provider != a.Provider || !state.Quota.Exceeded || (state.Quota.Reason != "quota" && state.Quota.Reason != "credential_quota") || !state.RetryAt.Equal(state.Quota.NextRecoverAt) {
				continue
			}
			if key == "" && len(a.ModelStates) > 0 && state.Quota.Reason != "credential_quota" {
				continue
			}
			if old, ok := before[key]; ok && old.blocked(now) {
				proof, known := previous[key]
				if !known || proof.RegistrationEpoch != a.RegistrationEpoch || proof.Fingerprint != old.fingerprint() || proof.Basis != basis || proof.OriginScope != origin || strings.Join(proof.Windows, ",") != strings.Join(windows, ",") {
					continue
				}
			}
			proofs[key] = quotaRecoveryProof{state.fingerprint(), a.RegistrationEpoch, basis, append([]string(nil), windows...), origin, now}
		}
	}
	m.quotaRecoveryProofs[a.ID] = proofs
}

func quotaFailureClassification(provider string, failure *Error, headers http.Header) (string, []string) {
	if failure == nil || failure.HTTPStatus != 429 || failure.Code == ErrorCodeForceCooldown || shouldSkipCredentialCooldown(failure) {
		return "", nil
	}
	if provider == "codex" {
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
			Response struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(failure.Message), &body) == nil && (body.Type == "usage_limit_reached" || body.Error.Type == "usage_limit_reached" || body.Response.Error.Type == "usage_limit_reached") {
			return "codex_usage_limit", []string{"codex:applicable"}
		}
		return "", nil
	}
	if provider != "claude" {
		return "", nil
	}
	// Simultaneous short-window exhaustion is mixed quota/RPM/TPM, not a safe clear.
	for _, limit := range []string{"requests", "tokens", "input-tokens", "output-tokens", "priority-tokens"} {
		if raw := quotaRecoveryHeader(headers, "anthropic-ratelimit-"+limit+"-remaining"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 {
				return "", nil
			}
		}
	}
	var windows []string
	for _, window := range []string{"5h", "7d", "7d-sonnet", "7d-opus", "7d_oi", "overage"} {
		if quotaRecoveryHeader(headers, "anthropic-ratelimit-unified-"+window+"-status") == "rejected" {
			windows = append(windows, "claude:"+window)
		}
	}
	if len(windows) == 0 {
		return "", nil
	} // Aggregate rejection cannot identify applicable budget.
	return "claude_unified_window", windows
}

func quotaRecoveryHeader(h http.Header, name string) string {
	var found string
	for key, values := range h {
		if strings.EqualFold(key, name) {
			if len(values) != 1 || found != "" {
				return "ambiguous"
			}
			found = strings.TrimSpace(values[0])
		}
	}
	return found
}

func (m *Manager) ReadQuotaCooldowns(authID string) pluginapi.HostQuotaCooldownReadResponse {
	return m.readQuotaCooldownsAt(authID, time.Now())
}

func (m *Manager) readQuotaCooldownsAt(authID string, now time.Time) pluginapi.HostQuotaCooldownReadResponse {
	out := pluginapi.HostQuotaCooldownReadResponse{Cooldowns: []pluginapi.HostQuotaCooldown{}}
	if m == nil || m.HomeEnabled() {
		out.Reason = "unsupported_runtime"
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.auths[authID]
	if a == nil {
		out.Reason = "not_found"
		return out
	}
	out.AuthIndex, out.Provider = a.Index, a.Provider
	states := quotaRecoveryStates(a)
	if states == nil {
		out.Reason = "unsupported_provider"
		return out
	}
	out.Supported = true
	keys := make([]string, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// Auth aggregates are not a separate credential-wide restriction.
		if key == "" && len(a.ModelStates) > 0 && a.Quota.Reason != "credential_quota" {
			continue
		}
		state := states[key]
		if !state.blocked(now) {
			continue
		}
		out.Cooldowns = append(out.Cooldowns, m.quotaCooldownViewLocked(a, key, state, now))
	}
	return out
}

func (m *Manager) quotaCooldownViewLocked(a *Auth, key string, state quotaRecoveryState, now time.Time) pluginapi.HostQuotaCooldown {
	if m.quotaRecoveryInstance == "" {
		m.quotaRecoveryInstance = uuid.NewString()
	}
	view := pluginapi.HostQuotaCooldown{Scope: "model", ModelKey: key, Classification: "unclear", NextRetryAt: state.RetryAt, UpdatedAt: state.UpdatedAt, RefusalReason: "unproven_quota"}
	if key == "" {
		view.Scope = "credential"
	}
	if state.Quota.NextRecoverAt.After(view.NextRetryAt) {
		view.NextRetryAt = state.Quota.NextRecoverAt
	}
	if state.Error != nil {
		view.HTTPStatus = state.Error.HTTPStatus
	}
	proof, known := m.quotaRecoveryProofs[a.ID][key]
	known = known && proof.RegistrationEpoch == a.RegistrationEpoch && proof.Fingerprint == state.fingerprint()
	if known {
		view.Classification, view.Basis, view.OriginScope = "quota", proof.Basis, proof.OriginScope
		view.RequiredWindows = append([]string(nil), proof.Windows...)
		view.FailureObservedAt = proof.FailedAt
		view.Clearable = true
		view.RefusalReason = ""
	}
	if state.Error != nil && state.Error.HTTPStatus != 429 {
		view.Classification = "non_quota"
		view.Clearable = false
		view.RefusalReason = "non_quota_error"
	}
	if a.Disabled || a.Status == StatusDisabled || state.Status == StatusDisabled || hasUnauthorizedAuthFailure(a) {
		view.Clearable = false
		view.RefusalReason = "credential_or_model_disabled"
	}
	// Auth-level failures can coexist with model quota. Their provenance is not
	// separable from aggregates, so do not recompute availability over them.
	if a.LastError != nil && a.LastError.HTTPStatus != 429 {
		view.Clearable = false
		view.RefusalReason = "credential_non_quota_error"
	}
	if m.cooldownDisabledForAuth(a) {
		view.Clearable = false
		view.RefusalReason = "cooldown_disabled"
	}
	if !state.blocked(now) {
		view.Clearable = false
		view.RefusalReason = "not_blocked"
	}
	if key != "" && key != canonicalModelKey(key) {
		view.Clearable = false
		view.RefusalReason = "noncanonical_model_key"
	}
	view.Revision = quotaRecoveryDigest(struct {
		Instance, ID, Index, Provider, Key string
		Epoch, Generation                  uint64
		Disabled                           bool
		Status                             Status
		AuthError                          *Error
		State                              quotaRecoveryState
		Proof                              quotaRecoveryProof
	}{m.quotaRecoveryInstance, a.ID, a.Index, a.Provider, key, a.RegistrationEpoch, a.Generation, a.Disabled, a.Status, a.LastError, state, proof})
	return view
}

// ClearQuotaCooldown compares the exact observed runtime version under the same
// lock as MarkResult. It neither calls ResetQuota nor writes token files.
func (m *Manager) ClearQuotaCooldown(ctx context.Context, authID string, req pluginapi.HostQuotaCooldownClearRequest) pluginapi.HostQuotaCooldownClearResponse {
	return m.clearQuotaCooldownAt(ctx, authID, req, time.Now())
}

func (m *Manager) clearQuotaCooldownAt(ctx context.Context, authID string, req pluginapi.HostQuotaCooldownClearRequest, now time.Time) pluginapi.HostQuotaCooldownClearResponse {
	out := pluginapi.HostQuotaCooldownClearResponse{AuthIndex: req.AuthIndex, Scope: req.Scope, ModelKey: req.ModelKey, Result: "unsupported"}
	if m == nil || m.HomeEnabled() {
		return out
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return out
	}
	if (req.Scope != "model" && req.Scope != "credential") || (req.Scope == "model" && req.ModelKey == "") || (req.Scope == "credential" && req.ModelKey != "") {
		return out
	}
	models, epoch := registry.GetGlobalRegistry().GetModelsAndEpochForClient(authID)
	m.mu.Lock()
	a := m.auths[authID]
	if a == nil || a.Index != req.AuthIndex {
		m.mu.Unlock()
		out.Result = "not_found"
		return out
	}
	states := quotaRecoveryStates(a)
	if states == nil {
		m.mu.Unlock()
		return out
	}
	state, exists := states[req.ModelKey]
	if !exists || (req.Scope == "credential" && len(a.ModelStates) > 0 && a.Quota.Reason != "credential_quota") {
		m.mu.Unlock()
		out.Result = "not_found"
		return out
	}
	current := m.quotaCooldownViewLocked(a, req.ModelKey, state, now)
	out.Current = &current
	if req.Revision == "" || req.Revision != current.Revision {
		m.mu.Unlock()
		out.Result = "stale"
		return out
	}
	if !current.Clearable {
		m.mu.Unlock()
		out.Result = "not_clearable"
		return out
	}
	e := req.Evidence
	if e.Reason != "applicable_new_cycle" || (e.Source != "inference_headers" && e.Source != "provider_event" && e.Source != "quota_endpoint") || strings.TrimSpace(e.Epoch) == "" || len(e.Epoch) > 256 || !e.AllApplicableWindowsPositive || !e.ObservedAt.After(current.FailureObservedAt) || e.ObservedAt.After(now) {
		m.mu.Unlock()
		out.Result = "invalid_evidence"
		return out
	}
	if req.Scope == "credential" {
		a.Unavailable = false
		a.NextRetryAfter = time.Time{}
		applyCooldownFields(&a.Quota, QuotaState{})
		a.LastError = nil
		a.StatusMessage = ""
		a.Status = StatusActive
	} else {
		s := a.ModelStates[req.ModelKey]
		s.Unavailable = false
		s.NextRetryAfter = time.Time{}
		applyCooldownFields(&s.Quota, QuotaState{})
		s.LastError = nil
		s.StatusMessage = ""
		s.Status = StatusActive
		s.UpdatedAt = now
	}
	delete(m.quotaRecoveryProofs[a.ID], req.ModelKey)
	// Recompute only aggregate fields on a detached copy: the normal helper may
	// expire sibling model timers, which this scoped callback must not mutate.
	if len(a.ModelStates) > 0 && a.Quota.Reason != "credential_quota" {
		shadow := a.Clone()
		applyCooldownFields(&shadow.Quota, QuotaState{})
		updateAggregatedAvailability(shadow, now)
		a.Unavailable = shadow.Unavailable
		a.NextRetryAfter = shadow.NextRetryAfter
		applyCooldownFields(&a.Quota, shadow.Quota)
	}
	a.Generation++
	a.UpdatedAt = now
	snapshot := a.Clone()
	m.mu.Unlock()
	// Existing generation-aware projections reject an older clear racing a new 429.
	if m.scheduler != nil {
		m.scheduler.upsertAuthResult(snapshot, []string{req.ModelKey}, req.Scope == "credential")
	}
	projections := make([]registry.ClientModelProjection, 0, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			projections = append(projections, m.clientModelProjectionForAuth(snapshot, model.ID, now))
		}
	}
	registry.GetGlobalRegistry().ApplyClientModelProjections(authID, epoch, snapshot.Generation, projections)
	m.persistCooldownStates(context.WithoutCancel(ctx))
	out.Cleared = true
	out.Result = "cleared"
	out.Current = nil
	return out
}
