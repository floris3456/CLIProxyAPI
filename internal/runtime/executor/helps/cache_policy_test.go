package helps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestPacerFinalMarkerBoundary(t *testing.T) {
	p := &pluginapi.CachePolicy{Protocol: 1, Scope: pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "install", SessionID: "chat", ProviderRoute: "claude"}, ContextEpoch: "epoch", Revision: 1, TTL: "1h", Kind: "primary"}
	opts := executor.Options{CachePolicy: p}
	body := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"t","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}},"cache_control":{"type":"ephemeral","ttl":"5m"}}],"system":[{"type":"text","text":"prefix","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	out, err := EnforceClaudeCachePolicy(body, opts, "http://127.0.0.1:1234/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(out, "tools.0.input_schema.properties.cache_control.type").String() != "string" || gjson.GetBytes(out, "tools.0.input_schema.properties.cache_control.ttl").Exists() {
		t.Fatal("tool schema rewritten")
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/v1/messages", nil)
	r.Header.Set("X-Pacer-Cache-Capability", "private")
	if err = FinalizeClaudeCachePolicyHeaders(r, out, opts, nil); err != nil || r.Header.Get("X-Pacer-Cache-Capability") != "" {
		t.Fatalf("finalize %v", err)
	}
	for _, path := range []string{"tools.0.cache_control.ttl", "system.0.cache_control.ttl", "messages.0.content.0.cache_control.ttl"} {
		if gjson.GetBytes(out, path).String() != "1h" {
			t.Fatal(path)
		}
	}
	for _, upstream := range []string{"http://api.anthropic.com/v1/messages", "https://unverified.invalid/v1/messages"} {
		if _, err = EnforceClaudeCachePolicy(body, opts, upstream); err == nil {
			t.Fatal("unsupported upstream accepted")
		}
	}
	p.TTL = "2h"
	if _, err = EnforceClaudeCachePolicy(body, opts, "https://api.anthropic.com/v1/messages"); err == nil {
		t.Fatal("unsupported TTL accepted")
	}
	p.TTL = "1h"
	client := CachePolicyHTTPClient(&http.Client{}, opts)
	if client.CheckRedirect == nil || client.CheckRedirect(r, nil) != http.ErrUseLastResponse {
		t.Fatal("managed redirects enabled")
	}
	noCache := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`)
	reporter := &UsageReporter{}
	if err = FinalizeClaudeCachePolicyHeaders(r, noCache, opts, reporter); err != nil {
		t.Fatal(err)
	}
	if reporter.cachePolicy.EffectiveTTL != "" || reporter.cachePolicy.BreakpointCount != 0 {
		t.Fatal("no-cache marked as refresh")
	}
}
