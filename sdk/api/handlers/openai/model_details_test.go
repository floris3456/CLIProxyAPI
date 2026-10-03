package openai

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func lookupFrom(infos map[string]*registry.ModelInfo) func(string) *registry.ModelInfo {
	return func(provider string) *registry.ModelInfo { return infos[provider] }
}

func appliers(names ...string) func(string) bool {
	return func(provider string) bool {
		for _, name := range names {
			if name == provider {
				return true
			}
		}
		return false
	}
}

func TestResolveReasoningMirrorsApplyThinking(t *testing.T) {
	levels := &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}
	cases := []struct {
		name      string
		providers []string
		infos     map[string]*registry.ModelInfo
		want      ModelReasoning
	}{
		{"defined levels", []string{"claude"}, map[string]*registry.ModelInfo{"claude": {Thinking: levels}},
			ModelReasoning{Mode: ReasoningLevels, Levels: []string{"low", "medium", "high", "xhigh", "max"}}},
		{"no thinking on a known provider is stripped", []string{"claude"}, map[string]*registry.ModelInfo{"claude": {}},
			ModelReasoning{Mode: ReasoningNone}},
		{"user-defined passes through", []string{"zen"}, map[string]*registry.ModelInfo{"zen": {UserDefined: true}},
			ModelReasoning{Mode: ReasoningPassthrough}},
		{"plugin provider without applier passes through", []string{"opencode-go"}, map[string]*registry.ModelInfo{"opencode-go": {}},
			ModelReasoning{Mode: ReasoningPassthrough}},
		{"routes intersect", []string{"antigravity", "claude"}, map[string]*registry.ModelInfo{
			"claude":      {Thinking: levels},
			"antigravity": {Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}},
		}, ModelReasoning{Mode: ReasoningLevels, Levels: []string{"low", "medium", "high"}}},
		{"a route without thinking removes reasoning", []string{"antigravity", "claude"}, map[string]*registry.ModelInfo{
			"claude": {Thinking: levels}, "antigravity": {},
		}, ModelReasoning{Mode: ReasoningNone}},
		{"budget model maps standard levels into its range", []string{"gemini"}, map[string]*registry.ModelInfo{
			"gemini": {Thinking: &registry.ThinkingSupport{Min: 128, Max: 32768, ZeroAllowed: false, DynamicAllowed: true}},
		}, ModelReasoning{Mode: ReasoningLevels, Levels: []string{"auto", "minimal", "low", "medium", "high", "xhigh", "max"}, BudgetMin: 128, BudgetMax: 32768}},
	}
	for _, tc := range cases {
		got := resolveReasoning(tc.providers, lookupFrom(tc.infos), appliers("claude", "codex", "antigravity", "gemini"))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v want %+v", tc.name, got, tc.want)
		}
	}
}

func TestBuildModelDetailLimitsKindAndModalities(t *testing.T) {
	infos := map[string]*registry.ModelInfo{
		"codex": {Type: registry.OpenAIImageModelType, DisplayName: "GPT Image 2"},
	}
	d := buildModelDetail("gpt-image-2", map[string]any{"id": "gpt-image-2", "owned_by": "openai"}, []string{"codex"}, lookupFrom(infos), appliers("codex"))
	if d.Kind != "image" || !reflect.DeepEqual(d.OutputModalities, []string{"image"}) || d.DisplayName != "GPT Image 2" {
		t.Fatalf("image model: %+v", d)
	}
	if d.Reasoning.Mode != ReasoningNone {
		t.Fatalf("image model reasoning: %+v", d.Reasoning)
	}
	infos = map[string]*registry.ModelInfo{
		"claude": {ContextLength: 1000000, MaxCompletionTokens: 128000, SupportedInputModalities: []string{"TEXT", "IMAGE"},
			Thinking: &registry.ThinkingSupport{Levels: []string{"low", "max"}}},
	}
	d = buildModelDetail("claude-opus-5-5", map[string]any{"id": "claude-opus-5-5", "context_length": 200000}, []string{"claude"}, lookupFrom(infos), appliers("claude"))
	if d.ContextLength != 200000 || d.MaxCompletionTokens != 128000 || !reflect.DeepEqual(d.InputModalities, []string{"text", "image"}) || d.Kind != "chat" {
		t.Fatalf("chat model: %+v", d)
	}
	d = buildModelDetail("x", map[string]any{"id": "x", "max_context_length": 1048576, "context_length": 1000}, nil, lookupFrom(nil), appliers())
	if d.ContextLength != 1048576 || d.Reasoning.Mode != ReasoningPassthrough || len(d.ServiceTiers) != 0 || d.Providers == nil {
		t.Fatalf("unknown model: %+v", d)
	}
}

func TestCodexServiceTiers(t *testing.T) {
	response := map[string]any{"models": []map[string]any{
		{"slug": "gpt-6-astra", "service_tiers": []any{map[string]any{"id": "priority", "name": "Fast"}}},
		{"slug": "claude-opus-5-5", "service_tiers": []any{}},
	}}
	got := codexServiceTiers(response)
	if !reflect.DeepEqual(got, map[string][]string{"gpt-6-astra": {"priority"}}) {
		t.Fatalf("%v", got)
	}
}

func TestOpenAIModelsDetailsEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("details-claude", "claude", []*registry.ModelInfo{{ID: "details-opus", OwnedBy: "anthropic", ContextLength: 1000000, MaxCompletionTokens: 128000,
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}}})
	reg.RegisterClient("details-zen", "zen", []*registry.ModelInfo{{ID: "details-free", UserDefined: true}})
	reg.RegisterClient("details-codex-image", "codex", []*registry.ModelInfo{{ID: "gpt-image-2"}})
	t.Cleanup(func() {
		reg.UnregisterClient("details-claude")
		reg.UnregisterClient("details-zen")
		reg.UnregisterClient("details-codex-image")
	})
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v1/models?details=true", nil)
	h.OpenAIModels(c)
	var body struct {
		Schema string        `json:"schema"`
		Hash   string        `json:"hash"`
		Data   []ModelDetail `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Schema != ModelDetailsSchema || len(body.Hash) != 64 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	got := map[string]ModelDetail{}
	for _, d := range body.Data {
		got[d.ID] = d
	}
	opus, free := got["details-opus"], got["details-free"]
	if opus.Reasoning.Mode != ReasoningLevels || !reflect.DeepEqual(opus.Reasoning.Levels, []string{"low", "medium", "high", "xhigh", "max"}) || opus.ContextLength != 1000000 || opus.MaxCompletionTokens != 128000 {
		t.Fatalf("opus: %+v", opus)
	}
	if image := got["gpt-image-2"]; image.Kind != "image" || image.Reasoning.Mode != ReasoningNone || !reflect.DeepEqual(image.OutputModalities, []string{"image"}) {
		t.Fatalf("codex image tool model: %+v", image)
	}
	if free.Reasoning.Mode != ReasoningPassthrough {
		t.Fatalf("user-defined: %+v", free)
	}
	// The plain list keeps its four-field shape.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v1/models", nil)
	h.OpenAIModels(c)
	if strings.Contains(w.Body.String(), "reasoning") || !strings.Contains(w.Body.String(), "details-opus") {
		t.Fatalf("plain list changed: %s", w.Body.String())
	}
}
