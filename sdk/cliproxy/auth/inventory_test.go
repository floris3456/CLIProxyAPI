package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	inventoryAccountA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	inventoryAccountB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func inventoryClaudeAuth(id, accountUUID, accessToken string) *Auth {
	metadata := map[string]any{"type": "claude", "access_token": accessToken, "refresh_token": "refresh-" + id}
	if accountUUID != "" {
		metadata["account_uuid"] = accountUUID
	}
	return &Auth{ID: id, Provider: "claude", Status: StatusActive, Metadata: metadata}
}

func readyInventoryManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	if err := m.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

// inventoryRefresh mirrors the refresh lifecycle: the executor returns rotated tokens and
// optionally an observation of the account UUID the provider returned in this process.
func inventoryRefresh(t *testing.T, m *Manager, id, accessToken, accountUUID string, observe bool) {
	t.Helper()
	current, ok := m.GetByID(id)
	if !ok {
		t.Fatalf("auth %s not registered", id)
	}
	base := current.Clone()
	updated := base.Clone()
	updated.Metadata["access_token"] = accessToken
	if accountUUID != "" {
		updated.Metadata["account_uuid"] = accountUUID
	}
	if observe {
		updated.ObserveProviderAccount(accountUUID)
	}
	if _, err := m.UpdateRefreshedAuth(context.Background(), base, updated); err != nil {
		t.Fatal(err)
	}
}

func inventoryCheckpoint(t *testing.T, m *Manager) pluginapi.HostAuthInventoryCheckpointResponse {
	t.Helper()
	r := m.AuthInventoryCheckpoint("claude")
	if r.Version != 1 || r.Provider != "claude" || r.Tuples == nil || r.At.IsZero() {
		t.Fatalf("checkpoint envelope = %+v", r)
	}
	for _, tuple := range r.Tuples {
		if tuple.BindingSequence > r.Sequence {
			t.Fatalf("tuple %+v binding after response sequence %d", tuple, r.Sequence)
		}
	}
	return r
}

func inventoryTupleFor(t *testing.T, r pluginapi.HostAuthInventoryCheckpointResponse, id string) pluginapi.HostAuthInventoryTuple {
	t.Helper()
	for _, tuple := range r.Tuples {
		if tuple.AuthID == id {
			return tuple
		}
	}
	t.Fatalf("tuple %s missing from %+v", id, r.Tuples)
	return pluginapi.HostAuthInventoryTuple{}
}

func requireKnownInventory(t *testing.T, r pluginapi.HostAuthInventoryCheckpointResponse) {
	t.Helper()
	if r.Coverage != "known" || r.Reason != "" || r.ManagerIncarnationID == "" {
		t.Fatalf("coverage = %q/%q incarnation=%q", r.Coverage, r.Reason, r.ManagerIncarnationID)
	}
}

