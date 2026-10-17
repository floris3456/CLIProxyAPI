package auth

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var quotaRecoveryTestTime = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

func newQuotaRecoveryManager(t *testing.T, provider string) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	a, err := m.Register(context.Background(), &Auth{ID: t.Name(), Provider: provider, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, provider, []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}, {ID: "model-c"}})
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	return m, a
}

func markQuotaRecoveryResult(m *Manager, a *Auth, model string, code int, body string, credential bool, headers http.Header) {
	ctx := logging.WithFreshResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, headers)
	retry := time.Hour
	m.markResultWithClock(ctx, Result{AuthID: a.ID, Provider: a.Provider, Model: model, CredentialScope: credential, RetryAfter: &retry, Error: &Error{HTTPStatus: code, Message: body}}, func() time.Time { return quotaRecoveryTestTime })
}

func quotaRecoveryRow(t *testing.T, m *Manager, a *Auth, scope, model string) pluginapi.HostQuotaCooldown {
	t.Helper()
	read := m.readQuotaCooldownsAt(a.ID, quotaRecoveryTestTime.Add(time.Second))
	for _, row := range read.Cooldowns {
		if row.Scope == scope && row.ModelKey == model {
			return row
		}
	}
	t.Fatalf("missing %s/%s row: %+v", scope, model, read)
	return pluginapi.HostQuotaCooldown{}
}

func quotaRecoveryRequest(a *Auth, row pluginapi.HostQuotaCooldown) pluginapi.HostQuotaCooldownClearRequest {
	return pluginapi.HostQuotaCooldownClearRequest{AuthIndex: a.Index, Scope: row.Scope, ModelKey: row.ModelKey, Revision: row.Revision, Evidence: pluginapi.HostQuotaRecoveryEvidence{Reason: "applicable_new_cycle", Source: "inference_headers", Epoch: "dummy-new-cycle", ObservedAt: quotaRecoveryTestTime.Add(time.Second), AllApplicableWindowsPositive: true}}
}

func TestPacerQuotaRecoveryScopedCASAndRouting(t *testing.T) {
	m, a := newQuotaRecoveryManager(t, "codex")
	markQuotaRecoveryResult(m, a, "model-b", 403, `{"error":{"type":"permission_denied"}}`, false, nil)
	markQuotaRecoveryResult(m, a, "model-c", 503, `{"error":{"type":"server_error"}}`, false, nil)
	markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"usage_limit_reached"}}`, false, nil)
	row := quotaRecoveryRow(t, m, a, "model", "model-a")
	if !row.Clearable || row.Classification != "quota" || row.Basis != "codex_usage_limit" {
		t.Fatalf("classification=%+v", row)
	}
	before, _ := m.GetByID(a.ID)
	if _, err := m.scheduler.pickSingle(context.Background(), "codex", "model-a", executor.Options{}, nil); err == nil {
		t.Fatal("scheduler admitted cooling model")
	}
	out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second))
	if !out.Cleared || out.Result != "cleared" {
		t.Fatalf("clear=%+v", out)
	}
	after, _ := m.GetByID(a.ID)
	for _, key := range []string{"model-b", "model-c"} {
		if !reflect.DeepEqual(before.ModelStates[key], after.ModelStates[key]) {
			t.Fatalf("changed unrelated %s", key)
		}
	}
	if blocked, _, _ := isAuthBlockedForModel(after, "model-a", quotaRecoveryTestTime); blocked {
		t.Fatal("target still blocked")
	}
	if got, err := m.scheduler.pickSingle(context.Background(), "codex", "model-a", executor.Options{}, nil); err != nil || got == nil {
		t.Fatalf("recovery not reflected in scheduler: %v", err)
	}
	if again := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second)); again.Result != "stale" {
		t.Fatalf("replayed revision=%+v", again)
	}
}

func TestPacerQuotaRecoveryNew429InvalidatesIdenticalDeadline(t *testing.T) {
	m, a := newQuotaRecoveryManager(t, "codex")
	mark := func() {
		markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"usage_limit_reached"}}`, false, nil)
	}
	mark()
	old := quotaRecoveryRow(t, m, a, "model", "model-a")
	mark()
	fresh := quotaRecoveryRow(t, m, a, "model", "model-a")
	if !old.NextRetryAt.Equal(fresh.NextRetryAt) || !old.UpdatedAt.Equal(fresh.UpdatedAt) {
		t.Fatal("fixture did not hold clock and deadline identical")
	}
	if old.Revision == fresh.Revision {
		t.Fatal("new 429 reused opaque version")
	}
	out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, old), quotaRecoveryTestTime.Add(2*time.Second))
	if out.Result != "stale" || out.Cleared {
		t.Fatalf("stale clear=%+v", out)
	}
	if !quotaRecoveryRow(t, m, a, "model", "model-a").Clearable {
		t.Fatal("refusal damaged latest classified cooldown")
	}
}

