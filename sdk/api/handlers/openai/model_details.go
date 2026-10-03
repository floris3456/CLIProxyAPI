package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// ModelDetailsSchema identifies the /v1/models?details=true document.
const ModelDetailsSchema = "cliproxyapi.model-details/1"

// Reasoning modes reported per model. They describe what CPA does with a
// reasoning effort on this model, not what an upstream might accept:
//   - "levels": CPA validates and forwards exactly these effort levels.
//   - "none": CPA strips reasoning settings (the model has no reasoning control).
//   - "passthrough": CPA has no definition and forwards settings unvalidated
//     (user-defined or plugin models); clients must not invent levels.
const (
	ReasoningLevels      = "levels"
	ReasoningNone        = "none"
	ReasoningPassthrough = "passthrough"
)

// ModelReasoning is the reasoning control CPA applies to a model.
type ModelReasoning struct {
	Mode   string   `json:"mode"`
	Levels []string `json:"levels,omitempty"`
	// Budget bounds are reported for budget-based models; Levels then lists the
	// effort names CPA maps into that range.
	BudgetMin int `json:"budget_min,omitempty"`
	BudgetMax int `json:"budget_max,omitempty"`
}

// ModelDetail is one entry of the details catalogue.
type ModelDetail struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	OwnedBy     string `json:"owned_by,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	// Kind is "chat" (Responses/Chat Completions) or "image" (/v1/images/generations).
	Kind                string         `json:"kind"`
	Providers           []string       `json:"providers"`
	ContextLength       int            `json:"context_length,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	InputModalities     []string       `json:"input_modalities,omitempty"`
	OutputModalities    []string       `json:"output_modalities"`
	Reasoning           ModelReasoning `json:"reasoning"`
	// ServiceTiers lists request service tiers CPA forwards for this model
	// (for example "priority", the Codex Fast mode).
	ServiceTiers []string `json:"service_tiers"`
}

// standardLevels is the order effort levels are reported in.
var standardLevels = []string{"none", "auto", "minimal", "low", "medium", "high", "xhigh", "max"}

func (h *OpenAIAPIHandler) modelDetailsResponse() map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	codexCatalogue := h.codexClientModelsResponse("")
	tiers := codexServiceTiers(codexCatalogue)
	codexContext := codexContextWindows(codexCatalogue)
	models := h.Models()
	details := make([]ModelDetail, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(stringValue(model["id"]))
		if id == "" {
			continue
		}
		providers := modelRegistry.GetModelProviders(id)
		detail := buildModelDetail(id, model, providers, func(provider string) *registry.ModelInfo {
			return registry.LookupModelInfo(id, provider)
		}, func(provider string) bool {
			return thinking.GetProviderApplier(provider) != nil
		})
		detail.ServiceTiers = append([]string{}, tiers[id]...)
		// Codex applies the catalogue's context_window (272k for current GPT models);
		// the generic registry can list a larger raw window (e.g. 921k for gpt-5.6)
		// that Codex does not use by default.
		if window := codexContext[id]; window > 0 && containsString(detail.Providers, "codex") {
			detail.ContextLength = window
		}
		// Image models are served by /v1/images/generations, not chat.
		if isSupportedImagesModel(id) {
			markImageModel(&detail)
		}
		details = append(details, detail)
	}
	sort.Slice(details, func(i, j int) bool { return details[i].ID < details[j].ID })
	encoded, _ := json.Marshal(details)
	sum := sha256.Sum256(encoded)
	return map[string]any{
		"object": "list",
		"schema": ModelDetailsSchema,
		"hash":   hex.EncodeToString(sum[:]),
		"data":   details,
	}
}

func buildModelDetail(id string, model map[string]any, providers []string, lookup func(string) *registry.ModelInfo, hasApplier func(string) bool) ModelDetail {
	detail := ModelDetail{
		ID:               id,
		Object:           "model",
		OwnedBy:          stringValue(model["owned_by"]),
		DisplayName:      stringValue(model["display_name"]),
		Kind:             "chat",
		Providers:        append([]string{}, providers...),
		ContextLength:    intValue(model["context_length"]),
		OutputModalities: []string{"text"},
		ServiceTiers:     []string{},
	}
	if v := intValue(model["max_context_length"]); v > 0 {
		detail.ContextLength = v
	}
	detail.MaxCompletionTokens = intValue(model["max_completion_tokens"])
	sort.Strings(detail.Providers)

	infos := make([]*registry.ModelInfo, 0, len(providers))
	for _, provider := range detail.Providers {
		if info := lookup(provider); info != nil {
			infos = append(infos, info)
		}
	}
	if len(infos) == 0 {
		if info := lookup(""); info != nil {
			infos = append(infos, info)
		}
	}
	for _, info := range infos {
		if detail.DisplayName == "" && info.DisplayName != "" {
			detail.DisplayName = info.DisplayName
		}
		if detail.ContextLength <= 0 {
			if info.MaxContextLength > 0 {
				detail.ContextLength = info.MaxContextLength
			} else if info.ContextLength > 0 {
				detail.ContextLength = info.ContextLength
			}
		}
		if detail.MaxCompletionTokens <= 0 && info.MaxCompletionTokens > 0 {
			detail.MaxCompletionTokens = info.MaxCompletionTokens
		}
		if info.Type == registry.OpenAIImageModelType {
			markImageModel(&detail)
		}
		if len(detail.InputModalities) == 0 && len(info.SupportedInputModalities) > 0 {
			detail.InputModalities = normalizeModalities(info.SupportedInputModalities)
		}
		if len(info.SupportedOutputModalities) > 0 && detail.Kind != "image" {
			if out := normalizeModalities(info.SupportedOutputModalities); len(out) > 0 {
				detail.OutputModalities = out
			}
		}
	}
	if len(detail.InputModalities) == 0 {
		detail.InputModalities = []string{"text"}
	}
	if detail.Kind == "image" {
		detail.Reasoning = ModelReasoning{Mode: ReasoningNone}
	} else {
		detail.Reasoning = resolveReasoning(detail.Providers, lookup, hasApplier)
	}
	return detail
}

