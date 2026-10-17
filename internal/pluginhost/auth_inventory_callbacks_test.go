package pluginhost

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHostAuthInventoryCheckpointDispatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	h := New()
	m := coreauth.NewManager(nil, nil, nil)
	h.SetAuthManager(m)
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	const account = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	verified, err := m.Register(ctx, &coreauth.Auth{ID: "claude-verified", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "t1", "account_uuid": account}})
	if err != nil {
		t.Fatal(err)
	}
	unverified, err := m.Register(ctx, &coreauth.Auth{ID: "claude-unverified", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "u1", "account_uuid": account}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Register(ctx, &coreauth.Auth{ID: "codex-other", Provider: "codex", Metadata: map[string]any{"type": "codex"}}); err != nil {
		t.Fatal(err)
	}
	refreshed := verified.Clone()
	refreshed.Metadata["access_token"] = "t2"
	refreshed.ObserveProviderAccount(account)
	if _, err = m.UpdateRefreshedAuth(ctx, verified, refreshed); err != nil {
		t.Fatal(err)
	}

	instance := &hostCallbackInstance{}
	callCtx := withHostCallbackIdentity(ctx, "pacer-inventory", instance)
	raw, err := h.callFromPlugin(callCtx, pluginabi.MethodHostAuthInventoryCheckpoint, []byte(`{"provider":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		t.Fatalf("envelope = %s (%v)", raw, err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(envelope.Result, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "provider", "coverage", "manager_incarnation_id", "sequence", "at", "tuples"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("missing %q in %s", key, envelope.Result)
		}
	}
	if _, ok := wire["reason"]; ok {
		t.Fatalf("known response carries reason: %s", envelope.Result)
	}
	if strings.Contains(string(envelope.Result), account) {
		t.Fatal("raw provider account UUID exported")
	}
	resp, err := decodeRPCEnvelope[pluginapi.HostAuthInventoryCheckpointResponse](raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Version != 1 || resp.Provider != "claude" || resp.Coverage != "known" || resp.ManagerIncarnationID == "" || resp.At.IsZero() || len(resp.Tuples) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	byID := map[string]pluginapi.HostAuthInventoryTuple{}
	for _, tuple := range resp.Tuples {
		if tuple.BindingSequence == 0 || tuple.BindingSequence > resp.Sequence || tuple.State != "active" {
			t.Fatalf("tuple = %+v (sequence %d)", tuple, resp.Sequence)
		}
		byID[tuple.AuthID] = tuple
	}
	if got := byID["claude-verified"]; got.AuthIndex != verified.Index || len(got.AccountRef) != 64 {
		t.Fatalf("verified tuple = %+v", got)
	} else if _, errHex := hex.DecodeString(got.AccountRef); errHex != nil || strings.ToLower(got.AccountRef) != got.AccountRef {
		t.Fatalf("account_ref not lowercase hex: %q", got.AccountRef)
	}
	if got := byID["claude-unverified"]; got.AuthIndex != unverified.Index || got.AccountRef != "" {
		t.Fatalf("unverified tuple = %+v", got)
	}
	var tuples []map[string]any
	if err = json.Unmarshal(wire["tuples"], &tuples); err != nil {
		t.Fatal(err)
	}
	for _, tuple := range tuples {
		if tuple["auth_id"] == "claude-unverified" {
			if _, present := tuple["account_ref"]; present {
				t.Fatalf("unknown account_ref not omitted: %s", wire["tuples"])
			}
		}
	}

	// Same authorization as host.auth.list: an anonymous callback context is admitted.
	if _, err = h.callFromPlugin(ctx, pluginabi.MethodHostAuthInventoryCheckpoint, []byte(`{"provider":"claude"}`)); err != nil {
		t.Fatalf("anonymous caller refused: %v", err)
	}
	raw, err = h.callFromPlugin(callCtx, pluginabi.MethodHostAuthInventoryCheckpoint, []byte(`{"provider":"codex"}`))
	if err != nil {
		t.Fatal(err)
	}
	if other, errDecode := decodeRPCEnvelope[pluginapi.HostAuthInventoryCheckpointResponse](raw); errDecode != nil || other.Coverage != "unsupported" || other.Reason != "unsupported_provider" || other.Tuples == nil || len(other.Tuples) != 0 {
		t.Fatalf("non-claude = %+v (%v)", other, errDecode)
	}
	for _, bad := range []string{``, `{}x`, `{"provider":"claude","extra":1}`, `{"provider":"claude"} {}`, `[]`, `{"provider":"` + strings.Repeat("c", 5000) + `"}`} {
		if _, err = h.callFromPlugin(callCtx, pluginabi.MethodHostAuthInventoryCheckpoint, []byte(bad)); err == nil {
			t.Fatalf("malformed request %.40q admitted", bad)
		}
	}
	instance.closed.Store(true)
	if _, err = h.callFromPlugin(callCtx, pluginabi.MethodHostAuthInventoryCheckpoint, []byte(`{"provider":"claude"}`)); err == nil {
		t.Fatal("closed callback instance admitted")
	}

	unbound := New()
	raw, err = unbound.callFromPlugin(ctx, pluginabi.MethodHostAuthInventoryCheckpoint, []byte(`{"provider":"claude"}`))
	if err != nil {
		t.Fatal(err)
	}
	if none, errDecode := decodeRPCEnvelope[pluginapi.HostAuthInventoryCheckpointResponse](raw); errDecode != nil || none.Coverage != "unknown" || none.Reason != "not_ready" || none.Tuples == nil {
		t.Fatalf("no manager = %+v (%v)", none, errDecode)
	}
}
