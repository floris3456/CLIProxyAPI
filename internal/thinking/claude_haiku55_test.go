package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/claude"
	clauderesponses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	"github.com/tidwall/gjson"
)

func TestClaudeHaiku55ResponsesThinkingPipeline(t *testing.T) {
	const model = "claude-haiku-5-5"
	for _, registered := range []bool{false, true} {
		name := "static lookup"
		if registered {
			name = "registered catalogue"
		}
		t.Run(name, func(t *testing.T) {
			if registered {
				r := registry.GetGlobalRegistry()
				r.RegisterClient("haiku55-thinking-test", "claude", registry.GetClaudeModels())
				t.Cleanup(func() { r.UnregisterClient("haiku55-thinking-test") })
			}
			for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "auto", "none"} {
				t.Run(effort, func(t *testing.T) {
					source := []byte(`{"model":"claude-haiku-5-5","input":"hello","reasoning":{"effort":"` + effort + `"}}`)
					translated, errConvert := clauderesponses.ConvertOpenAIResponsesRequestToClaude(model, source, false)
					if errConvert != nil {
						t.Fatalf("ConvertOpenAIResponsesRequestToClaude() error = %v", errConvert)
					}
					wantType, wantEffort := "adaptive", effort
					if effort == "none" {
						wantType = "disabled"
					}
					if effort == "none" || effort == "auto" {
						wantEffort = ""
					}
					if got := gjson.GetBytes(translated, "thinking.type").String(); got != wantType {
						t.Errorf("translated thinking.type = %q, want %q", got, wantType)
					}
					if gjson.GetBytes(translated, "thinking.budget_tokens").Exists() {
						t.Error("translator selected legacy manual thinking for Haiku 5.5")
					}
					out, err := thinking.ApplyThinkingWithSourceAndSummary(translated, source, model, "openai-response", "claude", "claude", thinking.SummaryConfig{})
					if err != nil {
						t.Fatal(err)
					}
					if got := gjson.GetBytes(out, "thinking.type").String(); got != wantType {
						t.Errorf("final thinking.type = %q, want %q", got, wantType)
					}
					if got := gjson.GetBytes(out, "output_config.effort"); got.String() != wantEffort || (wantEffort == "" && got.Exists()) {
						t.Errorf("final effort = %s, want %q (absent for auto/none)", got.Raw, wantEffort)
					}
					if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
						t.Error("final payload contains a legacy manual budget")
					}
				})
			}
			t.Run("native disabled clears stale effort and budget", func(t *testing.T) {
				body := []byte(`{"thinking":{"type":"disabled","budget_tokens":8192},"output_config":{"effort":"high"}}`)
				out, err := thinking.ApplyThinking(body, model, "claude", "claude", "claude")
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(out, "thinking.type").String() != "disabled" || gjson.GetBytes(out, "thinking.budget_tokens").Exists() || gjson.GetBytes(out, "output_config.effort").Exists() {
					t.Fatalf("unexpected disabled payload: %s", out)
				}
			})
		})
	}

	t.Run("legacy Haiku still uses manual thinking", func(t *testing.T) {
		const legacy = "claude-haiku-4-5-20251001"
		source := []byte(`{"input":"hello","reasoning":{"effort":"high"}}`)
		translated, errConvert := clauderesponses.ConvertOpenAIResponsesRequestToClaude(legacy, source, false)
		if errConvert != nil {
			t.Fatalf("ConvertOpenAIResponsesRequestToClaude() error = %v", errConvert)
		}
		out, err := thinking.ApplyThinkingWithSourceAndSummary(translated, source, legacy, "openai-response", "claude", "claude", thinking.SummaryConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(out, "thinking.type").String() != "enabled" || gjson.GetBytes(out, "thinking.budget_tokens").Int() <= 0 || gjson.GetBytes(out, "output_config.effort").Exists() {
			t.Fatalf("unexpected legacy payload: %s", out)
		}
	})
}
