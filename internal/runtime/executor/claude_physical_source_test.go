package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type physicalExecutorSink struct {
	authID  string
	records chan usage.Record
}

func (s *physicalExecutorSink) HandleUsage(ctx context.Context, r usage.Record) {
	_ = s.HandleUsageAcknowledged(ctx, r)
}
func (s *physicalExecutorSink) HandleUsageAcknowledged(_ context.Context, r usage.Record) error {
	if r.AuthID == s.authID {
		s.records <- r
	}
	return nil
}

type physicalExecutorBody struct {
	io.Reader
	entered, release chan struct{}
	once             sync.Once
}

func (b *physicalExecutorBody) Close() error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

func TestPacerPhysicalBuiltInLifetimeAndCancelManagedUnmanaged(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, canceled := range []bool{false, true} {
				name := "unmanaged"
				if managed {
					name = "managed"
				}
				if stream {
					name += "-stream"
				} else {
					name += "-json"
				}
				if canceled {
					name += "-canceled"
				}
				t.Run(name, func(t *testing.T) {
					m := usage.DefaultManager()
					a := &auth.Auth{ID: t.Name(), Index: name, Provider: "claude", Attributes: map[string]string{"api_key": "dummy-physical-key"}}
					sink := &physicalExecutorSink{authID: a.ID, records: make(chan usage.Record, 1)}
					m.RegisterNamed(t.Name(), sink)
					scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
					ctx, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					body := `{"id":"msg_physical","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`
					if stream {
						body = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_physical\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					responseBody := &physicalExecutorBody{Reader: strings.NewReader(body), entered: make(chan struct{}), release: make(chan struct{})}
					var releaseOnce sync.Once
					release := func() { releaseOnce.Do(func() { close(responseBody.release) }) }
					t.Cleanup(release)
					requestBody := make(chan io.ReadCloser, 1)
					transport := claudeSourceTransport(func(req *http.Request) (*http.Response, error) {
						// Observe at the real boundary before any simulated transport work.
						p := m.SourceCheckpoint(t.Name(), scope).Physical
						if p == nil || p.Outstanding != 1 || p.ThroughCut != nil {
							t.Error("transport entered without an admission guard", p)
						}
						requestBody <- req.Body // deliberately close asynchronously after Finish
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: responseBody, Request: req}, nil
					})
					ctx = context.WithValue(usage.WithStream(ctx, stream), "cliproxy.roundtripper", http.RoundTripper(transport))
					opts := coreexecutor.Options{Stream: stream, SourceFormat: translator.FromString("claude")}
					var operations []string
					if managed {
						opts.CachePolicy = &pluginapi.CachePolicy{Protocol: 1, Scope: pluginapi.CachePolicyScope{CallerScope: strings.Repeat("a", 64), InstallationID: "dummy-install", SessionID: name, ProviderRoute: "claude"}, ContextEpoch: "epoch", Revision: 1, TTL: "5m", Kind: "primary"}
						opts.LifecycleRequestID = "dummy-lifecycle"
						opts.CachePolicyAttempt = func(_ context.Context, r pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error) {
							operations = append(operations, r.Operation)
							return pluginapi.CachePolicyAttemptResponse{Policy: r.Policy, StartedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
						}
					}
					e := NewClaudeExecutor(&config.Config{})
					req := coreexecutor.Request{Model: "claude-sonnet-4-5", Payload: []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"dummy"}]}`)}
					done := make(chan error, 1)
					go func() {
						if !stream {
							_, err := e.Execute(ctx, a, req, opts)
							done <- err
							return
						}
						res, err := e.ExecuteStream(ctx, a, req, opts)
						if err != nil {
							done <- err
							return
						}
						for chunk := range res.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
						done <- err
					}()
					select {
					case <-responseBody.entered:
					case <-time.After(5 * time.Second):
						t.Fatal("producer never reached body close")
					}
					var record usage.Record
					select {
					case record = <-sink.records:
					case <-time.After(5 * time.Second):
						t.Fatal("final usage not delivered before producer close")
					}
					if record.SourceEvidence == nil || record.SourceEvidence.Physical == nil {
						t.Fatal("missing immutable dispatch observation")
					}
					beforeClosure := m.SourceCheckpoint(t.Name(), scope)
					p := beforeClosure.Physical
					if p.Outstanding != 1 || p.LastProducerClosedAt != nil || p.ThroughCut != nil {
						t.Fatal("terminal usage was treated as producer closure", p)
					}
					if canceled {
						cancel()
					}
					release()
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("executor did not finish after body closure")
					}
					p = m.SourceCheckpoint(t.Name(), scope).Physical
					if p.Outstanding != 1 || p.LastProducerClosedAt == nil || p.ThroughCut != nil {
						t.Fatal("Finish lost the asynchronous request-body guard", p)
					}
					select {
					case b := <-requestBody:
						if err := b.Close(); err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("transport request body missing")
					}
					s := m.SourceCheckpoint(t.Name(), scope)
					if s.Physical.Outstanding != 0 || s.Physical.Coverage != "unknown" || s.Physical.GapCount == 0 {
						t.Fatal("custom transport was promoted or never closed", s.Physical)
					}
					if canceled {
						found := false
						for _, gap := range s.Gaps {
							found = found || gap.Reason == "producer_error_or_cancellation"
						}
						// The full executor suite can exhaust the bounded global gap
						// preview before this test. Counts still retain later failures.
						if s.GapCount <= beforeClosure.GapCount || !found && !s.GapsTruncated {
							t.Fatal("late cancellation was erased after final usage", s.Gaps)
						}
					}
					if managed && strings.Join(operations, ",") != "begin,validate,finish" {
						t.Fatal("policy lifecycle changed", operations)
					}
				})
			}
		}
	}
}
