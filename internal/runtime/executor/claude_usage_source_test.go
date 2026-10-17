package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type claudeSourceTransport func(*http.Request) (*http.Response, error)

func (f claudeSourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type claudeSourceCapture struct{ records chan usage.Record }

func (c *claudeSourceCapture) HandleUsage(_ context.Context, r usage.Record) {
	if strings.HasPrefix(r.AuthID, "pacer-source-wire-") {
		c.records <- r
	}
}

type claudeSourceTruncatedBody struct {
	data string
	done bool
}

func (b *claudeSourceTruncatedBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.ErrUnexpectedEOF
	}
	b.done = true
	return copy(p, b.data), io.ErrUnexpectedEOF
}
func (*claudeSourceTruncatedBody) Close() error { return nil }

func TestPacerSourceClaudeBuiltInWireMarkers(t *testing.T) {
	capture := &claudeSourceCapture{records: make(chan usage.Record, 32)}
	sinkName := t.Name()
	usage.RegisterNamedPlugin(sinkName, capture)
	for _, tc := range []struct {
		name                       string
		stream, responses, summary bool
		failure                    string
	}{
		{"messages", false, false, false, ""}, {"messages-stream", true, false, false, ""},
		{"responses", false, true, false, ""}, {"responses-stream", true, true, false, ""},
		{"summary", false, false, true, ""}, {"canceled", false, false, false, "cancel"},
		{"truncated", false, false, false, "truncate"}, {"retry", false, false, false, "retry"},
		{"clean-eof-messages", true, false, false, "incomplete"}, {"clean-eof-responses", true, true, false, "incomplete"},
		{"incomplete-message", false, false, false, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &auth.Auth{ID: "pacer-source-wire-" + tc.name, Index: "source-index-" + tc.name, Provider: "claude", Attributes: map[string]string{"api_key": "dummy-not-a-real-key"}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = usage.WithStream(ctx, tc.stream)
			ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{IsCompaction: tc.summary})
			calls := 0
			transport := claudeSourceTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Scheme != "https" || req.URL.Host != "api.anthropic.com" || req.URL.Path != "/v1/messages" {
					t.Fatalf("unexpected dispatch=%s", req.URL)
				}
				// All network behavior is replaced here. Arbitrary injected transports
				// are deliberately quarantined; they cannot certify a production source.
				scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
				s := usage.DefaultManager().SourceCheckpoint(sinkName, scope)
				if s.Coverage != "unknown" || s.Reason != "delivery_gap" {
					t.Fatalf("custom transport admitted without gap: %+v", s)
				}
				if s.Physical == nil || s.Physical.Coverage != "unknown" || s.Physical.ThroughCut != nil {
					t.Fatalf("custom transport gained physical proof: %+v", s.Physical)
				}
				if tc.failure == "cancel" {
					cancel()
					return nil, context.Canceled
				}
				if tc.failure == "retry" && calls == 1 {
					return nil, errors.New("dummy connection dropped before reply")
				}
				body := `{"id":"msg_source","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`
				payload, _ := io.ReadAll(req.Body)
				if gjson.GetBytes(payload, "stream").Bool() {
					body = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_source\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				if tc.failure == "incomplete" {
					body = strings.ReplaceAll(body, "data: {\"type\":\"message_stop\"}\n\n", "")
					body = strings.ReplaceAll(body, `"stop_reason":"end_turn"`, `"stop_reason":null`)
				}
				var responseBody io.ReadCloser = io.NopCloser(strings.NewReader(body))
				if tc.failure == "truncate" {
					responseBody = &claudeSourceTruncatedBody{data: body}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: responseBody, Request: req}, nil
			})
			ctx = context.WithValue(ctx, "cliproxy.roundtripper", http.RoundTripper(transport))
			e := NewClaudeExecutor(&config.Config{})
			opts := coreexecutor.Options{SourceFormat: translator.FromString("claude"), Stream: tc.stream}
			if tc.responses {
				opts.ResponseFormat = translator.FromString("openai-response")
			}
			req := coreexecutor.Request{Model: "claude-sonnet-4-5", Payload: []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"Summarize this dummy history"}]}`)}
			run := func() error {
				if tc.stream {
					res, err := e.ExecuteStream(ctx, a, req, opts)
					if err != nil {
						return err
					}
					for chunk := range res.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}
				_, err := e.Execute(ctx, a, req, opts)
				return err
			}
			err := run()
			wantRecords := 1
			if tc.failure == "retry" {
				if err == nil {
					t.Fatal("first retry fixture succeeded")
				}
				err = run()
				wantRecords = 2
			}
			if (tc.failure == "cancel" || tc.failure == "truncate") != (err != nil) {
				t.Fatalf("execution error=%v", err)
			}
			ids := map[string]bool{}
			for range wantRecords {
				select {
				case rec := <-capture.records:
					evidence := rec.SourceEvidence
					if evidence == nil || !evidence.Tracked || evidence.Scope.AuthID != a.ID || evidence.Scope.AuthIndex != a.Index || evidence.Scope.Kind != pluginapi.ClaudeHTTPUsageSource || evidence.UpstreamStartedAt == nil || evidence.UpstreamStartedAt.Before(rec.RequestedAt) || evidence.FinalUsageAt.Before(*evidence.UpstreamStartedAt) {
						t.Fatalf("wire source evidence=%+v", evidence)
					}
					if ids[rec.RequestID] {
						t.Fatal("retry reused physical usage identity")
					}
					if evidence.Physical == nil || evidence.Physical.DispatchSequence <= evidence.Physical.AdmissionSequence || evidence.Physical.StartedAt.Before(evidence.RequestedAt) {
						t.Fatalf("physical dispatch observation missing: %+v", evidence.Physical)
					}
					ids[rec.RequestID] = true
					if rec.CachePolicy != nil && rec.CachePolicy.Selected != nil {
						t.Fatal("unmanaged fixture acquired policy")
					}
					if tc.failure == "incomplete" && (evidence.Gap == nil || evidence.Gap.Reason != "incomplete_claude_response") {
						t.Fatalf("clean EOF certified partial usage: %+v", evidence)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("built-in branch did not publish marked usage")
				}
			}
			if calls != wantRecords {
				t.Fatalf("dispatches=%d, want %d", calls, wantRecords)
			}
		})
	}
}
