package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const pacerTestCapability = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const pacerTestModel = "claude-sonnet-4-6"

// This fixture is deliberately not a policy state implementation. It enforces a fixed
// owned scope/reference so the host's rejection and execution boundaries are exercised.
type pacerFixture struct {
	mu          sync.Mutex
	attempts    map[string][]string
	started     map[string]time.Time
	outcomes    map[string]string
	denyAttempt string
	ttl         string
	handled     []pluginapi.CachePolicyRequest
	records     chan pluginapi.UsageRecord
	completed   chan pluginapi.RequestCompletion
}

func (p *pacerFixture) HandleCachePolicy(_ context.Context, r pluginapi.CachePolicyRequest) (pluginapi.CachePolicyResponse, error) {
	p.handled = append(p.handled, r)
	if r.Scope.ProviderRoute != "claude" {
		return pluginapi.CachePolicyResponse{}, pluginabi.NewError("unsupported", "unsupported", 422)
	}
	if r.Scope.CallerScope != session.CallerScope("test-key") || r.Scope.SessionID != "chat" || (r.Capability != "" && r.Capability != pacerTestCapability) {
		return pluginapi.CachePolicyResponse{}, pluginabi.NewError("foreign", "foreign", 403)
	}
	return pluginapi.CachePolicyResponse{StatusCode: 200, Body: json.RawMessage(`{"protocol":1,"active":{"ttl":"5m","context_epoch":"epoch","revision":1}}`)}, nil
}
func (p *pacerFixture) ResolveCachePolicy(_ context.Context, r pluginapi.CachePolicyResolveRequest) (pluginapi.CachePolicy, error) {
	if r.Scope.ProviderRoute != "claude" {
		return pluginapi.CachePolicy{}, pluginabi.NewError("unsupported", "unsupported", 422)
	}
	if r.Scope.CallerScope != session.CallerScope("test-key") || r.Scope.SessionID != "chat" || r.Scope.InstallationID != "install" || r.Capability != pacerTestCapability {
		return pluginapi.CachePolicy{}, pluginabi.NewError("foreign", "foreign", 403)
	}
	if r.ContextEpoch != "epoch" || r.Revision != 1 {
		return pluginapi.CachePolicy{}, pluginabi.NewError("stale", "stale", 409)
	}
	return pluginapi.CachePolicy{Protocol: 1, Scope: r.Scope, ContextEpoch: r.ContextEpoch, Revision: r.Revision, TTL: p.ttl, Kind: r.Kind}, nil
}

func (p *pacerFixture) CachePolicyAttempt(_ context.Context, r pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Capability != pacerTestCapability || r.AttemptID == "" || r.RequestID == "" {
		return pluginapi.CachePolicyAttemptResponse{}, pluginabi.NewError("denied", "denied", 403)
	}
	if p.attempts == nil {
		p.attempts = make(map[string][]string)
		p.started = make(map[string]time.Time)
		p.outcomes = make(map[string]string)
	}
	p.attempts[r.AttemptID] = append(p.attempts[r.AttemptID], r.Operation)
	if r.Operation == p.denyAttempt {
		return pluginapi.CachePolicyAttemptResponse{}, pluginabi.NewError("pending", "pending", 409)
	}
	if r.Operation == "begin" {
		p.started[r.AttemptID] = time.Now()
	} else if !r.StartedAt.Equal(p.started[r.AttemptID]) {
		return pluginapi.CachePolicyAttemptResponse{}, pluginabi.NewError("snapshot", "snapshot", 409)
	}
	if r.Operation == "finish" {
		p.outcomes[r.AttemptID] = r.Outcome
	}
	return pluginapi.CachePolicyAttemptResponse{Policy: r.Policy, StartedAt: p.started[r.AttemptID]}, nil
}
func (p *pacerFixture) HandleUsage(_ context.Context, r pluginapi.UsageRecord) {
	if r.AuthID == "pacer-dummy" {
		p.records <- r
	}
}
func (p *pacerFixture) HandleRequestComplete(_ context.Context, r pluginapi.RequestCompletion) error {
	p.completed <- r
	return nil
}

