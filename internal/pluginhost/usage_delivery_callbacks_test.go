package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type usageDeliveryBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *usageDeliveryBarrier) HandleUsage(context.Context, coreusage.Record) {
	b.entered <- struct{}{}
	<-b.release
}
func waitDeliveryBarrier(t *testing.T, b *usageDeliveryBarrier) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("usage dispatch did not reach barrier")
	}
}

func TestPacerDeliveryNativePersistenceFailureReachesManager(t *testing.T) {
	client := &cachePolicyRPCFixture{schema: 7, requests: make(map[string][]byte)}
	h := New()
	plug, err := registerRPCPlugin(context.Background(), h, "pacer-delivery", client, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.RegisterPluginForTest("pacer-delivery", plug)
	m := coreusage.NewManager(4)
	defer m.Stop()
	m.RegisterNamed("plugin:pacer-delivery", &usageAdapter{host: h, pluginID: "pacer-delivery", plugin: plug.Capabilities.UsagePlugin})
	barrier := &usageDeliveryBarrier{entered: make(chan struct{}, 1), release: make(chan struct{})}
	m.Register(barrier)
	defer close(barrier.release)
	client.fail = true
	m.Publish(context.Background(), coreusage.Record{Provider: "claude", RequestedAt: time.Now()})
	waitDeliveryBarrier(t, barrier)
	s := m.DeliverySnapshot("plugin:pacer-delivery", "claude")
	if s.Delivery.CallbackErrors != 1 || s.Delivery.CallbackAcknowledged != 0 || s.Through != nil {
		t.Fatalf("RPC failure swallowed: %+v", s)
	}
	client.fail = false
	barrier.release <- struct{}{}
	m.Publish(context.Background(), coreusage.Record{Provider: "claude", RequestedAt: time.Now()})
	waitDeliveryBarrier(t, barrier)
	s = m.DeliverySnapshot("plugin:pacer-delivery", "claude")
	if s.Delivery.CallbackErrors != 1 || s.Delivery.CallbackAcknowledged != 1 || s.Supported || s.Coverage != "unsupported" {
		t.Fatalf("error history erased after recovery: %+v", s)
	}
}

func TestPacerDeliveryNativeErrorEnvelopeCannotAcknowledge(t *testing.T) {
	a := &rpcPluginAdapter{client: staticEnvelopePluginClient{raw: marshalRPCError("persistence_failed", "dummy store refused commit")}}
	if err := a.HandleUsageAcknowledged(context.Background(), pluginapi.UsageRecord{}); err == nil {
		t.Fatal("native persistence failure became ack")
	}
}

func TestPacerDeliveryAdapterMissingFusedAndReplacementRefuseAck(t *testing.T) {
	h := New()
	old := &capturingUsagePlugin{captured: make(chan pluginapi.UsageRecord, 1)}
	a := &usageAdapter{host: h, pluginID: "pacer-delivery", plugin: old}
	if err := a.HandleUsageAcknowledged(context.Background(), coreusage.Record{}); !errors.Is(err, coreusage.ErrDeliveryMissing) {
		t.Fatalf("missing=%v", err)
	}
	h.RegisterPluginForTest("pacer-delivery", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{UsagePlugin: old}})
	if err := a.HandleUsageAcknowledged(context.Background(), coreusage.Record{}); !errors.Is(err, coreusage.ErrDeliveryUnacknowledged) {
		t.Fatalf("legacy void acquired ack: %v", err)
	}
	setHostSnapshotForTest(h, true, capabilityRecord{id: "pacer-delivery", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{UsagePlugin: &capturingUsagePlugin{captured: make(chan pluginapi.UsageRecord, 1)}}}})
	if err := a.HandleUsageAcknowledged(context.Background(), coreusage.Record{}); !errors.Is(err, coreusage.ErrDeliveryUnacknowledged) {
		t.Fatalf("replaced=%v", err)
	}
	good, _ := marshalRPCResult(rpcEmptyResponse{})
	oldRPC := &rpcPluginAdapter{client: staticEnvelopePluginClient{raw: good}}
	newRPC := &rpcPluginAdapter{client: staticEnvelopePluginClient{raw: good}}
	a.plugin = oldRPC
	setHostSnapshotForTest(h, true, capabilityRecord{id: "pacer-delivery", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{UsagePlugin: newRPC}}})
	if err := a.HandleUsageAcknowledged(context.Background(), coreusage.Record{}); !errors.Is(err, coreusage.ErrDeliveryMissing) {
		t.Fatalf("replacement RPC success acknowledged old adapter: %v", err)
	}
	h.mu.Lock()
	h.fused["pacer-delivery"] = "dummy"
	h.mu.Unlock()
	if err := a.HandleUsageAcknowledged(context.Background(), coreusage.Record{}); !errors.Is(err, coreusage.ErrDeliveryFused) {
		t.Fatalf("fused=%v", err)
	}
}

func TestPacerDeliveryHostCallbackIdentityAndUnsupportedContract(t *testing.T) {
	h := New()
	ctx := withHostCallbackPluginID(context.Background(), "pacer-delivery-callback")
	for _, provider := range []string{"claude", "codex"} {
		raw, err := h.callFromPlugin(ctx, pluginabi.MethodHostUsageDelivery, []byte(`{"provider":"`+provider+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := decodeRPCEnvelope[pluginapi.HostUsageDeliveryResponse](raw)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Supported || resp.Coverage != "unsupported" || resp.Through != nil || strings.Contains(string(raw), `"through"`) {
			t.Fatalf("unproven horizon=%s", raw)
		}
		if resp.Provider != provider || resp.ManagerIncarnationID == "" || resp.Reason != "sink_missing" {
			t.Fatalf("diagnostics=%+v", resp)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"provider":"home"}`, `{"provider":"claude","plugin_id":"other"}`, `{"provider":"claude","through":"2030-01-01T00:00:00Z"}`, `{"provider":"claude","wait":true}`, `{} {}`, strings.Repeat("x", 1025)} {
		if _, err := h.callFromPlugin(ctx, pluginabi.MethodHostUsageDelivery, []byte(raw)); err == nil {
			t.Fatalf("accepted invalid callback=%s", raw)
		}
	}
	if _, err := h.callFromPlugin(context.Background(), pluginabi.MethodHostUsageDelivery, []byte(`{"provider":"claude"}`)); err == nil {
		t.Fatal("caller selected identity without native context")
	}
	instance := &hostCallbackInstance{}
	instance.closed.Store(true)
	if _, err := h.callFromPlugin(withHostCallbackIdentity(context.Background(), "pacer-delivery-callback", instance), pluginabi.MethodHostUsageDelivery, []byte(`{"provider":"claude"}`)); err == nil {
		t.Fatal("retired callback instance admitted")
	}
	if pluginabi.SchemaVersion != 7 {
		t.Fatal("additive schema changed")
	}
	// JSON itself makes absent horizon unambiguous, even for an empty manager.
	encoded, _ := json.Marshal(coreusage.NewManager(1).DeliverySnapshot("plugin:unknown", "claude"))
	if strings.Contains(string(encoded), `"through"`) {
		t.Fatalf("empty queue acquired horizon: %s", encoded)
	}
}
