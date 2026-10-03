package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type catalogueResponse struct {
	Channels    []string             `json:"channels"`
	ChannelInfo []catalogueChannel   `json:"channel-info"`
	Models      []oauthSettingsModel `json:"models"`
}

func modelConfigFixture(t *testing.T) (*Handler, *coreauth.Manager) {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "mc-codex", Provider: "codex", Metadata: map[string]any{"excluded_models": []any{"mc-gpt-old", "mc-image-*"}}},
		{ID: "mc-plugin", Provider: "mc-go", Metadata: map[string]any{"excluded_models": []any{"mc-go/deepseek-pro"}}},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	registry.RecordClientCandidates("mc-codex", []*registry.ModelInfo{
		{ID: "mc-gpt"}, {ID: "mc-gpt-old"}, {ID: "mc-gpt-mini"}, {ID: "mc-image-1", Type: registry.OpenAIImageModelType},
	})
	registry.RecordClientCandidates("mc-plugin", []*registry.ModelInfo{{ID: "mc-go/deepseek-flash"}, {ID: "mc-go/deepseek-pro"}})
	t.Cleanup(func() {
		registry.ForgetClientCandidates("mc-codex")
		registry.ForgetClientCandidates("mc-plugin")
	})
	h := &Handler{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{"codex": {"mc-gpt-mini"}},
			OAuthModelAlias:     map[string][]config.OAuthModelAlias{"mc-go": {{Name: "mc-go/deepseek-flash", Alias: "deepseek-flash-cheap"}}},
			OAuthSettings:       map[string][]config.OAuthModelSetting{"mc-go": {{Name: "deepseek-flash-cheap", DisplayName: "Cheap"}}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name: "Lithos", BaseURL: "https://example.invalid/v1",
				Models:         []config.OpenAICompatibilityModel{{Name: "deepseek-v4.1-flash", Alias: "deepseek-flash-fast"}, {Name: "kimi-k3"}},
				ExcludedModels: []string{"kimi-k3"},
			}},
		},
		configFilePath: writeTestConfigFile(t),
		authManager:    manager,
	}
	return h, manager
}

func getCatalogue(t *testing.T, h *Handler) map[string]oauthSettingsModel {
	t.Helper()
	rec := oauthSettingsRequest(t, h, http.MethodGet, "/v0/management/oauth-settings/models", "", h.GetOAuthSettingsModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalogue status %d: %s", rec.Code, rec.Body.String())
	}
	var resp catalogueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	out := map[string]oauthSettingsModel{}
	for _, m := range resp.Models {
		out[m.Channel+"|"+m.Upstream] = m
	}
	return out
}

func TestModelCatalogueListsDisabledAliasedAndAPIKeyModels(t *testing.T) {
	h, _ := modelConfigFixture(t)
	rec := oauthSettingsRequest(t, h, http.MethodGet, "/v0/management/oauth-settings/models", "", h.GetOAuthSettingsModels)
	var resp catalogueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Join(resp.Channels, ",") != "codex,mc-go,compat:Lithos" {
		t.Fatalf("channels = %v (accounts first, then API-key providers)", resp.Channels)
	}
	m := getCatalogue(t, h)
	if !m["codex|mc-gpt"].Enabled {
		t.Fatal("mc-gpt should be enabled")
	}
	if got := m["codex|mc-gpt-mini"]; got.Enabled || got.DisabledBy == nil || got.DisabledBy.Scope != "channel" || got.DisabledBy.Pattern != "" {
		t.Fatalf("mc-gpt-mini = %+v, want disabled by the channel list", got)
	}
	if got := m["codex|mc-gpt-old"]; got.Enabled || got.DisabledBy == nil || got.DisabledBy.Scope != "account" || got.DisabledBy.Accounts != 1 {
		t.Fatalf("mc-gpt-old = %+v, want disabled by the credential", got)
	}
	if got := m["codex|mc-image-1"]; got.Enabled || got.DisabledBy.Pattern != "mc-image-*" || got.Kind != "image" {
		t.Fatalf("mc-image-1 = %+v, want disabled by wildcard", got)
	}
	flash := m["mc-go|mc-go/deepseek-flash"]
	if flash.ID != "deepseek-flash-cheap" || flash.Alias == nil || flash.Alias.Name != "deepseek-flash-cheap" || flash.Setting == nil || flash.Setting.DisplayName != "Cheap" {
		t.Fatalf("flash = %+v, want alias and the setting keyed by the alias", flash)
	}
	fast := m["compat:Lithos|deepseek-v4.1-flash"]
	if fast.Source != sourceAPIKey || fast.ID != "deepseek-flash-fast" || !fast.Enabled || fast.Supports.MaxOutputTokens || fast.Supports.KeepOriginal {
		t.Fatalf("lithos flash = %+v", fast)
	}
	if got := m["compat:Lithos|kimi-k3"]; got.Enabled || got.DisabledBy == nil || got.DisabledBy.Scope != "provider" {
		t.Fatalf("lithos kimi = %+v, want disabled by the provider list", got)
	}
}