func newPacerTestServer(t *testing.T) (*Server, *pacerFixture, *pluginhost.Host) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "dummy-management-key")
	p := &pacerFixture{ttl: "5m", records: make(chan pluginapi.UsageRecord, 32), completed: make(chan pluginapi.RequestCompletion, 64)}
	host := pluginhost.New()
	host.RegisterPluginForTest("pacer", pluginapi.Plugin{SchemaVersion: 7, Capabilities: pluginapi.Capabilities{CachePolicy: p, UsagePlugin: p, RequestLifecyclePlugin: p}})
	s := newTestServerWithOptions(t, WithPluginHost(host))
	previous := access.RegisteredProviders()
	s.cfg.APIKeys = []string{"test-key", "foreign-key"}
	configaccess.Register(&s.cfg.SDKConfig)
	s.accessManager.SetProviders(access.RegisteredProviders())
	t.Cleanup(func() {
		configaccess.Register(nil)
		for _, old := range previous {
			access.RegisterProvider(old.Identifier(), old)
		}
		usage.RegisterNamedPlugin("plugin:pacer", pacerNoUsage{})
	})
	return s, p, host
}

type pacerNoUsage struct{}

func (pacerNoUsage) HandleUsage(context.Context, usage.Record) {}

func pacerHeaders(r *http.Request) {
	r.Header.Set("Authorization", "Bearer test-key")
	r.Header.Set("Content-Type", "application/json")
	for k, v := range map[string]string{"Protocol": "1", "Installation": "install", "Session": "chat", "Route": "claude", "Epoch": "epoch", "Revision": "1", "Kind": "primary", "Capability": pacerTestCapability} {
		r.Header.Set(cachepolicy.HeaderPrefix+k, v)
	}
}

func TestPacerPolicyRouteAuthAndLimits(t *testing.T) {
	s, p, _ := newPacerTestServer(t)
	post := `{"protocol":1,"operation":"ensure","installation_id":"install","session_id":"chat","provider_route":"claude","context_epoch":"epoch","expected_revision":0,"ttl":"5m","operation_id":"op","enrollment":"new_empty"}`
	query := "?protocol=1&installation_id=install&session_id=chat&provider_route=claude"
	for _, tt := range []struct {
		name, method, path, key, body, cap string
		status                             int
	}{
		{"missing key", "POST", cachepolicy.Path, "", post, "", 401},
		{"invalid key", "POST", cachepolicy.Path, "invalid", post, "", 401},
		{"management key", "POST", cachepolicy.Path, "dummy-management-key", post, "", 401},
		{"ensure", "POST", cachepolicy.Path, "test-key", post, pacerTestCapability, 200},
		{"ensure requires capability", "POST", cachepolicy.Path, "test-key", post, "", 403},
		{"read", "GET", cachepolicy.Path + query, "test-key", "", pacerTestCapability, 200},
		{"foreign caller", "GET", cachepolicy.Path + query, "foreign-key", "", pacerTestCapability, 403},
		{"foreign cap", "GET", cachepolicy.Path + query, "test-key", "", strings.Repeat("x", 42) + "A", 403},
		{"missing cap", "GET", cachepolicy.Path + query, "test-key", "", "", 403},
		{"wrong route", "GET", strings.Replace(cachepolicy.Path+query, "claude", "codex", 1), "test-key", "", pacerTestCapability, 422},
		{"get mutation", "GET", cachepolicy.Path + query + "&operation=prepare", "test-key", "", pacerTestCapability, 400},
		{"get body", "GET", cachepolicy.Path + query, "test-key", post, pacerTestCapability, 400},
		{"method", "PUT", cachepolicy.Path, "test-key", post, "", 405},
		{"unknown operation", "POST", cachepolicy.Path, "test-key", strings.Replace(post, "ensure", "delete", 1), "", 400},
		{"caller injection", "POST", cachepolicy.Path, "test-key", strings.TrimSuffix(post, "}") + `,"caller_scope":"forged"}`, "", 400},
		{"duplicate", "POST", cachepolicy.Path, "test-key", strings.TrimSuffix(post, "}") + `,"ttl":"1h"}`, "", 400},
		{"case duplicate", "POST", cachepolicy.Path, "test-key", strings.TrimSuffix(post, "}") + `,"TTL":"1h"}`, "", 400},
		{"trailing", "POST", cachepolicy.Path, "test-key", post + `{}`, "", 400},
		{"large", "POST", cachepolicy.Path, "test-key", strings.Repeat("x", 16385), "", 413},
		{"unknown exact route", "POST", cachepolicy.Path + "/other", "test-key", post, "", 404},
		{"public mutation refused", "POST", "/v0/resource/plugins/pacer/cache-policy", "test-key", post, "", 404},
		{"management not inference auth", "GET", "/v8/management/config", "test-key", "", "", 401},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			if tt.key != "" {
				r.Header.Set("Authorization", "Bearer "+tt.key)
			}
			if tt.cap != "" {
				r.Header.Set(cachepolicy.CapabilityHeader, tt.cap)
			}
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.status, w.Body.String())
			}
		})
	}
	for _, r := range p.handled {
		if r.Scope.CallerScope == "test-key" || len(r.Scope.CallerScope) != 64 {
			t.Fatal("raw/unhashed caller")
		}
	}
	s.accessManager.SetProviders(nil)
	r := httptest.NewRequest("POST", cachepolicy.Path, strings.NewReader(post))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("anonymous config policy status=%d", w.Code)
	}
}