func TestPacerQuotaRecoveryConcurrent429AlwaysWins(t *testing.T) {
	for i := 0; i < 24; i++ {
		m, a := newQuotaRecoveryManager(t, "codex")
		mark := func() {
			markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"usage_limit_reached"}}`, false, nil)
		}
		mark()
		req := quotaRecoveryRequest(a, quotaRecoveryRow(t, m, a, "model", "model-a"))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; mark() }()
		go func() {
			defer wg.Done()
			<-start
			m.clearQuotaCooldownAt(context.Background(), a.ID, req, quotaRecoveryTestTime.Add(2*time.Second))
		}()
		close(start)
		wg.Wait()
		current, _ := m.GetByID(a.ID)
		if blocked, _, _ := isAuthBlockedForModel(current, "model-a", quotaRecoveryTestTime.Add(3*time.Second)); !blocked {
			t.Fatal("concurrent clear erased newer 429")
		}
		if _, err := m.scheduler.pickSingle(context.Background(), "codex", "model-a", executor.Options{}, nil); err == nil {
			t.Fatal("stale clear projection erased newer scheduler cooldown")
		}
	}
}

func TestPacerQuotaRecoveryRefusesUnclassifiedAndMixed(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body string
		status               int
		headers              http.Header
	}{
		{"rpm", "codex", `{"error":{"type":"rate_limit_exceeded"}}`, 429, nil},
		{"403", "codex", `{"error":{"type":"permission_denied"}}`, 403, nil},
		{"5xx", "codex", `{"error":{"type":"server_error"}}`, 503, nil},
		{"transport", "codex", "connection reset", 0, nil},
		{"claude aggregate ambiguous", "claude", `{"error":{"type":"rate_limit_error"}}`, 429, http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}},
		{"claude mixed rpm", "claude", `{"error":{"type":"rate_limit_error"}}`, 429, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Requests-Remaining": {"0"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a := newQuotaRecoveryManager(t, tc.provider)
			markQuotaRecoveryResult(m, a, "model-a", tc.status, tc.body, false, tc.headers)
			if tc.status == 0 && len(m.readQuotaCooldownsAt(a.ID, quotaRecoveryTestTime).Cooldowns) == 0 {
				return
			} // Transport lifecycle errors may have no credential cooldown at all.
			row := quotaRecoveryRow(t, m, a, "model", "model-a")
			if row.Clearable {
				t.Fatalf("unsafe classification=%+v", row)
			}
			before, _ := m.GetByID(a.ID)
			out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second))
			if out.Result != "not_clearable" || out.Cleared {
				t.Fatalf("unsafe clear=%+v", out)
			}
			after, _ := m.GetByID(a.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refusal mutated credential")
			}
		})
	}
	for _, code := range []int{403, 503, 429} {
		m, a := newQuotaRecoveryManager(t, "codex")
		markQuotaRecoveryResult(m, a, "model-a", code, `{"error":{"type":"rate_limit_exceeded"}}`, false, nil)
		markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"usage_limit_reached"}}`, false, nil)
		if quotaRecoveryRow(t, m, a, "model", "model-a").Clearable {
			t.Fatalf("quota overwrote prior nonquota layer %d", code)
		}
	}
}

