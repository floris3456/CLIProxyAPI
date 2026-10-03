package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestManagementOAuthSettingsRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")

	server := newTestServer(t)
	if errWrite := os.WriteFile(server.configFilePath, []byte("{}\n"), 0o600); errWrite != nil {
		t.Fatalf("failed to write config file: %v", errWrite)
	}
	manager := auth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &auth.Auth{ID: "route-test-codex", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	server.mgmt.SetAuthManager(manager)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("route-test-codex", "codex", []*registry.ModelInfo{
		{ID: "route-test-gpt", DisplayName: "Route GPT", ContextLength: 300000, MaxCompletionTokens: 64000},
	})
	t.Cleanup(func() { reg.UnregisterClient("route-test-codex") })

	do := func(method, target, body string, withKey bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withKey {
			req.Header.Set("Authorization", "Bearer test-management-key")
		}
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		return rr
	}

	for _, target := range []string{"/v0/management/oauth-settings", "/v0/management/oauth-settings/models", "/v8/management/oauth/settings/models"} {
		if rr := do(http.MethodGet, target, "", false); rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: status = %d, want 401", target, rr.Code)
		}
	}

	if rr := do(http.MethodPatch, "/v0/management/oauth-settings",
		`{"channel":"codex","setting":{"name":"route-test-gpt","max-output-tokens":32000,"display-name":"Custom"}}`, true); rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(server.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "route-test-gpt") || !strings.Contains(string(data), "max-output-tokens: 32000") {
		t.Fatalf("config not written:\n%s", data)
	}

	rr := do(http.MethodGet, "/v0/management/oauth-settings", "", true)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"display-name":"Custom"`) {
		t.Fatalf("get settings status = %d body=%s", rr.Code, rr.Body.String())
	}

	for _, target := range []string{"/v0/management/oauth-settings/models", "/v8/management/oauth/settings/models"} {
		rr = do(http.MethodGet, target, "", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", target, rr.Code, rr.Body.String())
		}
		var resp struct {
			Models []struct {
				ID      string `json:"id"`
				Channel string `json:"channel"`
				Setting *struct {
					MaxOutputTokens int `json:"max-output-tokens"`
				} `json:"setting"`
			} `json:"models"`
			Hash string `json:"details-hash"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Hash == "" {
			t.Fatalf("%s: details-hash empty; the details catalogue is not wired: %s", target, rr.Body.String())
		}
		found := false
		for _, m := range resp.Models {
			if m.ID == "route-test-gpt" && m.Channel == "codex" && m.Setting != nil && m.Setting.MaxOutputTokens == 32000 {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: route-test-gpt with its setting missing: %s", target, rr.Body.String())
		}
	}
}
