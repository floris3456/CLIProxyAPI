package pluginhost

import (
	"context"
	"encoding/json"
	"testing"

	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPacerSourceHostCheckpointAccountAndRegistrationBinding(t *testing.T) {
	h := New()
	m := auth.NewManager(nil, nil, nil)
	h.SetAuthManager(m)
	a, err := m.Register(context.Background(), &auth.Auth{ID: t.Name(), Provider: "claude", Status: auth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	client := &cachePolicyRPCFixture{schema: 7, requests: make(map[string][]byte)}
	plugin, err := registerRPCPlugin(context.Background(), h, "pacer-source", client, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.RegisterPluginForTest("pacer-source", plugin)
	h.RegisterUsagePlugins()
	scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
	_, _ = usage.DefaultManager().BeginClaudeSource(nil, a.ID, a.Index, true)
	ctx := withHostCallbackPluginID(context.Background(), "pacer-source")
	request, _ := json.Marshal(pluginapi.HostUsageCheckpointRequest{Scope: scope})
	raw, err := h.callFromPlugin(ctx, pluginabi.MethodHostUsageCheckpoint, request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := decodeRPCEnvelope[pluginapi.HostUsageCheckpointResponse](raw)
	if err != nil {
		t.Fatal(err)
	}
	if response.Scope != scope || response.IncarnationID == "" || response.ManagerIncarnationID == "" {
		t.Fatalf("scope binding=%+v", response)
	}
	if response.Physical == nil || response.Physical.Version != 1 || response.Physical.Coverage != "unknown" || response.Physical.ThroughCut != nil {
		t.Fatalf("live admission gained physical proof=%+v", response.Physical)
	}
	wrong := scope
	wrong.AuthID = "not-the-account"
	bad, _ := json.Marshal(pluginapi.HostUsageCheckpointRequest{Scope: wrong})
	if _, err = h.callFromPlugin(ctx, pluginabi.MethodHostUsageCheckpoint, bad); err == nil {
		t.Fatal("mixed account tuple admitted")
	}
	if _, err = h.callFromPlugin(context.Background(), pluginabi.MethodHostUsageCheckpoint, request); err == nil {
		t.Fatal("anonymous sink admitted")
	}
	// Replacement native capability before usage registration cannot borrow the
	// old delivery incarnation even when both versions implement usage.handle.
	otherClient := &cachePolicyRPCFixture{schema: 7, requests: make(map[string][]byte)}
	replacement, err := registerRPCPlugin(context.Background(), h, "pacer-source", otherClient, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	setHostSnapshotForTest(h, true, capabilityRecord{id: "pacer-source", plugin: replacement})
	raw, err = h.callFromPlugin(ctx, pluginabi.MethodHostUsageCheckpoint, request)
	if err != nil {
		t.Fatal(err)
	}
	response, err = decodeRPCEnvelope[pluginapi.HostUsageCheckpointResponse](raw)
	if err != nil {
		t.Fatal(err)
	}
	if response.Coverage != "unknown" || response.Through != nil || response.IncarnationID != "" || response.Reason != "sink_instance_mismatch" {
		t.Fatalf("reload borrowed proof=%+v", response)
	}
	if response.Physical == nil || response.Physical.Coverage != "unknown" || response.Physical.FromCut != nil || response.Physical.ThroughCut != nil || response.Physical.Reason != "sink_instance_mismatch" {
		t.Fatalf("physical instance binding=%+v", response.Physical)
	}
}
