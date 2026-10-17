package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type claudeTierRPC struct {
	authID string
	raw    chan []byte
}

func (c *claudeTierRPC) Shutdown() {}
func (c *claudeTierRPC) Call(_ context.Context, method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister:
		return marshalRPCResult(rpcRegistration{SchemaVersion: 7, Capabilities: rpcCapabilities{UsagePlugin: true}})
	case pluginabi.MethodUsageHandle:
		var r pluginapi.UsageRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if r.AuthID == c.authID {
			c.raw <- append([]byte(nil), raw...)
		}
	}
	return marshalRPCResult(rpcEmptyResponse{})
}

type claudeTierTransport func(*http.Request) (*http.Response, error)

func (f claudeTierTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClaudeResponseTierActualExecutorNativeUsageBridge(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"standard", "priority", "missing", "invalid", "conflicting"} {
			if !stream && mode == "conflicting" {
				continue
			}
			t.Run(fmt.Sprintf("%v/%s", stream, mode), func(t *testing.T) {
				h := New()
				defer h.ShutdownAll()
				client := &claudeTierRPC{authID: t.Name(), raw: make(chan []byte, 1)}
				p, err := registerRPCPlugin(context.Background(), h, t.Name(), client, pluginabi.MethodPluginRegister, nil)
				if err != nil {
					t.Fatal(err)
				}
				h.RegisterPluginForTest(t.Name(), p)
				h.RegisterUsagePlugins()
				tier, want := "", mode
				switch mode {
				case "missing":
					want = ""
				case "invalid":
					tier = `,"service_tier":false`
					want = ""
				case "conflicting":
					tier = `,"service_tier":"standard"`
					want = ""
				default:
					tier = `,"service_tier":"` + mode + `"`
				}
				counters := `"input_tokens":10,"cache_read_input_tokens":500,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}`
				response := `{"id":"msg_tier","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":"end_turn","usage":{` + counters + `,"output_tokens":4` + tier + `}}`
				if stream {
					lastTier := ""
					if mode == "conflicting" {
						lastTier = `,"service_tier":"priority"`
					}
					response = "data: " + `{"type":"message_start","message":{"id":"msg_tier","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{` + counters + `,"output_tokens":0` + tier + `}}}` + "\n\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4` + lastTier + `}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				transport := claudeTierTransport(func(req *http.Request) (*http.Response, error) {
					if req.Body != nil {
						if err := req.Body.Close(); err != nil {
							return nil, err
						}
					}
					header := http.Header{"Content-Type": {"application/json"}}
					if stream {
						header.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				})
				ctx := context.WithValue(usage.WithServiceTier(usage.WithStream(context.Background(), stream), "auto"), "cliproxy.roundtripper", http.RoundTripper(transport))
				a := &coreauth.Auth{ID: t.Name(), Index: "dummy-index", Provider: "claude", Attributes: map[string]string{"api_key": "dummy-tier-key"}}
				e := runtimeexecutor.NewClaudeExecutor(&config.Config{})
				req := coreexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(`{"model":"claude-opus-5-5","service_tier":"auto","max_tokens":16,"messages":[{"role":"user","content":"dummy"}]}`)}
				opts := coreexecutor.Options{Stream: stream, SourceFormat: translator.FromString("claude")}
				if stream {
					result, e := e.ExecuteStream(ctx, a, req, opts)
					if e != nil {
						t.Fatal(e)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if _, err = e.Execute(ctx, a, req, opts); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case raw := <-client.raw:
					var r pluginapi.UsageRecord
					if err = json.Unmarshal(raw, &r); err != nil {
						t.Fatal(err)
					}
					if r.ServiceTier != "auto" || r.ResponseServiceTier != want || r.Failed {
						t.Fatalf("native SDK callback lost assigned tier: requested=%q assigned=%q want=%q failed=%v", r.ServiceTier, r.ResponseServiceTier, want, r.Failed)
					}
					if r.AccountingVersion != 2 || r.TokenBreakdown.TotalTokens != 614 || r.TokenBreakdown.Input.CacheReadTokens != 500 || r.Detail.CacheCreation == nil || *r.Detail.CacheCreation.Ephemeral5mInputTokens != 25 || *r.Detail.CacheCreation.Ephemeral1hInputTokens != 75 {
						t.Fatalf("native callback accounting changed: %+v", r)
					}
					t.Logf("requested=%s assigned=%q total=%d split=25/75", r.ServiceTier, r.ResponseServiceTier, r.TokenBreakdown.TotalTokens)
				case <-time.After(5 * time.Second):
					t.Fatal("missing native usage RPC callback")
				}
			})
		}
	}
}
