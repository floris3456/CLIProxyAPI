package cliproxy

import (
	"context"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestRegisterModelsForAuthRecordsCandidatesBeforeExclusions(t *testing.T) {
	service := &Service{cfg: &config.Config{OAuthExcludedModels: map[string][]string{"claude": {"*"}}}}
	auth := &coreauth.Auth{ID: "auth-candidates-claude", Provider: "claude", Status: coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"}}
	GlobalModelRegistry().UnregisterClient(auth.ID)
	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(auth.ID)
		internalregistry.ForgetClientCandidates(auth.ID)
	})
	service.registerModelsForAuth(context.Background(), auth)
	if got := GlobalModelRegistry().GetModelsForClient(auth.ID); len(got) != 0 {
		t.Fatalf("registered %d models, want 0 (all excluded)", len(got))
	}
	if got := internalregistry.ClientCandidates(auth.ID); len(got) == 0 {
		t.Fatal("no candidates recorded; disabled models could not be listed or re-enabled")
	}
}

func TestOpenAICompatExcludedModelsSkipsConfiguredModel(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "lithos",
		Models: []config.OpenAICompatibilityModel{
			{Name: "deepseek-v4.1-flash", Alias: "deepseek-flash-fast"},
			{Name: "kimi-k3"},
			{Name: "glm-5.3"},
		},
		ExcludedModels: []string{"kimi-*", "DEEPSEEK-FLASH-FAST"},
	}
	models := buildOpenAICompatibilityConfigModels(compat)
	if len(models) != 1 || models[0].ID != "glm-5.3" {
		ids := []string{}
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		t.Fatalf("models = %v, want only glm-5.3 (alias and wildcard exclusions apply)", ids)
	}
}
