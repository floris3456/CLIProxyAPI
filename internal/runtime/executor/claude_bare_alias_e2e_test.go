package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// TestClaudeCloakedResponsesStreamRestoresBareToolAlias drives a Responses-API
// client (like OpenCode) through a cloaked Claude OAuth credential. The fake
// upstream answers with a tool call whose name lacks the "mcp__<server>__"
// prefix; the client must still receive its own tool name.
func TestClaudeCloakedResponsesStreamRestoresBareToolAlias(t *testing.T) {
	var upstreamTools []string
	var bare string
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, errRead := io.ReadAll(req.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		writeAlias := ""
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			name := tool.Get("name").String()
			upstreamTools = append(upstreamTools, name)
			if strings.HasSuffix(name, "_write") {
				writeAlias = name
			}
		}
		if writeAlias == "" {
			t.Fatalf("upstream tools %v have no write alias", upstreamTools)
		}
		bare = bareMCPAlias(t, writeAlias)
		sse := "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"` + bare + `","input":{}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\",\"content\":\"hi\"}"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		header := make(http.Header)
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(sse)), Request: req}, nil
	})

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(transport))
	auth := &cliproxyauth.Auth{
		ID:         "test-cloaked-bare-alias",
		Attributes: map[string]string{"api_key": "sk-ant-oat-test-bare-alias-key"},
		Metadata: map[string]any{
			"account_uuid": "11111111-2222-4333-8444-555555555555",
			claudeauth.ClaudeDeviceIDsMetadataKey: []string{
				"0000000000000000000000000000000000000000000000000000000000000001",
			},
		},
	}
	payload := []byte(`{
		"model": "claude-opus-5",
		"stream": true,
		"input": [{"role": "user", "content": [{"type": "input_text", "text": "Write a.txt"}]}],
		"tools": [
			{"type": "function", "name": "read", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}},
			{"type": "function", "name": "write", "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}}}}
		]
	}`)

	result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var out [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		out = append(out, chunk.Payload)
	}
	joined := bytes.Join(out, []byte("\n"))

	for _, name := range upstreamTools {
		if !strings.HasPrefix(name, "mcp__") {
			t.Fatalf("upstream tool %q is not cloaked; precondition broken (tools %v)", name, upstreamTools)
		}
	}
	if !bytes.Contains(joined, []byte(`"name":"write"`)) {
		t.Fatalf("client stream does not call tool \"write\"; got:\n%s", joined)
	}
	if bytes.Contains(joined, []byte(bare)) {
		t.Fatalf("client stream still contains bare alias %q:\n%s", bare, joined)
	}
}
