package cliproxy

import (
	"reflect"
	"strings"
	"testing"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/codex"
	"github.com/tidwall/gjson"
)

func TestApplyOAuthSettings_OutputDisplayNameAndThinkingLevels(t *testing.T) {
	shared := &registry.ThinkingSupport{ZeroAllowed: false, DynamicAllowed: true, Levels: []string{"low", "medium", "high", "xhigh", "max"}}
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"claude": {{Name: "claude-opus-5-5", MaxOutputTokens: 64000, DisplayName: "Opus (CPA)", ThinkingLevels: []string{"low", "medium", "high"}}},
	}}
	models := []*ModelInfo{
		{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", ContextLength: 1000000, MaxCompletionTokens: 128000, Thinking: shared},
		{ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5", MaxCompletionTokens: 128000, Thinking: shared},
	}
	out := applyOAuthSettings(cfg, "claude", "oauth", models)
	opus := out[0]
	if opus.MaxCompletionTokens != 64000 || opus.DisplayName != "Opus (CPA)" || opus.ContextLength != 1000000 {
		t.Fatalf("opus = %+v", opus)
	}
	if !reflect.DeepEqual(opus.Thinking.Levels, []string{"low", "medium", "high"}) || !opus.Thinking.DynamicAllowed || !opus.ExplicitThinking {
		t.Fatalf("opus thinking = %+v explicit=%t", opus.Thinking, opus.ExplicitThinking)
	}
	if len(shared.Levels) != 5 || out[1] != models[1] {
		t.Fatalf("the shared definition or an unconfigured model changed: shared=%v sonnet=%p/%p", shared.Levels, out[1], models[1])
	}
	if opus.MaxContextLength != 0 {
		t.Fatalf("no max-context-length configured, got %d", opus.MaxContextLength)
	}
}

// Configured levels are what CPA enforces, not only what it advertises.
func TestApplyOAuthSettings_ThinkingLevelsAreEnforced(t *testing.T) {
	const id = "oauth-settings-levels-model"
	base := []*ModelInfo{{ID: id, Object: "model", OwnedBy: "openai", Type: "openai",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}}}
	body := []byte(`{"model":"` + id + `","reasoning":{"effort":"max"},"input":[]}`)
	apply := func(models []*ModelInfo) (string, error) {
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("oauth-settings-levels", "codex", models)
		defer reg.UnregisterClient("oauth-settings-levels")
		out, err := thinking.ApplyThinking(body, id, "codex", "codex", "codex")
		return gjson.GetBytes(out, "reasoning.effort").String(), err
	}
	if got, err := apply(base); err != nil || got != "max" {
		t.Fatalf("control: effort = %q, err = %v; want max accepted", got, err)
	}
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex": {{Name: id, ThinkingLevels: []string{"low", "medium", "high"}}},
	}}
	configured := applyOAuthSettings(cfg, "codex", "oauth", base)
	if _, err := apply(configured); err == nil || !strings.Contains(err.Error(), "valid levels: low, medium, high") {
		t.Fatalf("configured low..high: max must be rejected with the configured levels, err = %v", err)
	}
	if got, err := apply2(t, configured, id, "medium"); err != nil || got != "medium" {
		t.Fatalf("configured low..high: medium = %q, err = %v", got, err)
	}
}

func apply2(t *testing.T, models []*ModelInfo, id, level string) (string, error) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("oauth-settings-levels", "codex", models)
	defer reg.UnregisterClient("oauth-settings-levels")
	out, err := thinking.ApplyThinking([]byte(`{"model":"`+id+`","reasoning":{"effort":"`+level+`"},"input":[]}`), id, "codex", "codex", "codex")
	return gjson.GetBytes(out, "reasoning.effort").String(), err
}

func TestApplyOAuthSettings_CodexCatalogCarriesOverrides(t *testing.T) {
	cfg := &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex": {{Name: "gpt-6-sol", MaxOutputTokens: 64000, DisplayName: "Sol", ThinkingLevels: []string{"low", "high"}}},
	}}
	out := applyOAuthSettings(cfg, "codex", "oauth", []*ModelInfo{{ID: "gpt-6-sol", Object: "model", OwnedBy: "openai", Type: "openai",
		DisplayName: "GPT-6 Sol", ContextLength: 272000, MaxCompletionTokens: 128000,
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}}})
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("oauth-settings-catalog", "codex", out)
	t.Cleanup(func() { reg.UnregisterClient("oauth-settings-catalog") })
	resp := codexmodels.BuildResponse(reg.GetAvailableModels("openai"), nil, false)
	models, _ := resp["models"].([]map[string]any)
	for _, m := range models {
		if m["slug"] != "gpt-6-sol" {
			continue
		}
		if m["display_name"] != "Sol" {
			t.Errorf("display_name = %v", m["display_name"])
		}
		if m["max_tokens"] != 64000 {
			t.Errorf("max_tokens = %v", m["max_tokens"])
		}
		var levels []string
		switch raw := m["supported_reasoning_levels"].(type) {
		case []map[string]any:
			for _, l := range raw {
				levels = append(levels, l["effort"].(string))
			}
		case []any:
			for _, l := range raw {
				levels = append(levels, l.(map[string]any)["effort"].(string))
			}
		}
		if !reflect.DeepEqual(levels, []string{"low", "high"}) {
			t.Errorf("supported_reasoning_levels = %v (%T)", levels, m["supported_reasoning_levels"])
		}
		return
	}
	t.Fatal("gpt-6-sol missing from the Codex catalogue")
}