const pacerTestJSON = `{"id":"msg_pacer","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":13,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}}}`
const pacerTestSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_pacer\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":0,\"cache_read_input_tokens\":13,\"cache_creation_input_tokens\":100,\"cache_creation\":{\"ephemeral_5m_input_tokens\":25,\"ephemeral_1h_input_tokens\":75}}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestPacerMessagesResponsesWireAndUsage(t *testing.T) {
	s, p, host := newPacerTestServer(t)
	type wire struct {
		body   []byte
		header http.Header
	}
	captured := make(chan wire, 32)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		b, _ := io.ReadAll(r.Body)
		captured <- wire{b, r.Header.Clone()}
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.25")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", "1893456000")
		if gjson.GetBytes(b, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, pacerTestSSE)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, pacerTestJSON)
		}
	}))
	t.Cleanup(upstream.Close)
	m := s.handlers.AuthManager
	// Conflicting payload TTL and OAuth's automatic 1h shaping both precede the final boundary.
	s.cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: pacerTestModel, Protocol: "claude"}},
		Params: map[string]any{"messages.0.content.0.cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
	}}}
	m.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
	_, err := m.Register(context.Background(), &auth.Auth{ID: "pacer-dummy", Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-dummy-upstream-key", "base_url": upstream.URL, "cloak_mode": "always"}, Metadata: map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "claude_device_ids": []string{strings.Repeat("0", 64)}}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("pacer-dummy", "claude", []*registry.ModelInfo{{ID: pacerTestModel, Object: "model", Type: "claude", OwnedBy: "anthropic", MaxCompletionTokens: 8192}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("pacer-dummy") })
	host.RegisterUsagePlugins()
	prevEnabled, prevStats := redisqueue.Enabled(), redisqueue.UsageStatisticsEnabled()
	redisqueue.SetEnabled(true)
	redisqueue.SetUsageStatisticsEnabled(true)
	t.Cleanup(func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevEnabled)
		redisqueue.SetUsageStatisticsEnabled(prevStats)
	})
	seenIDs := map[string]bool{}
	for _, ttl := range []string{"5m", "1h"} {
		for _, endpoint := range []string{"messages", "responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", ttl, endpoint, stream), func(t *testing.T) {
					p.ttl = ttl
					body := fmt.Sprintf(`{"model":%q,"max_tokens":1024,"stream":%v,"system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`, pacerTestModel, stream)
					if endpoint == "responses" {
						body = fmt.Sprintf(`{"model":%q,"stream":%v,"instructions":"stable system","input":[{"role":"user","content":"hello"}]}`, pacerTestModel, stream)
					}
					r := httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(body))
					pacerHeaders(r)
					w := httptest.NewRecorder()
					s.engine.ServeHTTP(w, r)
					if w.Code != 200 {
						t.Fatalf("status %d: %s", w.Code, w.Body.String())
					}
					var got wire
					select {
					case got = <-captured:
					case <-time.After(3 * time.Second):
						t.Fatal("no upstream request")
					}
					markers := 0
					var inspect func(gjson.Result)
					inspect = func(v gjson.Result) {
						if v.IsObject() {
							v.ForEach(func(k, c gjson.Result) bool {
								if k.String() == "cache_control" {
									markers++
									if c.Get("ttl").String() != ttl {
										t.Errorf("wire TTL=%s want %s", c.Get("ttl"), ttl)
									}
								} else {
									inspect(c)
								}
								return true
							})
						} else if v.IsArray() {
							v.ForEach(func(_, c gjson.Result) bool { inspect(c); return true })
						}
					}
					inspect(gjson.ParseBytes(got.body))
					if markers == 0 {
						t.Fatal("no cache breakpoints")
					}
					if ttl == "1h" && !strings.Contains(got.header.Get("Anthropic-Beta"), "extended-cache-ttl-2025-04-11") {
						t.Fatal("missing beta")
					}
					if cachepolicy.Managed(got.header) {
						t.Fatal("private managed header leaked upstream")
					}
					var rec pluginapi.UsageRecord
					select {
					case rec = <-p.records:
					case <-time.After(3 * time.Second):
						t.Fatal("no native usage")
					}
					if rec.RequestID == "" || seenIDs[rec.RequestID] || rec.LifecycleRequestID == "" || rec.LifecycleRequestID == rec.RequestID {
						t.Fatal("attempt/lifecycle correlation missing or reused")
					}
					seenIDs[rec.RequestID] = true
					if rec.CachePolicy == nil || rec.CachePolicy.Selected.TTL != ttl || rec.CachePolicy.EffectiveTTL != ttl || !rec.CachePolicy.Dispatched || rec.CachePolicy.UpstreamStartedAt.IsZero() {
						t.Fatalf("policy evidence=%+v", rec.CachePolicy)
					}
					if rec.Detail.CacheCreation == nil || *rec.Detail.CacheCreation.Ephemeral5mInputTokens != 25 || *rec.Detail.CacheCreation.Ephemeral1hInputTokens != 75 || *rec.Detail.CacheCreation.UnknownTokens != 0 {
						t.Fatalf("raw split lost: %+v", rec.Detail.CacheCreation)
					}
					if rec.AccountingVersion != 2 || rec.TokenBreakdown.TotalTokens != 127 || rec.TokenBreakdown.Input.UncachedTokens != 11 || rec.TokenBreakdown.Input.CacheWriteTokens != 100 {
						t.Fatalf("normalized usage=%+v", rec.TokenBreakdown)
					}
					if rec.ResponseHeaders.Get("Anthropic-Ratelimit-Unified-5h-Reset") != "1893456000" {
						t.Fatal("passive response headers missing")
					}
					if rec.ResponseHeadersObservedAt["Anthropic-Ratelimit-Unified-5h-Reset"].IsZero() {
						t.Fatal("header observation clock missing")
					}
					select {
					case completion := <-p.completed:
						if completion.RequestID != rec.LifecycleRequestID {
							t.Fatal("logical completion correlation lost")
						}
					case <-time.After(3 * time.Second):
						t.Fatal("no logical completion")
					}
					p.mu.Lock()
					sequence := strings.Join(p.attempts[rec.RequestID], ",")
					outcome := p.outcomes[rec.RequestID]
					p.mu.Unlock()
					if sequence != "begin,validate,finish" || outcome != "succeeded" || rec.CachePolicy.AttemptStartedAt.IsZero() {
						t.Fatalf("attempt lifecycle %s/%s", sequence, outcome)
					}
					queue := redisqueue.PopOldest(10)
					found := false
					for _, raw := range queue {
						if gjson.GetBytes(raw, "execution_id").String() != rec.RequestID {
							continue
						}
						found = true
						if gjson.GetBytes(raw, "lifecycle_request_id").String() != rec.LifecycleRequestID || gjson.GetBytes(raw, "cache_policy.selected.ttl").String() != ttl || gjson.GetBytes(raw, "tokens.cache_creation.ephemeral_1h_input_tokens").Int() != 75 || gjson.GetBytes(raw, "token_breakdown.total_tokens").Int() != 127 {
							t.Fatalf("queue fields lost: %s", raw)
						}
					}
					if !found {
						t.Fatal("attempt missing from CPAMP queue")
					}
				})
			}
		}
	}
	before := upstreamCalls.Load()
	for _, tt := range []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"foreign caller", func(r *http.Request) { r.Header.Set("Authorization", "Bearer foreign-key") }, 403},
		{"guessed session", func(r *http.Request) { r.Header.Set("X-Pacer-Cache-Session", "guessed") }, 403},
		{"foreign capability", func(r *http.Request) { r.Header.Set(cachepolicy.CapabilityHeader, strings.Repeat("x", 42)+"A") }, 403},
		{"stale revision", func(r *http.Request) { r.Header.Set("X-Pacer-Cache-Revision", "2") }, 409},
		{"partial headers", func(r *http.Request) { r.Header.Del("X-Pacer-Cache-Epoch") }, 400},
		{"duplicate headers", func(r *http.Request) { r.Header.Add("X-Pacer-Cache-Revision", "1") }, 400},
		{"unsupported helper", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`))
		}, 422},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`))
			pacerHeaders(r)
			tt.mutate(r)
			w := httptest.NewRecorder()
			s.engine.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.status, w.Body.String())
			}
		})
	}
	for _, operation := range []string{"begin", "validate"} {
		p.mu.Lock()
		p.denyAttempt = operation
		p.mu.Unlock()
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`))
		pacerHeaders(r)
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, r)
		if w.Code != 409 {
			t.Fatalf("%s rejection status=%d", operation, w.Code)
		}
	}
	s.pluginHost = pluginhost.New()
	s.handlers.SetPluginHost(s.pluginHost)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`))
	pacerHeaders(r)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("missing plugin status=%d", w.Code)
	}
	if upstreamCalls.Load() != before {
		t.Fatal("refused request reached upstream")
	}
}

func TestPacerTruncatedWireRetainsPartialUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, strings.Split(pacerTestSSE, "event: message_stop")[0])
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", fmt.Sprint(len(pacerTestJSON)+100))
			_, _ = io.WriteString(w, pacerTestJSON)
		}
	}))
	t.Cleanup(upstream.Close)
	for _, endpoint := range []string{"messages", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", endpoint, stream), func(t *testing.T) {
				s, p, host := newPacerTestServer(t)
				m := s.handlers.AuthManager
				m.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
				m.SetRetryConfig(0, 0, 0)
				if _, err := m.Register(context.Background(), &auth.Auth{ID: "pacer-dummy", Provider: "claude", Attributes: map[string]string{"api_key": "dummy", "base_url": upstream.URL, "cloak_mode": "never"}}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient("pacer-dummy", "claude", []*registry.ModelInfo{{ID: pacerTestModel, Object: "model", Type: "claude", OwnedBy: "anthropic"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("pacer-dummy") })
				host.RegisterUsagePlugins()
				body := fmt.Sprintf(`{"model":%q,"max_tokens":1024,"stream":%v,"messages":[{"role":"user","content":"hello"}]}`, pacerTestModel, stream)
				if endpoint == "responses" {
					body = fmt.Sprintf(`{"model":%q,"stream":%v,"input":"hello"}`, pacerTestModel, stream)
				}
				r := httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(body))
				pacerHeaders(r)
				w := httptest.NewRecorder()
				s.engine.ServeHTTP(w, r)
				var rec pluginapi.UsageRecord
				select {
				case rec = <-p.records:
				case <-time.After(3 * time.Second):
					t.Fatal("missing partial usage")
				}
				select {
				case completion := <-p.completed:
					if completion.RequestID != rec.LifecycleRequestID || completion.Outcome != pluginapi.RequestCompletionFailed {
						t.Fatalf("partial request completion=%+v", completion)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("missing partial completion")
				}
				if !rec.Failed || rec.Detail.CacheCreation == nil || rec.Detail.CacheCreation.Ephemeral5mInputTokens == nil || *rec.Detail.CacheCreation.Ephemeral5mInputTokens != 25 || *rec.Detail.CacheCreation.Ephemeral1hInputTokens != 75 {
					t.Fatalf("partial usage lost: %+v", rec)
				}
				p.mu.Lock()
				outcome := p.outcomes[rec.RequestID]
				sequence := strings.Join(p.attempts[rec.RequestID], ",")
				p.mu.Unlock()
				if outcome != "failed" || sequence != "begin,validate,finish" {
					t.Fatalf("partial lifecycle %s/%s", sequence, outcome)
				}
			})
		}
	}
}

