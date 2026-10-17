package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// An OpenAI Responses client (OpenCode) talking to a cloaked Claude OAuth
// credential continues a long chat across local midnight. The client appends a
// developer item with the new date, then the next tool turn. The upstream body
// before the update must stay a prefix of the body after it: the Claude Code
// system blocks, the currentDate reminder in the first user turn and every
// earlier message. Otherwise Anthropic re-reads the whole history uncached.
func TestClaudeResponsesLateDeveloperUpdateAcrossMidnightKeepsCachePrefix(t *testing.T) {
	var bodies [][]byte
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		bodies = append(bodies, body)
		// Responses clients are served from an upstream stream.
		response := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_%d\",\"type\":\"message\",\"model\":\"claude-opus-5-5\",\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"OK\"}]}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", len(bodies))
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		header.Set("request-id", fmt.Sprintf("req_upstream_%d", len(bodies)))
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	})

	amsterdam, errLocation := time.LoadLocation("Europe/Amsterdam")
	if errLocation != nil {
		t.Skipf("timezone data unavailable: %v", errLocation)
	}
	now := time.Date(2026, 10, 2, 23, 59, 58, 0, amsterdam)
	previousClock := claudeCodeCurrentTimeFunc
	claudeCodeCurrentTimeFunc = func(*config.Config, *cliproxyauth.Auth) time.Time { return now }
	t.Cleanup(func() { claudeCodeCurrentTimeFunc = previousClock })

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	auth := &cliproxyauth.Auth{
		ID:         "test-cloaked-responses-midnight",
		Attributes: map[string]string{"api_key": "sk-ant-oat-test-oauth-key-midnight"},
		Metadata: map[string]any{
			"account_uuid": "11111111-2222-4333-8444-555555555555",
			claudeauth.ClaudeDeviceIDsMetadataKey: []string{
				"0000000000000000000000000000000000000000000000000000000000000001",
			},
		},
	}
	exec := NewClaudeExecutor(&config.Config{})
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}

	history := `
		{"type":"message","role":"developer","content":"You are a coding agent. Today's date: Fri Oct 02 2026"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the failing test"}]},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"go test\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"FAIL"}`
	tools := `"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`
	before := `{"model":"claude-opus-5-5","prompt_cache_key":"f019dc8fbffe8xwRzhGCO2LSfG",` + tools + `,"input":[` + history + `]}`
	after := `{"model":"claude-opus-5-5","prompt_cache_key":"f019dc8fbffe8xwRzhGCO2LSfG",` + tools + `,"input":[` + history + `,
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking at the failure"}]},
		{"type":"function_call","call_id":"call_2","name":"shell","arguments":"{\"cmd\":\"cat x_test.go\"}"},
		{"type":"function_call_output","call_id":"call_2","output":"package x"},
		{"type":"message","role":"developer","content":"Today's date is now: Sat Oct 03 2026"}]}`

	if _, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(before)}, options); err != nil {
		t.Fatalf("request before midnight failed: %v", err)
	}
	now = time.Date(2026, 10, 3, 0, 0, 17, 0, amsterdam)
	if _, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: []byte(after)}, options); err != nil {
		t.Fatalf("request after midnight failed: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("captured %d upstream bodies, want 2", len(bodies))
	}

	first, second := gjson.ParseBytes(bodies[0]), gjson.ParseBytes(bodies[1])
	// system[0] is the per-request billing header, which Anthropic excludes from
	// the cached prompt; every other system block is part of the cache prefix.
	firstSystem, secondSystem := first.Get("system").Array(), second.Get("system").Array()
	if len(firstSystem) != len(secondSystem) {
		t.Fatalf("system block count changed: %s -> %s", first.Get("system").Raw, second.Get("system").Raw)
	}
	for idx := 1; idx < len(firstSystem); idx++ {
		if firstSystem[idx].Raw != secondSystem[idx].Raw {
			t.Fatalf("system[%d] changed:\nbefore: %s\nafter:  %s", idx, firstSystem[idx].Raw, secondSystem[idx].Raw)
		}
	}

	// Cache breakpoints legitimately move to the newest turn, so compare content
	// without cache_control markers.
	firstMessages, secondMessages := first.Get("messages").Array(), second.Get("messages").Array()
	if len(secondMessages) <= len(firstMessages) {
		t.Fatalf("later request has %d messages, want more than %d", len(secondMessages), len(firstMessages))
	}
	for idx := range firstMessages {
		if a, b := withoutCacheControl(firstMessages[idx].Raw), withoutCacheControl(secondMessages[idx].Raw); a != b {
			t.Fatalf("messages[%d] changed, so the cached history is invalidated:\nbefore: %s\nafter:  %s", idx, a, b)
		}
	}
	if !strings.Contains(firstMessages[0].Raw, "Today's date is 2026-10-02.") {
		t.Fatalf("first user turn lacks the pinned currentDate reminder: %s", firstMessages[0].Raw)
	}

	last := secondMessages[len(secondMessages)-1]
	if last.Get("role").String() != "system" || !strings.Contains(last.Raw, "Today's date is now: Sat Oct 03 2026") {
		t.Fatalf("date update must be appended after the tool_result turn as role=system, got %s", second.Get("messages").Raw)
	}
}

// Legacy Claude models have no mid-conversation system slot, so a translated
// request keeps the previous top-level placement for them. Native Claude callers
// own their wire and keep being validated by validateClaudeMidSystemMessageModel.
func TestTranslatedMidSystemNeedsTopLevel(t *testing.T) {
	positional := []byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"update"}]}]}`)
	if !translatedMidSystemNeedsTopLevel(sdktranslator.FormatOpenAIResponse, positional) {
		t.Fatal("legacy model with a translated role=system message must fold to top-level")
	}
	if translatedMidSystemNeedsTopLevel(sdktranslator.FormatClaude, positional) {
		t.Fatal("native Claude requests must not be rewritten")
	}
	modern, _ := sjson.SetBytes(positional, "model", "claude-opus-5-5")
	if translatedMidSystemNeedsTopLevel(sdktranslator.FormatOpenAIResponse, modern) {
		t.Fatal("modern models keep the positional role=system message")
	}
	folded := rebuildMidSystemMessagesToTopLevel(positional)
	if got := gjson.GetBytes(folded, "system.0.text").String(); got != "update" {
		t.Fatalf("folded system = %s", gjson.GetBytes(folded, "system").Raw)
	}
	if got := gjson.GetBytes(folded, "messages.#").Int(); got != 1 {
		t.Fatalf("folded messages = %s", gjson.GetBytes(folded, "messages").Raw)
	}
}

func withoutCacheControl(raw string) string {
	var out strings.Builder
	parsed := gjson.Parse(raw)
	var walk func(value gjson.Result)
	walk = func(value gjson.Result) {
		switch {
		case value.IsObject():
			out.WriteByte('{')
			first := true
			value.ForEach(func(key, item gjson.Result) bool {
				if key.String() == "cache_control" {
					return true
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				out.WriteString(key.Raw)
				out.WriteByte(':')
				walk(item)
				return true
			})
			out.WriteByte('}')
		case value.IsArray():
			out.WriteByte('[')
			for idx, item := range value.Array() {
				if idx > 0 {
					out.WriteByte(',')
				}
				walk(item)
			}
			out.WriteByte(']')
		default:
			out.WriteString(value.Raw)
		}
	}
	walk(parsed)
	return out.String()
}
