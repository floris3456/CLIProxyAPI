package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type cachePolicyRPCFixture struct {
	schema   uint32
	requests map[string][]byte
	fail     bool
}

func (p *cachePolicyRPCFixture) Shutdown() {}
func (p *cachePolicyRPCFixture) Call(_ context.Context, method string, raw []byte) ([]byte, error) {
	p.requests[method] = append([]byte(nil), raw...)
	if p.fail {
		return nil, errors.New("private plugin failure")
	}
	switch method {
	case pluginabi.MethodPluginRegister:
		return marshalRPCResult(rpcRegistration{SchemaVersion: p.schema, Capabilities: rpcCapabilities{CachePolicy: true, UsagePlugin: true, RequestLifecyclePlugin: true}})
	case pluginabi.MethodCachePolicyHandle:
		return marshalRPCResult(pluginapi.CachePolicyResponse{StatusCode: 200, Body: json.RawMessage(`{"protocol":1}`)})
	case pluginabi.MethodCachePolicyResolve:
		var r pluginapi.CachePolicyResolveRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return marshalRPCResult(pluginapi.CachePolicy{Protocol: 1, Scope: r.Scope, ContextEpoch: r.ContextEpoch, Revision: r.Revision, Kind: r.Kind, TTL: "1h"})
	case pluginabi.MethodUsageHandle:
		return marshalRPCResult(rpcEmptyResponse{})
	case pluginabi.MethodCachePolicyAttempt:
		var r pluginapi.CachePolicyAttemptRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return marshalRPCResult(pluginapi.CachePolicyAttemptResponse{Policy: r.Policy, StartedAt: time.Now()})
	default:
		return nil, errors.New("unsupported")
	}
}
func TestPacerNativePolicyRPCAndFailClosed(t *testing.T) {
	client := &cachePolicyRPCFixture{schema: 7, requests: make(map[string][]byte)}
	host := New()
	plug, err := registerRPCPlugin(context.Background(), host, "pacer", client, pluginabi.MethodPluginRegister, nil)
	if err != nil || plug.Capabilities.CachePolicy == nil {
		t.Fatalf("register=%+v %v", plug, err)
	}
	host.RegisterPluginForTest("pacer", plug)
	r := pluginapi.CachePolicyResolveRequest{Protocol: 1, Scope: pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "install", SessionID: "chat", ProviderRoute: "claude"}, Capability: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ContextEpoch: "epoch", Revision: 1, Kind: "primary", RequestID: "logical", SourceFormat: "claude", Model: "claude-sonnet-4-6"}
	p, err := host.ResolveCachePolicy(context.Background(), r)
	if err != nil || p.TTL != "1h" {
		t.Fatalf("resolve=%+v %v", p, err)
	}
	if _, err = host.CachePolicyAttempt(context.Background(), pluginapi.CachePolicyAttemptRequest{Operation: "begin", RequestID: "logical", AttemptID: "physical", Capability: r.Capability, Policy: p}); err != nil {
		t.Fatal(err)
	}
	_, err = host.HandleCachePolicy(context.Background(), pluginapi.CachePolicyRequest{Method: "GET", Scope: r.Scope, Capability: r.Capability})
	if err != nil {
		t.Fatal(err)
	}
	var decoded pluginapi.CachePolicyResolveRequest
	if err = json.Unmarshal(client.requests[pluginabi.MethodCachePolicyResolve], &decoded); err != nil || decoded != r {
		t.Fatalf("RPC reference lost: %+v %v", decoded, err)
	}
	creation := int64(75)
	plug.Capabilities.UsagePlugin.HandleUsage(context.Background(), pluginapi.UsageRecord{RequestID: "physical", LifecycleRequestID: "logical", CachePolicy: &pluginapi.CachePolicyEvidence{Selected: &p}, Detail: pluginapi.UsageDetail{CacheCreation: &pluginapi.CacheCreationUsage{Ephemeral1hInputTokens: &creation}}})
	var record pluginapi.UsageRecord
	if err = json.Unmarshal(client.requests[pluginabi.MethodUsageHandle], &record); err != nil || record.LifecycleRequestID != "logical" || record.CachePolicy.Selected.TTL != "1h" || record.Detail.CacheCreation.Ephemeral5mInputTokens != nil || *record.Detail.CacheCreation.Ephemeral1hInputTokens != 75 {
		t.Fatalf("RPC usage lost: %+v %v", record, err)
	}
	client.fail = true
	if _, err = host.ResolveCachePolicy(context.Background(), r); err == nil || err.Error() != "pacer_cache_policy_unavailable" {
		t.Fatalf("failure did not close/redact: %v", err)
	}
	client.fail = false
	client.schema = 6
	legacy, err := registerRPCPlugin(context.Background(), host, "pacer", client, pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Capabilities.CachePolicy != nil {
		t.Fatal("legacy schema acquired policy capability")
	}
	host = New()
	host.RegisterPluginForTest("pacer", legacy)
	if _, err = host.ResolveCachePolicy(context.Background(), r); err == nil {
		t.Fatal("legacy fallback bypass")
	}
	host.RegisterPluginForTest("imposter", plug)
	if _, err = host.ResolveCachePolicy(context.Background(), r); err == nil {
		t.Fatal("other plugin acquired pacer route")
	}
	withoutCompletion := plug
	withoutCompletion.Capabilities.RequestLifecyclePlugin = nil
	host = New()
	host.RegisterPluginForTest("pacer", withoutCompletion)
	if _, err = host.ResolveCachePolicy(context.Background(), r); err == nil {
		t.Fatal("policy admission without logical settlement capability")
	}
}
