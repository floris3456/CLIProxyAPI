package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// The model catalog is refreshed from the remote models.json at runtime, and that
// catalog can still advertise zero_allowed=true for Claude models that reject
// thinking.type="disabled". The real model IDs must fall back to the lowest
// adaptive effort even when the registry entry allows an explicit disable.
func TestThinkingE2EClaudeCannotDisableRemoteCatalog(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	uid := fmt.Sprintf("thinking-e2e-claude-cannot-disable-remote-%d", time.Now().UnixNano())

	levels := []string{"low", "medium", "high", "xhigh", "max"}
	reg.RegisterClient(uid, "test", []*registry.ModelInfo{
		{
			ID:                  "claude-opus-5-5",
			Object:              "model",
			Created:             1790035200,
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Opus 5.5",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			// Mirrors the remote catalog, which still reports zero_allowed=true.
			Thinking: &registry.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true, Levels: levels},
		},
		{
			ID:                  "claude-fable-5-1",
			Object:              "model",
			Created:             1788220800,
			OwnedBy:             "anthropic",
			Type:                "claude",
			DisplayName:         "Claude Fable 5.1",
			ContextLength:       1000000,
			MaxCompletionTokens: 128000,
			Thinking:            &registry.ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, Levels: levels},
		},
	})
	defer reg.UnregisterClient(uid)

	lowAdaptive := func(name, from, model, input string) thinkingTestCase {
		return thinkingTestCase{
			name:         name,
			from:         from,
			to:           "claude",
			model:        model,
			inputJSON:    input,
			expectField:  "thinking.type",
			expectValue:  "adaptive",
			expectField2: "output_config.effort",
			expectValue2: "low",
			expectAbsent: []string{"thinking.budget_tokens"},
		}
	}

	cases := []thinkingTestCase{
		lowAdaptive("1", "openai", "claude-opus-5-5",
			`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`),
		lowAdaptive("2", "claude", "claude-opus-5-5",
			`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`),
		lowAdaptive("3", "claude", "claude-opus-5-5(none)",
			`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`),
		lowAdaptive("4", "gemini", "claude-opus-5-5",
			`{"model":"claude-opus-5-5","contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`),
		lowAdaptive("5", "openai", "claude-fable-5-1",
			`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`),
	}

	runThinkingTests(t, cases)
}
