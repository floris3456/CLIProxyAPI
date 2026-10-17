package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Verification is in-memory only: the persisted auth file and the runtime metadata after a
// verified refresh are byte-for-byte what the same refresh writes without verification.
func TestAuthInventoryVerificationIsNeverPersisted(t *testing.T) {
	const account = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	initial := []byte(`{"type":"claude","email":"user@example.com","access_token":"old-token","refresh_token":"refresh","account_uuid":"` + account + `"}`)
	run := func(observe bool) ([]byte, map[string]any, string) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "claude-user.json")
		if err := os.WriteFile(path, initial, 0o600); err != nil {
			t.Fatal(err)
		}
		store := sdkauth.NewFileTokenStore()
		store.SetBaseDir(dir)
		m := coreauth.NewManager(store, nil, nil)
		if err := m.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		list := m.List()
		if len(list) != 1 {
			t.Fatalf("loaded %d auths", len(list))
		}
		base := list[0]
		updated := base.Clone()
		updated.Metadata["access_token"] = "new-token"
		updated.Metadata["account_uuid"] = account
		if observe {
			updated.ObserveProviderAccount(account)
		}
		saved, err := m.UpdateRefreshedAuth(context.Background(), base, updated)
		if err != nil {
			t.Fatal(err)
		}
		if _, pending := saved.ProviderAccountObservation(); pending {
			t.Fatal("returned auth still carries the observation")
		}
		current, _ := m.GetByID(base.ID)
		if _, pending := current.ProviderAccountObservation(); pending {
			t.Fatal("stored auth carries the observation")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ref := ""
		for _, tuple := range m.AuthInventoryCheckpoint("claude").Tuples {
			ref = tuple.AccountRef
		}
		return raw, current.Metadata, ref
	}
	verifiedFile, verifiedMeta, verifiedRef := run(true)
	plainFile, plainMeta, plainRef := run(false)
	if verifiedRef == "" || plainRef != "" {
		t.Fatalf("control: verified ref=%q plain ref=%q", verifiedRef, plainRef)
	}
	if !bytes.Equal(verifiedFile, plainFile) {
		t.Fatalf("auth file differs:\n%s\n%s", verifiedFile, plainFile)
	}
	if !reflect.DeepEqual(verifiedMeta, plainMeta) {
		t.Fatalf("metadata differs:\n%#v\n%#v", verifiedMeta, plainMeta)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(verifiedFile, &onDisk); err != nil {
		t.Fatal(err)
	}
	for key := range onDisk {
		switch key {
		case "type", "email", "access_token", "refresh_token", "account_uuid", "disabled":
		default:
			t.Fatalf("unexpected persisted key %q in %s", key, verifiedFile)
		}
	}
	if bytes.Contains(verifiedFile, []byte(verifiedRef)) {
		t.Fatal("account_ref persisted")
	}
	raw, err := json.Marshal(verifiedMeta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(verifiedRef)) {
		t.Fatal("account_ref in metadata")
	}
}