func TestPacerQuotaRecoveryCredentialAndPropagatedScopes(t *testing.T) {
	m, a := newQuotaRecoveryManager(t, "claude")
	// A sibling exists before credential-wide propagation.
	m.mu.Lock()
	m.auths[a.ID].ModelStates = map[string]*ModelState{"model-b": {Status: StatusActive}}
	m.mu.Unlock()
	headers := http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-7d-Status": {"allowed"}}
	markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"rate_limit_error"}}`, true, headers)
	row := quotaRecoveryRow(t, m, a, "credential", "")
	if !row.Clearable || row.Basis != "claude_unified_window" || !reflect.DeepEqual(row.RequiredWindows, []string{"claude:5h"}) {
		t.Fatalf("credential proof=%+v", row)
	}
	before, _ := m.GetByID(a.ID)
	if out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second)); !out.Cleared {
		t.Fatalf("clear=%+v", out)
	}
	after, _ := m.GetByID(a.ID)
	if !reflect.DeepEqual(before.ModelStates, after.ModelStates) {
		t.Fatal("credential clear changed models")
	}
	for _, key := range []string{"model-a", "model-b"} {
		row = quotaRecoveryRow(t, m, a, "model", key)
		if row.OriginScope != "credential" || !row.Clearable {
			t.Fatalf("propagated proof=%+v", row)
		}
		if out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second)); !out.Cleared {
			t.Fatalf("clear=%+v", out)
		}
	}
	after, _ = m.GetByID(a.ID)
	if blocked, _, _ := isAuthBlockedForModel(after, "model-a", quotaRecoveryTestTime); blocked {
		t.Fatal("explicitly recovered scopes remain blocked")
	}
}

func TestPacerQuotaRecoveryEvidenceDisabledReplacementAndRestore(t *testing.T) {
	m, a := newQuotaRecoveryManager(t, "codex")
	markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"usage_limit_reached"}}`, false, nil)
	row := quotaRecoveryRow(t, m, a, "model", "model-a")
	for _, mutate := range []func(*pluginapi.HostQuotaRecoveryEvidence){
		func(e *pluginapi.HostQuotaRecoveryEvidence) { e.AllApplicableWindowsPositive = false },
		func(e *pluginapi.HostQuotaRecoveryEvidence) { e.Source = "analytics_success" },
		func(e *pluginapi.HostQuotaRecoveryEvidence) { e.Reason = "timer_elapsed" },
		func(e *pluginapi.HostQuotaRecoveryEvidence) { e.Epoch = "" },
		func(e *pluginapi.HostQuotaRecoveryEvidence) { e.ObservedAt = quotaRecoveryTestTime.Add(-time.Second) },
		func(e *pluginapi.HostQuotaRecoveryEvidence) { e.ObservedAt = quotaRecoveryTestTime.Add(time.Hour) },
	} {
		req := quotaRecoveryRequest(a, row)
		mutate(&req.Evidence)
		if out := m.clearQuotaCooldownAt(context.Background(), a.ID, req, quotaRecoveryTestTime.Add(2*time.Second)); out.Result != "invalid_evidence" {
			t.Fatalf("evidence=%+v", out)
		}
	}
	m.mu.Lock()
	m.auths[a.ID].Disabled = true
	m.mu.Unlock()
	disabled := quotaRecoveryRow(t, m, a, "model", "model-a")
	if disabled.Clearable {
		t.Fatal("disabled credential is clearable")
	}
	if out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, disabled), quotaRecoveryTestTime.Add(2*time.Second)); out.Result != "not_clearable" {
		t.Fatalf("disabled clear=%+v", out)
	}
	snapshot, _ := m.GetByID(a.ID)
	snapshot.Disabled = false
	if _, err := m.Register(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second)); out.Result != "stale" {
		t.Fatalf("replacement accepted old revision=%+v", out)
	}
	if restored := quotaRecoveryRow(t, m, a, "model", "model-a"); restored.Clearable {
		t.Fatal("replacement inherited unverified provenance")
	}
}

func TestPacerQuotaRecoveryPreservesLaterCredentialAuthFailure(t *testing.T) {
	for _, code := range []int{401, 403, 503} {
		m, a := newQuotaRecoveryManager(t, "codex")
		markQuotaRecoveryResult(m, a, "model-a", 429, `{"error":{"type":"usage_limit_reached"}}`, false, nil)
		markQuotaRecoveryResult(m, a, "", code, `{"error":{"type":"non_quota_failure"}}`, false, nil)
		row := quotaRecoveryRow(t, m, a, "model", "model-a")
		before, _ := m.GetByID(a.ID)
		if row.Clearable {
			t.Fatalf("auth failure %d permits model recovery", code)
		}
		out := m.clearQuotaCooldownAt(context.Background(), a.ID, quotaRecoveryRequest(a, row), quotaRecoveryTestTime.Add(2*time.Second))
		if out.Cleared {
			t.Fatalf("auth failure %d was cleared", code)
		}
		after, _ := m.GetByID(a.ID)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("auth failure %d mutated", code)
		}
	}
}