func TestAuthInventorySequenceMonotoneAcrossAddUpdateRemove(t *testing.T) {
	ctx := context.Background()
	m := readyInventoryManager(t)
	c0 := inventoryCheckpoint(t, m)
	requireKnownInventory(t, c0)
	if len(c0.Tuples) != 0 {
		t.Fatalf("empty manager tuples = %+v", c0.Tuples)
	}

	a, err := m.Register(ctx, inventoryClaudeAuth("claude-a", inventoryAccountA, "token-1"))
	if err != nil {
		t.Fatal(err)
	}
	c1 := inventoryCheckpoint(t, m)
	requireKnownInventory(t, c1)
	ta := inventoryTupleFor(t, c1, "claude-a")
	if c1.Sequence <= c0.Sequence || ta.BindingSequence != c1.Sequence || ta.AuthIndex != a.Index || ta.State != "active" || ta.AccountRef != "" {
		t.Fatalf("add: seq %d→%d tuple=%+v", c0.Sequence, c1.Sequence, ta)
	}

	if _, err = m.Register(ctx, &Auth{ID: "codex-x", Provider: "codex", Metadata: map[string]any{"type": "codex"}}); err != nil {
		t.Fatal(err)
	}
	updatedLabel := a.Clone()
	updatedLabel.Label = "renamed"
	if _, err = m.Update(ctx, updatedLabel); err != nil {
		t.Fatal(err)
	}
	c2 := inventoryCheckpoint(t, m)
	if c2.Sequence != c1.Sequence || len(c2.Tuples) != 1 {
		t.Fatalf("non-claude add / non-binding update bumped: %d→%d tuples=%+v", c1.Sequence, c2.Sequence, c2.Tuples)
	}

	inventoryRefresh(t, m, "claude-a", "token-2", inventoryAccountA, true)
	c3 := inventoryCheckpoint(t, m)
	ta = inventoryTupleFor(t, c3, "claude-a")
	if c3.Sequence <= c2.Sequence || ta.BindingSequence != c3.Sequence || ta.AccountRef != inventoryAccountRef(inventoryAccountA) {
		t.Fatalf("verification: seq %d→%d tuple=%+v", c2.Sequence, c3.Sequence, ta)
	}
	if len(ta.AccountRef) != 64 {
		t.Fatalf("account_ref = %q, want 64 lowercase hex", ta.AccountRef)
	}
	if _, errHex := hex.DecodeString(ta.AccountRef); errHex != nil {
		t.Fatal(errHex)
	}

	m.Remove(ctx, "claude-a")
	c4 := inventoryCheckpoint(t, m)
	ta = inventoryTupleFor(t, c4, "claude-a")
	if c4.Sequence <= c3.Sequence || ta.State != "removed" || ta.BindingSequence != c4.Sequence || ta.AccountRef != inventoryAccountRef(inventoryAccountA) {
		t.Fatalf("removal tombstone: seq %d→%d tuple=%+v", c3.Sequence, c4.Sequence, ta)
	}

	if _, err = m.Register(ctx, inventoryClaudeAuth("claude-a", inventoryAccountA, "token-2")); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Register(ctx, inventoryClaudeAuth("claude-0", "", "token-0")); err != nil {
		t.Fatal(err)
	}
	c5 := inventoryCheckpoint(t, m)
	ta = inventoryTupleFor(t, c5, "claude-a")
	if c5.Sequence <= c4.Sequence || ta.State != "active" || ta.AccountRef != "" || ta.BindingSequence <= c4.Sequence {
		t.Fatalf("re-register: seq %d→%d tuple=%+v", c4.Sequence, c5.Sequence, ta)
	}
	if !sort.SliceIsSorted(c5.Tuples, func(i, j int) bool {
		if c5.Tuples[i].AuthID != c5.Tuples[j].AuthID {
			return c5.Tuples[i].AuthID < c5.Tuples[j].AuthID
		}
		return c5.Tuples[i].AuthIndex < c5.Tuples[j].AuthIndex
	}) || len(c5.Tuples) != 2 {
		t.Fatalf("tuples not sorted by (auth_id, auth_index): %+v", c5.Tuples)
	}
	if c5.ManagerIncarnationID != c0.ManagerIncarnationID {
		t.Fatal("incarnation changed within one manager")
	}
}

func TestAuthInventorySameAccountRefreshDoesNotBump(t *testing.T) {
	m := readyInventoryManager(t)
	if _, err := m.Register(context.Background(), inventoryClaudeAuth("claude-a", inventoryAccountA, "token-1")); err != nil {
		t.Fatal(err)
	}
	inventoryRefresh(t, m, "claude-a", "token-2", inventoryAccountA, true)
	before := inventoryCheckpoint(t, m)
	for i := 3; i < 6; i++ {
		inventoryRefresh(t, m, "claude-a", fmt.Sprintf("token-%d", i), inventoryAccountA, true)
	}
	after := inventoryCheckpoint(t, m)
	if after.Sequence != before.Sequence || inventoryTupleFor(t, after, "claude-a") != inventoryTupleFor(t, before, "claude-a") {
		t.Fatalf("same-account rotation is a binding change: %+v → %+v", before, after)
	}
	if inventoryTupleFor(t, after, "claude-a").AccountRef == "" {
		t.Fatal("verification lost on same-account refresh")
	}
	// A disk echo of the persisted refresh (same tokens, same UUID) keeps verification.
	current, _ := m.GetByID("claude-a")
	echo := inventoryClaudeAuth("claude-a", inventoryAccountA, "token-5")
	echo.Metadata["refresh_token"] = current.Metadata["refresh_token"]
	if _, err := m.Update(context.Background(), echo); err != nil {
		t.Fatal(err)
	}
	if echoed := inventoryCheckpoint(t, m); echoed.Sequence != after.Sequence || inventoryTupleFor(t, echoed, "claude-a").AccountRef == "" {
		t.Fatalf("unchanged disk echo changed binding: %+v", echoed)
	}
}

