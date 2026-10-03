package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func oauthSettingsRequest(t *testing.T, h *Handler, method, target, body string, fn func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	fn(c)
	return rec
}

func TestPutOAuthSettingsValidatesAndPersists(t *testing.T) {
	path := writeTestConfigFile(t)
	h := &Handler{cfg: &config.Config{}, configFilePath: path}

	rec := oauthSettingsRequest(t, h, http.MethodPut, "/v0/management/oauth-settings",
		`{"Codex":[{"name":"gpt-5.5","max-output-tokens":64000,"thinking-levels":["HIGH","low"]},{"name":"empty-entry"}]}`, h.PutOAuthSettings)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := h.cfg.OAuthSettings["codex"]
	if len(got) != 1 || got[0].Name != "gpt-5.5" || got[0].MaxOutputTokens != 64000 {
		t.Fatalf("settings = %+v, want only the gpt-5.5 entry (entries without overrides dropped)", h.cfg.OAuthSettings)
	}
	if strings.Join(got[0].ThinkingLevels, ",") != "low,high" {
		t.Fatalf("levels = %v, want normalized [low high]", got[0].ThinkingLevels)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "max-output-tokens: 64000") {
		t.Fatalf("config file not persisted:\n%s", data)
	}
}

func TestPutOAuthSettingsRejectsUnknownLevelAndNegativeLimit(t *testing.T) {
	for name, body := range map[string]string{
		"unknown level":   `{"codex":[{"name":"gpt-5.5","thinking-levels":["ultra"]}]}`,
		"negative output": `{"codex":[{"name":"gpt-5.5","max-output-tokens":-1}]}`,
		"missing name":    `{"codex":[{"max-output-tokens":1000}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{cfg: &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
				"claude": {{Name: "keep", DisplayName: "Keep"}},
			}}, configFilePath: writeTestConfigFile(t)}
			rec := oauthSettingsRequest(t, h, http.MethodPut, "/v0/management/oauth-settings", body, h.PutOAuthSettings)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if len(h.cfg.OAuthSettings["claude"]) != 1 {
				t.Fatalf("existing settings changed on a rejected write: %+v", h.cfg.OAuthSettings)
			}
		})
	}
}

func TestPatchOAuthSettingsUpsertsAndRemovesOneModel(t *testing.T) {
	h := &Handler{cfg: &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex": {{Name: "gpt-5.5", DisplayName: "Old"}, {Name: "gpt-6-sol", MaxContextLength: 200000}},
	}}, configFilePath: writeTestConfigFile(t)}

	rec := oauthSettingsRequest(t, h, http.MethodPatch, "/v0/management/oauth-settings",
		`{"channel":"codex","setting":{"name":"GPT-5.5","display-name":"New"}}`, h.PatchOAuthSettings)
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := h.cfg.OAuthSettings["codex"]
	if len(got) != 2 || got[0].DisplayName != "New" || got[1].Name != "gpt-6-sol" {
		t.Fatalf("after upsert = %+v, want gpt-5.5 replaced in place", got)
	}

	rec = oauthSettingsRequest(t, h, http.MethodPatch, "/v0/management/oauth-settings",
		`{"channel":"codex","setting":{"name":"gpt-5.5"}}`, h.PatchOAuthSettings)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got = h.cfg.OAuthSettings["codex"]
	if len(got) != 1 || got[0].Name != "gpt-6-sol" {
		t.Fatalf("after clear = %+v, want only gpt-6-sol", got)
	}

	rec = oauthSettingsRequest(t, h, http.MethodPatch, "/v0/management/oauth-settings",
		`{"channel":"codex","setting":{"name":"gpt-6-sol"}}`, h.PatchOAuthSettings)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear last status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := h.cfg.OAuthSettings["codex"]; ok {
		t.Fatalf("channel should be removed when its last setting is cleared: %+v", h.cfg.OAuthSettings)
	}

	rec = oauthSettingsRequest(t, h, http.MethodPatch, "/v0/management/oauth-settings",
		`{"channel":"codex","setting":{"name":"absent"}}`, h.PatchOAuthSettings)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("clearing an absent setting status = %d, want 404", rec.Code)
	}
	rec = oauthSettingsRequest(t, h, http.MethodPatch, "/v0/management/oauth-settings",
		`{"channel":"codex"}`, h.PatchOAuthSettings)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch without settings/setting status = %d, want 400", rec.Code)
	}
}

func TestDeleteOAuthSettingsModelAndChannel(t *testing.T) {
	h := &Handler{cfg: &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
		"codex":  {{Name: "gpt-5.5", DisplayName: "A"}, {Name: "gpt-6-sol", DisplayName: "B"}},
		"claude": {{Name: "claude-opus-5-5", ThinkingLevels: []string{"high"}}},
	}}, configFilePath: writeTestConfigFile(t)}

	rec := oauthSettingsRequest(t, h, http.MethodDelete, "/v0/management/oauth-settings?channel=codex&name=gpt-5.5", "", h.DeleteOAuthSettings)
	if rec.Code != http.StatusOK || len(h.cfg.OAuthSettings["codex"]) != 1 {
		t.Fatalf("delete model: status %d settings %+v", rec.Code, h.cfg.OAuthSettings)
	}
	rec = oauthSettingsRequest(t, h, http.MethodDelete, "/v0/management/oauth-settings?channel=claude", "", h.DeleteOAuthSettings)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete channel status %d", rec.Code)
	}
	if _, ok := h.cfg.OAuthSettings["claude"]; ok {
		t.Fatalf("claude channel still present: %+v", h.cfg.OAuthSettings)
	}
	rec = oauthSettingsRequest(t, h, http.MethodDelete, "/v0/management/oauth-settings?channel=claude", "", h.DeleteOAuthSettings)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete absent channel status %d, want 404", rec.Code)
	}
}

func TestGetOAuthSettingsModelsMergesLiveLimitsAndSettings(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "oauth-settings-test-codex", Provider: "codex", Attributes: map[string]string{}},
		{ID: "oauth-settings-test-apikey", Provider: "codex", Attributes: map[string]string{"api_key": "k"}},
		{ID: "oauth-settings-test-off", Provider: "claude", Disabled: true},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("oauth-settings-test-codex", "codex", []*registry.ModelInfo{
		{ID: "oauth-settings-gpt", DisplayName: "Registry name", ContextLength: 900000, MaxCompletionTokens: 1000},
		{ID: "oauth-settings-image", Type: registry.OpenAIImageModelType},
	})
	reg.RegisterClient("oauth-settings-test-apikey", "codex", []*registry.ModelInfo{{ID: "oauth-settings-apikey-only"}})
	reg.RegisterClient("oauth-settings-test-off", "claude", []*registry.ModelInfo{{ID: "oauth-settings-disabled"}})
	t.Cleanup(func() {
		reg.UnregisterClient("oauth-settings-test-codex")
		reg.UnregisterClient("oauth-settings-test-apikey")
		reg.UnregisterClient("oauth-settings-test-off")
	})

	h := &Handler{
		cfg: &config.Config{OAuthSettings: map[string][]config.OAuthModelSetting{
			"codex": {{Name: "oauth-settings-gpt", MaxOutputTokens: 128000}},
		}},
		authManager: manager,
		modelDetails: func() map[string]any {
			return map[string]any{"hash": "h1", "data": []map[string]any{{
				"id": "oauth-settings-gpt", "display_name": "Effective", "kind": "chat",
				"context_length": 400000, "input_length": 272000, "max_completion_tokens": 128000,
				"reasoning": map[string]any{"mode": "levels", "levels": []string{"low", "high"}},
			}}}
		},
	}
	rec := oauthSettingsRequest(t, h, http.MethodGet, "/v0/management/oauth-settings/models", "", h.GetOAuthSettingsModels)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Channels       []string             `json:"channels"`
		Models         []oauthSettingsModel `json:"models"`
		ThinkingLevels []string             `json:"thinking-levels"`
		Hash           string               `json:"details-hash"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byID := map[string]oauthSettingsModel{}
	for _, m := range resp.Models {
		byID[m.ID] = m
	}
	gpt, ok := byID["oauth-settings-gpt"]
	if !ok {
		t.Fatalf("models = %+v, want the OAuth codex model", resp.Models)
	}
	if gpt.Channel != "codex" || gpt.DisplayName != "Effective" || gpt.ContextLength != 400000 || gpt.InputLength != 272000 ||
		gpt.MaxCompletionTokens != 128000 || gpt.Reasoning == nil || strings.Join(gpt.Reasoning.Levels, ",") != "low,high" {
		t.Fatalf("gpt = %+v, want effective limits from the details catalogue", gpt)
	}
	if gpt.Setting == nil || gpt.Setting.MaxOutputTokens != 128000 {
		t.Fatalf("gpt setting = %+v, want the configured entry", gpt.Setting)
	}
	if img := byID["oauth-settings-image"]; img.Kind != "image" || img.Setting != nil {
		t.Fatalf("image = %+v, want kind image without setting", img)
	}
	if _, ok := byID["oauth-settings-apikey-only"]; ok {
		t.Fatal("API-key credentials must not be listed (oauth-settings do not apply to them)")
	}
	if _, ok := byID["oauth-settings-disabled"]; ok {
		t.Fatal("disabled credentials must not be listed")
	}
	if strings.Join(resp.ThinkingLevels, ",") != strings.Join(config.OAuthSettingThinkingLevels, ",") {
		t.Fatalf("thinking levels = %v", resp.ThinkingLevels)
	}
	if resp.Hash != "h1" {
		t.Fatalf("details hash = %q", resp.Hash)
	}
}
