package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Startup captures a batch before the file watcher supplies its normalized
// credentials. A delayed batch must not put the pre-watcher raw model back.
func TestRegisterModelsForAuthBatchDoesNotRestoreStaleAliasClassification(t *testing.T) {
	ctx := coreauth.WithSkipPersist(context.Background())
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{coreManager: manager, cfg: &config.Config{
		ClaudeKey:       []config.ClaudeKey{{APIKey: "synthetic", Models: []config.ClaudeModel{{Name: "raw-model"}}}},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{"claude": {{Name: "raw-model", Alias: "cheap-alias"}}},
	}}
	var startup []*coreauth.Auth
	for _, id := range []string{"snapshot-first", "snapshot-second"} {
		GlobalModelRegistry().UnregisterClient(id)
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(id) })
		auth, err := manager.Register(ctx, &coreauth.Auth{ID: id, Provider: "claude", Attributes: map[string]string{"api_key": "synthetic"}})
		if err != nil {
			t.Fatal(err)
		}
		startup = append(startup, auth.Clone())
		current := auth.Clone()
		current.Attributes[coreauth.AttributeAuthKind] = coreauth.AuthKindOAuth
		current, err = manager.Update(ctx, current)
		if err != nil {
			t.Fatal(err)
		}
		service.completeModelRegistrationForAuth(ctx, current)
	}
	// One stale worker finishes after the other credential's watcher registration.
	service.registerModelsForAuthBatch(ctx, startup[:1])
	for _, auth := range startup {
		models := GlobalModelRegistry().GetModelsForClient(auth.ID)
		if len(models) != 1 || models[0].ID != "cheap-alias" {
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
			}
			t.Errorf("%s: delayed startup registration restored %v; want [cheap-alias]", auth.ID, ids)
		}
	}
}

// Optional native ABI qualification uses the installed provider's actual library,
// never provider credentials or network access. The deterministic unit above runs
// without that external artifact; this test also exercises both real file parsers.
func TestRegisterModelsForAuthBatchRealPluginWatcherRace(t *testing.T) {
	library := os.Getenv("CPA_TEST_OAUTH_PLUGIN")
	if library == "" {
		t.Skip("set CPA_TEST_OAUTH_PLUGIN to the native OpenCode Go library")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode-go-cliproxyapi.so"), data, 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"deepseek-v4.1-flash"},{"id":"kimi-k3"}]}`)
	}))
	defer upstream.Close()
	path := filepath.Join(root, "config.yaml")
	text := fmt.Sprintf(`auth-dir: %q
plugins:
  enabled: true
  dir: %q
  configs:
    opencode-go-cliproxyapi:
      enabled: true
      api-keys: [{value: synthetic-first}, {value: synthetic-second}]
      base-url: %q
      allow-http: true
oauth-model-alias:
  opencode-go:
    - name: opencode-go/deepseek-v4.1-flash
      alias: deepseek-v4.1-flash-cheap
`, filepath.Join(root, "auths"), dir, upstream.URL+"/v1")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AuthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(cfg.AuthDir)
	manager := coreauth.NewManager(store, nil, nil)
	service, err := NewBuilder().WithConfig(cfg).WithConfigPath(path).WithCoreAuthManager(manager).Build()
	if err != nil {
		t.Fatal(err)
	}
	defer service.pluginHost.ShutdownAll()
	defer sdkauth.RegisterPluginAuthParser(nil)
	ctx := coreauth.WithSkipPersist(context.Background())
	service.loadRuntimeAuth(ctx)
	startup := manager.List()
	if len(startup) != 2 {
		t.Fatalf("real plugin materialized %d credentials, want 2", len(startup))
	}
	for _, auth := range startup {
		id := auth.ID
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(id) })
		if auth.AuthKind() != coreauth.AuthKindAPIKey {
			t.Fatalf("startup kind = %s, want the parser's api-key shape", auth.AuthKind())
		}
	}
	entries, err := os.ReadDir(cfg.AuthDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		file := filepath.Join(cfg.AuthDir, entry.Name())
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		auths, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{Config: cfg, AuthDir: cfg.AuthDir, Now: time.Now(), PluginAuthParser: service.pluginHost}, file, body)
		if err != nil {
			t.Fatal(err)
		}
		for _, auth := range auths {
			if auth.AuthKind() != coreauth.AuthKindOAuth {
				t.Fatalf("watcher kind = %s, want oauth", auth.AuthKind())
			}
			current, err := manager.Update(ctx, auth)
			if err != nil {
				t.Fatal(err)
			}
			service.completeModelRegistrationForAuth(ctx, current)
		}
	}
	// Exactly the production topology: two current alias-only credentials, then a
	// batch worker resolves one snapshot captured before the watcher normalized it.
	service.registerModelsForAuthBatch(ctx, startup[:1])
	for _, auth := range startup {
		models := GlobalModelRegistry().GetModelsForClient(auth.ID)
		alias := false
		for _, model := range models {
			if model.ID == "opencode-go/deepseek-v4.1-flash" {
				t.Error("delayed startup batch leaked the original name alongside the other credential's alias")
			}
			alias = alias || model.ID == "deepseek-v4.1-flash-cheap"
		}
		if !alias {
			t.Error("current credential lost its configured alias")
		}
	}
}
