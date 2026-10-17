package management

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// A UUID Anthropic returned to this process at login reaches the runtime manager as verified;
// the persisted file re-synthesized for the hook does not lose that in-memory fact.
func TestSaveTokenRecordCarriesLoginAccountVerification(t *testing.T) {
	const account = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	h := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", nil)
	m := coreauth.NewManager(nil, nil, nil)
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.SetPostAuthPersistHook(func(ctx context.Context, auth *coreauth.Auth) error {
		_, err := m.Register(ctx, auth.Clone())
		return err
	})
	save := func(email string, observe bool) string {
		t.Helper()
		storage := &claude.ClaudeTokenStorage{
			AccessToken:  "sk-ant-oat01-" + email,
			RefreshToken: "refresh-" + email,
			Email:        email,
			AccountUUID:  account,
			Expire:       "2026-12-31T23:59:59Z",
		}
		fileName := claude.CredentialFileName(email, "", account)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "claude",
			FileName: fileName,
			Storage:  storage,
			Metadata: map[string]any{"email": email, "account_uuid": account},
		}
		if observe {
			record.ObserveProviderAccount(account)
		}
		if _, err := h.saveTokenRecord(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		return fileName
	}
	loginID := save("login@example.com", true)
	importID := save("import@example.com", false)
	refs := map[string]string{}
	r := m.AuthInventoryCheckpoint("claude")
	if r.Coverage != "known" {
		t.Fatalf("coverage = %s/%s", r.Coverage, r.Reason)
	}
	for _, tuple := range r.Tuples {
		refs[tuple.AuthID] = tuple.AccountRef
	}
	if len(refs) != 2 {
		t.Fatalf("tuples = %+v", r.Tuples)
	}
	if refs[loginID] == "" {
		t.Fatalf("login token exchange UUID not verified: %+v", r.Tuples)
	}
	if refs[importID] != "" {
		t.Fatalf("saved record without a provider response verified: %+v", r.Tuples)
	}
}
