package executor

import (
	"context"
	"fmt"
	"testing"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const inventoryVerifiedAccount = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

func inventoryManager(t *testing.T, auth *cliproxyauth.Auth) (*cliproxyauth.Manager, *cliproxyauth.Auth) {
	t.Helper()
	m := cliproxyauth.NewManager(nil, nil, nil)
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	registered, err := m.Register(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	return m, registered
}

func inventoryTuple(t *testing.T, m *cliproxyauth.Manager, id string) (pluginapi.HostAuthInventoryTuple, uint64) {
	t.Helper()
	r := m.AuthInventoryCheckpoint("claude")
	if r.Coverage != "known" {
		t.Fatalf("coverage = %s/%s", r.Coverage, r.Reason)
	}
	for _, tuple := range r.Tuples {
		if tuple.AuthID == id {
			return tuple, r.Sequence
		}
	}
	t.Fatalf("tuple %s missing", id)
	return pluginapi.HostAuthInventoryTuple{}, 0
}

// refreshThroughManager runs the executor's refresh result handling and the manager's
// refresh merge exactly as Manager.refreshAuth does after a successful token exchange.
func refreshThroughManager(t *testing.T, m *cliproxyauth.Manager, id string, td *claudeauth.ClaudeTokenData) {
	t.Helper()
	current, ok := m.GetByID(id)
	if !ok {
		t.Fatalf("auth %s missing", id)
	}
	base := current.Clone()
	updated := base.Clone()
	applyClaudeRefreshedTokens(updated, td)
	if _, err := m.UpdateRefreshedAuth(context.Background(), base, updated); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAccountFallbacksAreNotVerified(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]any
		fetch    func() (*claudeauth.OAuthProfile, error)
	}{
		{
			name:     "setup_token",
			metadata: map[string]any{"type": "claude", "scopes": "user:inference"},
			fetch: func() (*claudeauth.OAuthProfile, error) {
				return nil, fmt.Errorf("setup token must not fetch a profile")
			},
		},
		{
			name:     "profile_403",
			metadata: map[string]any{"type": "claude"},
			fetch: func() (*claudeauth.OAuthProfile, error) {
				return nil, fmt.Errorf("fetch Claude OAuth profile failed with status 403: permission_error")
			},
		},
		{
			name:     "profile_without_uuid",
			metadata: map[string]any{"type": "claude"},
			fetch: func() (*claudeauth.OAuthProfile, error) {
				return &claudeauth.OAuthProfile{}, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := NewClaudeExecutor(&config.Config{})
			executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
				return tc.fetch()
			}
			m, registered := inventoryManager(t, &cliproxyauth.Auth{
				ID:         "claude-" + tc.name,
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "sk-ant-oat01-" + tc.name},
				Metadata:   tc.metadata,
			})
			prepared, err := m.PrepareRequestAuth(context.Background(), executor, registered)
			if err != nil {
				t.Fatal(err)
			}
			if claudeauth.ReadMetadataString(&prepared.Metadata, "account_uuid") == "" {
				t.Fatal("fallback did not populate a credential identity")
			}
			if tuple, _ := inventoryTuple(t, m, registered.ID); tuple.AccountRef != "" {
				t.Fatalf("fallback UUID verified: %+v", tuple)
			}
		})
	}
}

func TestClaudeAccountVerificationFromProfileAndRefresh(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	executor.oauthProfileFetcher = func(context.Context, *cliproxyauth.Auth, string) (*claudeauth.OAuthProfile, error) {
		profile := &claudeauth.OAuthProfile{}
		profile.Account.UUID = inventoryVerifiedAccount
		return profile, nil
	}
	m, registered := inventoryManager(t, &cliproxyauth.Auth{
		ID:         "claude-profile",
		Provider:   "claude",
		Attributes: map[string]string{"api_key": "sk-ant-oat01-profile"},
		Metadata:   map[string]any{"type": "claude"},
	})
	unknown, _ := inventoryTuple(t, m, registered.ID)
	if _, err := m.PrepareRequestAuth(context.Background(), executor, registered); err != nil {
		t.Fatal(err)
	}
	verified, seqVerified := inventoryTuple(t, m, registered.ID)
	if verified.AccountRef == "" || verified.BindingSequence != seqVerified || verified.BindingSequence <= unknown.BindingSequence {
		t.Fatalf("request-time profile did not verify: %+v → %+v", unknown, verified)
	}

	// Normal refresh with the same verified UUID: not a binding change.
	refreshThroughManager(t, m, registered.ID, &claudeauth.ClaudeTokenData{AccessToken: "rotated-1", RefreshToken: "r1", AccountUUID: inventoryVerifiedAccount})
	same, seqSame := inventoryTuple(t, m, registered.ID)
	if same != verified || seqSame != seqVerified {
		t.Fatalf("same-account refresh bumped: %+v/%d → %+v/%d", verified, seqVerified, same, seqSame)
	}

	// Refresh whose profile lookup was refused (no UUID returned): verification clears.
	refreshThroughManager(t, m, registered.ID, &claudeauth.ClaudeTokenData{AccessToken: "rotated-2", RefreshToken: "r2"})
	cleared, seqCleared := inventoryTuple(t, m, registered.ID)
	if cleared.AccountRef != "" || seqCleared <= seqSame || cleared.BindingSequence != seqCleared {
		t.Fatalf("refused profile kept verification: %+v (seq %d)", cleared, seqCleared)
	}
	current, _ := m.GetByID(registered.ID)
	if claudeauth.ReadMetadataString(&current.Metadata, "account_uuid") != inventoryVerifiedAccount {
		t.Fatal("refresh erased the stored credential identity")
	}

	// The next refresh profile that returns the UUID verifies it again.
	refreshThroughManager(t, m, registered.ID, &claudeauth.ClaudeTokenData{AccessToken: "rotated-3", RefreshToken: "r3", AccountUUID: inventoryVerifiedAccount})
	again, seqAgain := inventoryTuple(t, m, registered.ID)
	if again.AccountRef != verified.AccountRef || seqAgain <= seqCleared || again.BindingSequence != seqAgain {
		t.Fatalf("refresh profile did not re-verify: %+v", again)
	}
}

func TestClaudeAccountLoadedFromDiskUnverifiedUntilRefresh(t *testing.T) {
	m, registered := inventoryManager(t, &cliproxyauth.Auth{
		ID:       "claude-disk",
		Provider: "claude",
		Metadata: map[string]any{"type": "claude", "access_token": "sk-ant-oat01-disk", "refresh_token": "r0", "account_uuid": inventoryVerifiedAccount},
	})
	if tuple, _ := inventoryTuple(t, m, registered.ID); tuple.AccountRef != "" {
		t.Fatalf("disk UUID verified without a provider response: %+v", tuple)
	}
	refreshThroughManager(t, m, registered.ID, &claudeauth.ClaudeTokenData{AccessToken: "sk-ant-oat01-disk-2", RefreshToken: "r1", AccountUUID: inventoryVerifiedAccount})
	if tuple, _ := inventoryTuple(t, m, registered.ID); tuple.AccountRef == "" {
		t.Fatalf("refresh profile did not verify the loaded UUID: %+v", tuple)
	}
}