// resolveReasoning mirrors thinking.ApplyThinking's per-provider decision:
// user-defined or unknown models pass through, models without thinking support
// are stripped when CPA has an applier for the provider, otherwise levels are
// validated. Across providers the constrained routes are intersected, because a
// request may be served by any of them; passthrough routes do not widen it.
func resolveReasoning(providers []string, lookup func(string) *registry.ModelInfo, hasApplier func(string) bool) ModelReasoning {
	var constrained [][]string
	budgetMin, budgetMax := 0, 0
	if len(providers) == 0 {
		providers = []string{""}
	}
	for _, provider := range providers {
		info := lookup(provider)
		if info == nil || info.UserDefined {
			continue
		}
		if info.Thinking == nil {
			if provider != "" && hasApplier(provider) {
				constrained = append(constrained, []string{})
			}
			continue
		}
		levels := thinkingLevels(info.Thinking)
		if len(info.Thinking.Levels) == 0 {
			if budgetMin == 0 || info.Thinking.Min > budgetMin {
				budgetMin = info.Thinking.Min
			}
			if budgetMax == 0 || (info.Thinking.Max > 0 && info.Thinking.Max < budgetMax) {
				budgetMax = info.Thinking.Max
			}
		}
		constrained = append(constrained, levels)
	}
	if len(constrained) == 0 {
		return ModelReasoning{Mode: ReasoningPassthrough}
	}
	levels := constrained[0]
	for _, other := range constrained[1:] {
		levels = intersectLevels(levels, other)
	}
	if len(levels) == 0 {
		return ModelReasoning{Mode: ReasoningNone}
	}
	return ModelReasoning{Mode: ReasoningLevels, Levels: levels, BudgetMin: budgetMin, BudgetMax: budgetMax}
}

// thinkingLevels lists the effort names CPA accepts for a thinking definition:
// explicit levels as defined, or the standard levels whose mapped budget lies in
// the model's budget range.
func thinkingLevels(support *registry.ThinkingSupport) []string {
	if len(support.Levels) > 0 {
		out := make([]string, 0, len(support.Levels))
		for _, level := range support.Levels {
			if l := strings.ToLower(strings.TrimSpace(level)); l != "" && !containsString(out, l) {
				out = append(out, l)
			}
		}
		return out
	}
	out := []string{}
	for _, level := range standardLevels {
		budget, ok := thinking.ConvertLevelToBudget(level)
		if !ok {
			continue
		}
		switch {
		case budget == 0:
			if support.ZeroAllowed {
				out = append(out, level)
			}
		case budget < 0:
			if support.DynamicAllowed {
				out = append(out, level)
			}
		case level == "max":
			// "max" maps to the top of the range for budget models.
			if support.Max > 0 {
				out = append(out, level)
			}
		case (support.Min <= 0 || budget >= support.Min) && (support.Max <= 0 || budget <= support.Max):
			out = append(out, level)
		}
	}
	return out
}

func markImageModel(detail *ModelDetail) {
	detail.Kind = "image"
	detail.OutputModalities = []string{"image"}
	detail.Reasoning = ModelReasoning{Mode: ReasoningNone}
}

func intersectLevels(a, b []string) []string {
	out := []string{}
	for _, level := range a {
		if containsString(b, level) {
			out = append(out, level)
		}
	}
	return out
}

func codexServiceTiers(response map[string]any) map[string][]string {
	out := map[string][]string{}
	for _, model := range codexModels(response) {
		slug := stringValue(model["slug"])
		if slug == "" {
			continue
		}
		var ids []string
		switch tiers := model["service_tiers"].(type) {
		case []any:
			for _, tier := range tiers {
				if m, ok := tier.(map[string]any); ok {
					if id := stringValue(m["id"]); id != "" && !containsString(ids, id) {
						ids = append(ids, id)
					}
				}
			}
		case []map[string]any:
			for _, m := range tiers {
				if id := stringValue(m["id"]); id != "" && !containsString(ids, id) {
					ids = append(ids, id)
				}
			}
		}
		if len(ids) > 0 {
			out[slug] = ids
		}
	}
	return out
}

// codexContextWindows maps Codex catalogue slugs to their default context_window
// (not max_context_window, which is an opt-in extension).
func codexContextWindows(response map[string]any) map[string]int {
	out := map[string]int{}
	for _, model := range codexModels(response) {
		if slug := stringValue(model["slug"]); slug != "" {
			if window := intValue(model["context_window"]); window > 0 {
				out[slug] = window
			}
		}
	}
	return out
}

func codexModels(response map[string]any) []map[string]any {
	models, _ := response["models"].([]map[string]any)
	if models == nil {
		if raw, ok := response["models"].([]any); ok {
			for _, item := range raw {
				if m, ok := item.(map[string]any); ok {
					models = append(models, m)
				}
			}
		}
	}
	return models
}

func normalizeModalities(values []string) []string {
	out := []string{}
	for _, value := range values {
		v := strings.ToLower(strings.TrimSpace(value))
		if v != "" && !containsString(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
