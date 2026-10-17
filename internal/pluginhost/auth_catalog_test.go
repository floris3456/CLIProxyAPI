package pluginhost

import (
	"context"
	"encoding/json"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type catalogClient struct {
	*cachePolicyRPCFixture
	instance *hostCallbackInstance
}

func (c *catalogClient) callbackInstance() *hostCallbackInstance { return c.instance }

func TestPacerStartupListenerInstallationAndHeldAdmission(t *testing.T) {
	h := New()
	defer h.ShutdownAll()
	m := coreauth.NewManager(nil, nil, nil)
	h.SetAuthManager(m)
	a, err := m.Register(context.Background(), &coreauth.Auth{ID: t.Name(), Provider: "claude", Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := &catalogClient{cachePolicyRPCFixture: &cachePolicyRPCFixture{schema: 7, requests: map[string][]byte{}}, instance: &hostCallbackInstance{}}
	plugin, err := registerRPCPlugin(context.Background(), h, "startup-listener", client, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	callback := withHostCallbackIdentity(context.Background(), "startup-listener", client.instance)
	h.RegisterPluginForTest("startup-listener", plugin)
	_, catalog, err := h.listAuthCatalog(callback)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.State != "ready" || catalog.ListenerState != "installing" {
		t.Fatalf("loaded auth borrowed uninstalled listener: %+v", catalog)
	}
	scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
	raw, _ := json.Marshal(pluginapi.HostUsageCheckpointRequest{Scope: scope})
	checkpoint := func(ctx context.Context) pluginapi.HostUsageCheckpointResponse {
		t.Helper()
		body, e := h.callFromPlugin(ctx, pluginabi.MethodHostUsageCheckpoint, raw)
		if e != nil {
			t.Fatal(e)
		}
		r, e := decodeRPCEnvelope[pluginapi.HostUsageCheckpointResponse](body)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	if r := checkpoint(callback); r.Coverage != "unknown" || r.Through != nil || r.IncarnationID != "" {
		t.Fatalf("pre-install callback borrowed proof: %+v", r)
	}
	h.RegisterUsagePlugins()
	_, catalog, err = h.listAuthCatalog(callback)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.ListenerState != "ready" {
		t.Fatal(catalog)
	}
	if r := checkpoint(callback); r.Reason != "source_not_enrolled" || r.Physical == nil || r.Physical.Coverage != "unknown" || r.Physical.FromCut != nil || r.Physical.ThroughCut != nil {
		t.Fatalf("registration alone invented a physical cut: %+v", r)
	}
	// Real source admission remains outstanding while the catalog/listener is
	// ready. No manual physical authority or manufactured quiescence is used.
	ticket, _ := usage.DefaultManager().BeginClaudeSource(context.Background(), a.ID, a.Index, true)
	defer ticket.Finish()
	if r := checkpoint(callback); r.Physical == nil || r.Physical.Coverage != "unknown" || r.Physical.Outstanding == 0 || r.Physical.ThroughCut != nil {
		t.Fatalf("catalog readiness bypassed held producer: %+v", r)
	}
	other := &catalogClient{cachePolicyRPCFixture: &cachePolicyRPCFixture{schema: 7, requests: map[string][]byte{}}, instance: &hostCallbackInstance{}}
	replacement, err := registerRPCPlugin(context.Background(), h, "startup-listener", other, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	setHostSnapshotForTest(h, true, capabilityRecord{id: "startup-listener", plugin: replacement})
	_, catalog, err = h.listAuthCatalog(callback)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.ListenerState != "retired" {
		t.Fatalf("old callback reclassified as startup: %+v", catalog)
	}
	if r := checkpoint(callback); r.Physical == nil || r.Physical.Coverage != "unknown" || r.Physical.FromCut != nil || r.Physical.ThroughCut != nil {
		t.Fatalf("old listener borrowed replacement proof: %+v", r)
	}
}
