package pluginhost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPacerQuotaCooldownCallbacksCASAndNoTokenWrites(t *testing.T) {
	store := &recordingTokenStore{}
	m := coreauth.NewManager(store, nil, nil)
	h := New()
	h.SetAuthManager(m)
	a, err := m.Register(context.Background(), &coreauth.Auth{ID: "pacer-quota-dummy", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "dummy-not-a-real-token"}})
	if err != nil {
		t.Fatal(err)
	}
	mark := func() {
		retry := time.Hour
		m.MarkResult(context.Background(), coreauth.Result{AuthID: a.ID, Provider: "codex", Model: "model-a", RetryAfter: &retry, Error: &coreauth.Error{HTTPStatus: 429, Message: `{"error":{"type":"usage_limit_reached"}}`}})
	}
	mark()
	readRaw, _ := json.Marshal(pluginapi.HostQuotaCooldownReadRequest{AuthIndex: a.Index})
	read := func() pluginapi.HostQuotaCooldownReadResponse {
		t.Helper()
		raw, err := h.callFromPlugin(context.Background(), pluginabi.MethodHostQuotaCooldownRead, readRaw)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "dummy-not-a-real-token") || strings.Contains(string(raw), "usage_limit_reached") {
			t.Fatal("raw credential/error leaked")
		}
		resp, err := decodeRPCEnvelope[pluginapi.HostQuotaCooldownReadResponse](raw)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := read()
	if !first.Supported || len(first.Cooldowns) != 1 || !first.Cooldowns[0].Clearable {
		t.Fatalf("read=%+v", first)
	}
	row := first.Cooldowns[0]
	req := pluginapi.HostQuotaCooldownClearRequest{AuthIndex: a.Index, Scope: row.Scope, ModelKey: row.ModelKey, Revision: row.Revision, Evidence: pluginapi.HostQuotaRecoveryEvidence{Reason: "applicable_new_cycle", Source: "quota_endpoint", Epoch: "dummy-cycle-2", ObservedAt: time.Now(), AllApplicableWindowsPositive: true}}
	clear := func() pluginapi.HostQuotaCooldownClearResponse {
		t.Helper()
		rawReq, _ := json.Marshal(req)
		raw, err := h.callFromPlugin(context.Background(), pluginabi.MethodHostQuotaCooldownClear, rawReq)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := decodeRPCEnvelope[pluginapi.HostQuotaCooldownClearResponse](raw)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	mark()
	writes := store.saveCalls
	if resp := clear(); resp.Result != "stale" || resp.Cleared {
		t.Fatalf("stale RPC=%+v", resp)
	}
	req.Revision = read().Cooldowns[0].Revision
	req.Evidence.ObservedAt = time.Now()
	if resp := clear(); !resp.Cleared || resp.Result != "cleared" {
		t.Fatalf("recovery RPC=%+v", resp)
	}
	if store.saveCalls != writes {
		t.Fatal("quota callback wrote credential token store")
	}
	if resp := read(); len(resp.Cooldowns) != 0 {
		t.Fatalf("runtime quota still held: %+v", resp)
	}
	if pluginabi.SchemaVersion != 7 {
		t.Fatal("additive callback changed schema")
	}
}

func TestPacerQuotaCooldownCallbacksRejectUnknownAndBroadRequests(t *testing.T) {
	h := New()
	h.SetAuthManager(coreauth.NewManager(nil, nil, nil))
	for _, method := range []string{pluginabi.MethodHostQuotaCooldownRead, pluginabi.MethodHostQuotaCooldownClear} {
		for _, raw := range []string{`{}`, `{"auth_index":"unknown"}`, `{"auth_index":"unknown","reset_all":true}`, `{} {}`, `null`, strings.Repeat("x", 16385)} {
			if _, err := h.callFromPlugin(context.Background(), method, []byte(raw)); err == nil {
				t.Fatalf("accepted %s broad/invalid request", method)
			}
		}
	}
}