func patchModel(t *testing.T, h *Handler, body string) (int, string) {
	t.Helper()
	rec := oauthSettingsRequest(t, h, http.MethodPatch, "/v0/management/model-config", body, h.PatchModelConfig)
	return rec.Code, rec.Body.String()
}

func TestPatchModelConfigDisableAndEnableAccountModels(t *testing.T) {
	h, manager := modelConfigFixture(t)
	if code, body := patchModel(t, h, `{"channel":"codex","model":"mc-gpt","enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable: %d %s", code, body)
	}
	if got := h.cfg.OAuthExcludedModels["codex"]; strings.Join(got, ",") != "mc-gpt-mini,mc-gpt" {
		t.Fatalf("codex exclusions = %v", got)
	}
	if code, body := patchModel(t, h, `{"channel":"codex","model":"mc-gpt-mini","enabled":true}`); code != http.StatusOK {
		t.Fatalf("enable channel-disabled: %d %s", code, body)
	}
	if got := h.cfg.OAuthExcludedModels["codex"]; strings.Join(got, ",") != "mc-gpt" {
		t.Fatalf("codex exclusions after enable = %v", got)
	}
	// Disabled only by the credential's own list: enabling edits the credential.
	if code, body := patchModel(t, h, `{"channel":"codex","model":"mc-gpt-old","enabled":true}`); code != http.StatusOK {
		t.Fatalf("enable account-disabled: %d %s", code, body)
	}
	auth, _ := manager.GetByID("mc-codex")
	if got := authExcludedModels(auth); strings.Join(got, ",") != "mc-image-*" {
		t.Fatalf("credential exclusions = %v, want only the wildcard left", got)
	}
	if got := auth.Attributes["excluded_models"]; got != "mc-gpt,mc-image-*" {
		t.Fatalf("merged exclusion attribute = %q, want global + per-account", got)
	}
	// A wildcard cannot be lifted for one model.
	code, body := patchModel(t, h, `{"channel":"codex","model":"mc-image-1","enabled":true}`)
	if code != http.StatusConflict || !strings.Contains(body, "mc-image-*") {
		t.Fatalf("enable wildcard-disabled: %d %s", code, body)
	}
	if m := getCatalogue(t, h); m["codex|mc-gpt"].Enabled || !m["codex|mc-gpt-old"].Enabled || !m["codex|mc-gpt-mini"].Enabled {
		t.Fatalf("catalogue after toggles = %+v", m)
	}
}

func TestPatchModelConfigAliasSettingsAndCollisions(t *testing.T) {
	h, _ := modelConfigFixture(t)
	// Rename the alias; the setting keyed by the old alias follows the model.
	code, body := patchModel(t, h, `{"channel":"mc-go","model":"mc-go/deepseek-flash","alias":{"name":"deepseek-flash-go","keep-original":true}}`)
	if code != http.StatusOK {
		t.Fatalf("alias: %d %s", code, body)
	}
	if got := h.cfg.OAuthModelAlias["mc-go"]; len(got) != 1 || got[0].Alias != "deepseek-flash-go" || !got[0].Fork || got[0].Name != "mc-go/deepseek-flash" {
		t.Fatalf("aliases = %+v", got)
	}
	if got := h.cfg.OAuthSettings["mc-go"]; len(got) != 1 || got[0].Name != "mc-go/deepseek-flash" || got[0].DisplayName != "Cheap" {
		t.Fatalf("settings = %+v, want re-keyed by the upstream name", got)
	}
	m := getCatalogue(t, h)
	if got := m["mc-go|mc-go/deepseek-flash"]; strings.Join(got.Exposed, ",") != "deepseek-flash-go,mc-go/deepseek-flash" || got.Setting == nil {
		t.Fatalf("flash = %+v", got)
	}
	// Taking another model's name is refused.
	code, body = patchModel(t, h, `{"channel":"codex","model":"mc-gpt","alias":{"name":"deepseek-flash-fast"}}`)
	if code != http.StatusConflict || !strings.Contains(body, "compat:Lithos") {
		t.Fatalf("collision: %d %s", code, body)
	}
	// Alias, enabled state and settings in one write.
	code, body = patchModel(t, h, `{"channel":"codex","model":"mc-gpt","alias":{"name":"gpt-main"},"enabled":false,"setting":{"max-output-tokens":64000,"thinking-levels":["high","low"]}}`)
	if code != http.StatusOK {
		t.Fatalf("combined: %d %s", code, body)
	}
	if got := h.cfg.OAuthSettings["codex"]; len(got) != 1 || got[0].Name != "mc-gpt" || got[0].MaxOutputTokens != 64000 || strings.Join(got[0].ThinkingLevels, ",") != "low,high" {
		t.Fatalf("codex settings = %+v", got)
	}
	// Removing the alias and clearing settings.
	code, body = patchModel(t, h, `{"channel":"codex","model":"mc-gpt","alias":null,"setting":null}`)
	if code != http.StatusOK {
		t.Fatalf("clear: %d %s", code, body)
	}
	if _, ok := h.cfg.OAuthModelAlias["codex"]; ok {
		t.Fatalf("codex alias not removed: %+v", h.cfg.OAuthModelAlias)
	}
	if _, ok := h.cfg.OAuthSettings["codex"]; ok {
		t.Fatalf("codex settings not removed: %+v", h.cfg.OAuthSettings)
	}
	for _, bad := range []string{
		`{"channel":"codex","model":"absent","enabled":true}`,
		`{"channel":"codex","model":"mc-gpt"}`,
		`{"channel":"codex","model":"mc-gpt","alias":{"name":"a,b"}}`,
		`{"channel":"codex","model":"mc-gpt","setting":{"thinking-levels":["ultra"]}}`,
	} {
		if code, body := patchModel(t, h, bad); code == http.StatusOK {
			t.Fatalf("%s accepted: %s", bad, body)
		}
	}
}

func TestPatchModelConfigAPIKeyProvider(t *testing.T) {
	h, _ := modelConfigFixture(t)
	code, body := patchModel(t, h, `{"channel":"compat:Lithos","model":"kimi-k3","enabled":true,"alias":{"name":"kimi-fast"},"setting":{"display-name":"Kimi (fast)","max-context-length":256000,"thinking-levels":["low","high"]}}`)
	if code != http.StatusOK {
		t.Fatalf("compat patch: %d %s", code, body)
	}
	compat := h.cfg.OpenAICompatibility[0]
	if len(compat.ExcludedModels) != 0 {
		t.Fatalf("excluded = %v", compat.ExcludedModels)
	}
	kimi := compat.Models[1]
	if kimi.Alias != "kimi-fast" || kimi.DisplayName != "Kimi (fast)" || kimi.MaxContextLength != 256000 || kimi.Thinking == nil || strings.Join(kimi.Thinking.Levels, ",") != "low,high" {
		t.Fatalf("kimi = %+v", kimi)
	}
	if code, body := patchModel(t, h, `{"channel":"compat:Lithos","model":"deepseek-v4.1-flash","enabled":false}`); code != http.StatusOK {
		t.Fatalf("compat disable: %d %s", code, body)
	}
	if got := h.cfg.OpenAICompatibility[0].ExcludedModels; strings.Join(got, ",") != "deepseek-v4.1-flash" {
		t.Fatalf("excluded = %v", got)
	}
	if code, _ := patchModel(t, h, `{"channel":"compat:Lithos","model":"kimi-k3","alias":{"name":"x","keep-original":true}}`); code != http.StatusBadRequest {
		t.Fatalf("keep-original on compat: %d, want 400", code)
	}
	if code, _ := patchModel(t, h, `{"channel":"compat:Lithos","model":"kimi-k3","setting":{"max-output-tokens":1000}}`); code != http.StatusBadRequest {
		t.Fatalf("max output on compat: %d, want 400", code)
	}
	if code, body := patchModel(t, h, `{"channel":"compat:Lithos","model":"kimi-k3","setting":null,"alias":null}`); code != http.StatusOK {
		t.Fatalf("compat clear: %d %s", code, body)
	}
	kimi = h.cfg.OpenAICompatibility[0].Models[1]
	if kimi.Alias != "" || kimi.DisplayName != "" || kimi.MaxContextLength != 0 || kimi.Thinking != nil {
		t.Fatalf("kimi after clear = %+v", kimi)
	}
}