func TestPacerRetryRetainsAttemptPolicy(t *testing.T) {
	s, p, host := newPacerTestServer(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(b, "messages.0.content.0.cache_control.ttl").String() != "5m" {
			t.Error("retry lost selected TTL")
		}
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pacerTestJSON)
	}))
	t.Cleanup(upstream.Close)
	m := s.handlers.AuthManager
	m.RegisterExecutor(executor.NewClaudeExecutor(s.cfg))
	m.SetRetryConfig(1, 0, 1)
	_, err := m.Register(context.Background(), &auth.Auth{ID: "pacer-dummy", Provider: "claude", Attributes: map[string]string{"api_key": "dummy", "base_url": upstream.URL, "cloak_mode": "never"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("pacer-dummy", "claude", []*registry.ModelInfo{{ID: pacerTestModel, Object: "model", Type: "claude", OwnedBy: "anthropic"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("pacer-dummy") })
	host.RegisterUsagePlugins()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}]}`))
	pacerHeaders(r)
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, r)
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("retry status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	var records []pluginapi.UsageRecord
	for range 2 {
		select {
		case rec := <-p.records:
			records = append(records, rec)
		case <-time.After(3 * time.Second):
			t.Fatal("missing retry usage")
		}
	}
	if records[0].RequestID == records[1].RequestID || records[0].LifecycleRequestID != records[1].LifecycleRequestID || !records[0].Failed || records[1].Failed {
		t.Fatal("retry attempt identity/outcome lost")
	}
	for _, rec := range records {
		if rec.CachePolicy.Selected.TTL != "5m" || rec.CachePolicy.Selected.ContextEpoch != "epoch" || !rec.CachePolicy.Dispatched {
			t.Fatal("retry policy lost")
		}
	}
}