func TestAuthInventoryAccountChangeBumps(t *testing.T) {
	ctx := context.Background()
	m := readyInventoryManager(t)
	if _, err := m.Register(ctx, inventoryClaudeAuth("claude-a", inventoryAccountA, "token-1")); err != nil {
		t.Fatal(err)
	}
	inventoryRefresh(t, m, "claude-a", "token-2", inventoryAccountA, true)
	c1 := inventoryCheckpoint(t, m)

	inventoryRefresh(t, m, "claude-a", "token-3", inventoryAccountB, true)
	c2 := inventoryCheckpoint(t, m)
	tb := inventoryTupleFor(t, c2, "claude-a")
	if c2.Sequence != c1.Sequence+1 || tb.BindingSequence != c2.Sequence || tb.AccountRef != inventoryAccountRef(inventoryAccountB) || tb.AccountRef == inventoryTupleFor(t, c1, "claude-a").AccountRef {
		t.Fatalf("verified account change: %+v → %+v", c1, c2)
	}

	// The provider returned no UUID (refused/empty profile): verification clears.
	inventoryRefresh(t, m, "claude-a", "token-4", "", true)
	c3 := inventoryCheckpoint(t, m)
	tc := inventoryTupleFor(t, c3, "claude-a")
	if c3.Sequence != c2.Sequence+1 || tc.BindingSequence != c3.Sequence || tc.AccountRef != "" {
		t.Fatalf("known→unknown: %+v → %+v", c2, c3)
	}

	// Unknown→known is a binding change too.
	inventoryRefresh(t, m, "claude-a", "token-5", inventoryAccountB, true)
	c4 := inventoryCheckpoint(t, m)
	if c4.Sequence != c3.Sequence+1 || inventoryTupleFor(t, c4, "claude-a").AccountRef != inventoryAccountRef(inventoryAccountB) {
		t.Fatalf("unknown→known: %+v", c4)
	}

	// Credentials replaced without a provider observation (file edit/import): unverified.
	current, _ := m.GetByID("claude-a")
	replaced := current.Clone()
	replaced.Metadata["access_token"] = "imported-token"
	if _, err := m.Update(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	c5 := inventoryCheckpoint(t, m)
	if c5.Sequence != c4.Sequence+1 || inventoryTupleFor(t, c5, "claude-a").AccountRef != "" {
		t.Fatalf("unobserved credential replacement kept verification: %+v", c5)
	}

	// An observation that disagrees with the UUID actually stored on the credential is not verification.
	inventoryRefresh(t, m, "claude-a", "token-6", inventoryAccountB, true)
	current, _ = m.GetByID("claude-a")
	mismatch := current.Clone()
	mismatch.Metadata["account_uuid"] = inventoryAccountA
	mismatch.ObserveProviderAccount(inventoryAccountB)
	if _, err := m.Update(ctx, mismatch); err != nil {
		t.Fatal(err)
	}
	if got := inventoryTupleFor(t, inventoryCheckpoint(t, m), "claude-a"); got.AccountRef != "" {
		t.Fatalf("mismatched observation verified: %+v", got)
	}
}

func TestAuthInventoryUnverifiedAccountOmitted(t *testing.T) {
	ctx := context.Background()
	store := &inventoryTestStore{items: []*Auth{inventoryClaudeAuth("disk-claude", inventoryAccountA, "disk-token")}}
	m := NewManager(store, nil, nil)
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(ctx, inventoryClaudeAuth("registered-claude", inventoryAccountB, "reg-token")); err != nil {
		t.Fatal(err)
	}
	r := inventoryCheckpoint(t, m)
	requireKnownInventory(t, r)
	for _, id := range []string{"disk-claude", "registered-claude"} {
		if tuple := inventoryTupleFor(t, r, id); tuple.AccountRef != "" {
			t.Fatalf("stored/registered UUID treated as verified: %+v", tuple)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Tuples []map[string]any `json:"tuples"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, tuple := range wire.Tuples {
		if _, present := tuple["account_ref"]; present {
			t.Fatalf("unknown account_ref not omitted: %s", raw)
		}
	}

	// The loaded UUID becomes verified only after an in-process provider observation.
	inventoryRefresh(t, m, "disk-claude", "disk-token-2", inventoryAccountA, true)
	verified := inventoryCheckpoint(t, m)
	if tuple := inventoryTupleFor(t, verified, "disk-claude"); tuple.AccountRef != inventoryAccountRef(inventoryAccountA) {
		t.Fatalf("in-process refresh did not verify loaded UUID: %+v", tuple)
	}

	// A reload from the store is disk provenance again.
	if err = m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded := inventoryCheckpoint(t, m)
	if tuple := inventoryTupleFor(t, reloaded, "disk-claude"); tuple.AccountRef != "" || tuple.BindingSequence <= verified.Sequence {
		t.Fatalf("reload kept in-memory verification: %+v (seq %d)", tuple, reloaded.Sequence)
	}
	if tuple := inventoryTupleFor(t, reloaded, "registered-claude"); tuple.State != "removed" {
		t.Fatalf("auth absent from reload not tombstoned: %+v", tuple)
	}
}

func TestAuthInventoryOverflowCountsTombstones(t *testing.T) {
	ctx := WithDeferredAPIKeyModelAliasRebuild(context.Background())
	m := readyInventoryManager(t)
	for i := 0; i < authInventoryLimitForTest; i++ {
		if _, err := m.Register(ctx, inventoryClaudeAuth(fmt.Sprintf("claude-%04d", i), "", "token")); err != nil {
			t.Fatal(err)
		}
	}
	full := inventoryCheckpoint(t, m)
	requireKnownInventory(t, full)
	if len(full.Tuples) != authInventoryLimitForTest {
		t.Fatalf("tuples = %d, want %d", len(full.Tuples), authInventoryLimitForTest)
	}
	m.Remove(ctx, "claude-0000")
	if r := inventoryCheckpoint(t, m); r.Coverage != "known" || len(r.Tuples) != authInventoryLimitForTest {
		t.Fatalf("tombstone dropped from bound: %s/%d", r.Coverage, len(r.Tuples))
	}
	if _, err := m.Register(ctx, inventoryClaudeAuth("claude-overflow", "", "token")); err != nil {
		t.Fatal(err)
	}
	r := inventoryCheckpoint(t, m)
	if r.Coverage != "unknown" || r.Reason != "overflow" || r.Tuples == nil || len(r.Tuples) != 0 || r.Sequence <= full.Sequence {
		t.Fatalf("1025 tuples = %s/%s tuples=%d seq=%d", r.Coverage, r.Reason, len(r.Tuples), r.Sequence)
	}
}

func TestAuthInventoryIncarnationPerManager(t *testing.T) {
	m1 := readyInventoryManager(t)
	m2 := readyInventoryManager(t)
	r1, r2 := inventoryCheckpoint(t, m1), inventoryCheckpoint(t, m2)
	if r1.ManagerIncarnationID == "" || r1.ManagerIncarnationID == r2.ManagerIncarnationID {
		t.Fatalf("incarnations %q %q", r1.ManagerIncarnationID, r2.ManagerIncarnationID)
	}
	if again := inventoryCheckpoint(t, m1); again.ManagerIncarnationID != r1.ManagerIncarnationID {
		t.Fatal("incarnation not stable")
	}
	var literal Manager
	if r := literal.AuthInventoryCheckpoint("claude"); r.ManagerIncarnationID == "" || r.ManagerIncarnationID == r1.ManagerIncarnationID {
		t.Fatalf("zero-value manager incarnation = %q", r.ManagerIncarnationID)
	}
}

func TestAuthInventoryUnsupportedProvider(t *testing.T) {
	m := readyInventoryManager(t)
	if _, err := m.Register(context.Background(), inventoryClaudeAuth("claude-a", "", "token")); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"codex", "", "Claude"} {
		r := m.AuthInventoryCheckpoint(provider)
		if r.Version != 1 || r.Provider != provider || r.Coverage != "unsupported" || r.Reason != "unsupported_provider" || r.Tuples == nil || len(r.Tuples) != 0 {
			t.Fatalf("provider %q = %+v", provider, r)
		}
	}
}

func TestAuthInventoryCatalogNotReady(t *testing.T) {
	ctx := context.Background()
	m := NewManager(nil, nil, nil)
	if _, err := m.Register(ctx, inventoryClaudeAuth("claude-a", "", "token")); err != nil {
		t.Fatal(err)
	}
	notReady := func(r pluginapi.HostAuthInventoryCheckpointResponse, label string) {
		t.Helper()
		if r.Coverage != "unknown" || r.Reason != "not_ready" || r.Tuples == nil || len(r.Tuples) != 0 || r.ManagerIncarnationID == "" {
			t.Fatalf("%s: %+v", label, r)
		}
	}
	notReady(m.AuthInventoryCheckpoint("claude"), "uninitialized")

	failing := NewManager(&inventoryTestStore{err: fmt.Errorf("dummy load failure")}, nil, nil)
	if err := failing.Load(ctx); err == nil {
		t.Fatal("load failure lost")
	}
	notReady(failing.AuthInventoryCheckpoint("claude"), "failed")

	blocking := &inventoryTestStore{entered: make(chan struct{}), release: make(chan struct{})}
	loading := NewManager(blocking, nil, nil)
	done := make(chan error, 1)
	go func() { done <- loading.Load(ctx) }()
	<-blocking.entered
	read := make(chan pluginapi.HostAuthInventoryCheckpointResponse, 1)
	go func() { read <- loading.AuthInventoryCheckpoint("claude") }()
	select {
	case r := <-read:
		notReady(r, "loading")
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint blocked behind store I/O")
	}
	close(blocking.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireKnownInventory(t, inventoryCheckpoint(t, loading))

	home := readyInventoryManager(t)
	cfg := &internalconfig.Config{}
	cfg.Home.Enabled = true
	home.runtimeConfig.Store(cfg)
	notReady(home.AuthInventoryCheckpoint("claude"), "home")

	var nilManager *Manager
	notReady2 := nilManager.AuthInventoryCheckpoint("claude")
	if notReady2.Coverage != "unknown" || notReady2.Reason != "not_ready" || notReady2.Tuples == nil {
		t.Fatalf("nil manager: %+v", notReady2)
	}
}

func TestAuthInventoryConcurrentMutationsNoTornReads(t *testing.T) {
	ctx := WithDeferredAPIKeyModelAliasRebuild(context.Background())
	m := readyInventoryManager(t)
	stop := make(chan struct{})
	var writers, readers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 60; i++ {
				id := fmt.Sprintf("claude-%d-%d", w, i%5)
				account := inventoryAccountA
				if i%3 == 0 {
					account = inventoryAccountB
				}
				switch i % 4 {
				case 0, 1:
					_, _ = m.Register(ctx, inventoryClaudeAuth(id, account, fmt.Sprintf("t-%d", i)))
				case 2:
					if current, ok := m.GetByID(id); ok {
						updated := current.Clone()
						updated.Metadata["access_token"] = fmt.Sprintf("r-%d", i)
						updated.Metadata["account_uuid"] = account
						updated.ObserveProviderAccount(account)
						_, _ = m.UpdateRefreshedAuth(ctx, current, updated)
					}
				default:
					m.Remove(ctx, id)
				}
			}
		}(w)
	}
	errs := make(chan string, 8)
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			var last uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp := m.AuthInventoryCheckpoint("claude")
				if resp.Sequence < last {
					errs <- fmt.Sprintf("sequence went backwards %d < %d", resp.Sequence, last)
					return
				}
				last = resp.Sequence
				for _, tuple := range resp.Tuples {
					if tuple.BindingSequence > resp.Sequence || tuple.BindingSequence == 0 {
						errs <- fmt.Sprintf("torn read: tuple %+v vs sequence %d", tuple, resp.Sequence)
						return
					}
				}
			}
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for msg := range errs {
		t.Fatal(msg)
	}
	final := inventoryCheckpoint(t, m)
	requireKnownInventory(t, final)
}

const authInventoryLimitForTest = 1024

// inventoryTestStore serves fixed auths; with entered/release it blocks inside List.
type inventoryTestStore struct {
	items   []*Auth
	err     error
	entered chan struct{}
	release chan struct{}
}

func (s *inventoryTestStore) List(ctx context.Context) ([]*Auth, error) {
	if s.entered != nil {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([]*Auth, 0, len(s.items))
	for _, a := range s.items {
		out = append(out, a.Clone())
	}
	return out, s.err
}
func (s *inventoryTestStore) Save(context.Context, *Auth) (string, error) { return "", nil }
func (s *inventoryTestStore) Delete(context.Context, string) error        { return nil }
