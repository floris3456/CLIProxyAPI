package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// First-party Claude membership ledger behind host.auth.inventory_checkpoint
// (docs/claude-auth-inventory-checkpoint.md). Every binding change is applied
// under m.mu by Register, updateInternal, Remove and Load.

const authInventoryLimit = 1024

// providerAccountObservation is an in-memory note of the account UUID the
// provider itself returned to this process. It is never persisted.
type providerAccountObservation struct {
	accountUUID string
}

// accountObservationMu guards Auth.accountObservation: request preparation may
// run concurrently on one shared Auth.
var accountObservationMu sync.Mutex

// ObserveProviderAccount records, in memory only, that the provider returned
// accountUUID for this credential in this process (login token exchange,
// refresh profile or request-time profile). An empty accountUUID records that
// the provider did not confirm an account (fallback, refused or empty profile),
// which clears earlier verification. The Manager consumes the note when the
// auth is registered or updated; it never reaches the store.
func (a *Auth) ObserveProviderAccount(accountUUID string) {
	if a == nil {
		return
	}
	observation := &providerAccountObservation{accountUUID: strings.TrimSpace(accountUUID)}
	accountObservationMu.Lock()
	a.accountObservation = observation
	accountObservationMu.Unlock()
}

// ProviderAccountObservation returns a note recorded by ObserveProviderAccount
// that the Manager has not consumed yet.
func (a *Auth) ProviderAccountObservation() (string, bool) {
	if a == nil {
		return "", false
	}
	accountObservationMu.Lock()
	observation := a.accountObservation
	accountObservationMu.Unlock()
	if observation == nil {
		return "", false
	}
	return observation.accountUUID, true
}

func takeProviderAccountObservation(a *Auth) *providerAccountObservation {
	if a == nil {
		return nil
	}
	accountObservationMu.Lock()
	observation := a.accountObservation
	a.accountObservation = nil
	accountObservationMu.Unlock()
	return observation
}

type inventoryKey struct {
	authID    string
	authIndex string
}

type inventoryEntry struct {
	account string // verified provider account UUID, lowercase; empty means unknown
	binding uint64
	removed bool
}

type authInventory struct {
	incarnation string
	// sequence is written only under Manager.mu and read atomically while loading.
	sequence atomic.Uint64
	entries  map[inventoryKey]*inventoryEntry
	live     map[string]inventoryKey
	overflow bool
}

func (m *Manager) inventory() *authInventory {
	m.inventoryOnce.Do(func() {
		m.authInventory = &authInventory{
			incarnation: uuid.NewString(),
			entries:     make(map[inventoryKey]*inventoryEntry),
			live:        make(map[string]inventoryKey),
		}
	})
	return m.authInventory
}

func isInventoryProvider(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), "claude")
}

func inventoryAccountUUID(a *Auth) string {
	if value := authMetadataString(a, "account_uuid"); value != "" {
		return value
	}
	return authMetadataString(a, "accountUuid")
}

// noteInventoryLocked records the binding of auth id after a mutation. prev is
// the stored auth before the mutation and next the stored auth after it (nil
// when removed). Values from the backing store are never verified. Without a
// provider observation, verification survives only an update that changes
// neither the credentials nor the stored account UUID. Callers hold m.mu.
func (m *Manager) noteInventoryLocked(id string, prev, next *Auth, observation *providerAccountObservation, fromStore bool) {
	inv := m.inventory()
	oldKey, hadLive := inv.live[id]
	if next == nil || !isInventoryProvider(next.Provider) {
		if hadLive {
			inv.remove(oldKey)
			delete(inv.live, id)
		}
		return
	}
	key := inventoryKey{authID: next.ID, authIndex: next.Index}
	stored := inventoryAccountUUID(next)
	account := ""
	switch {
	case fromStore:
	case observation != nil:
		if observation.accountUUID != "" && strings.EqualFold(observation.accountUUID, stored) {
			account = strings.ToLower(stored)
		}
	case hadLive && oldKey == key && prev != nil:
		if e := inv.entries[key]; e != nil && e.account != "" && !CredentialsChanged(prev, next) && strings.EqualFold(e.account, stored) {
			account = e.account
		}
	}
	if hadLive && oldKey != key {
		inv.remove(oldKey)
	}
	if inv.set(key, account) {
		inv.live[id] = key
	} else {
		delete(inv.live, id)
	}
}

// set records the tuple's current binding and reports whether it is tracked.
// The ledger never holds more than authInventoryLimit entries; an additional
// tuple makes the inventory permanently overflowed for this incarnation.
func (inv *authInventory) set(key inventoryKey, account string) bool {
	e := inv.entries[key]
	if e != nil && !e.removed && e.account == account {
		return true
	}
	if e == nil {
		if len(inv.entries) >= authInventoryLimit {
			inv.overflow = true
			inv.sequence.Add(1)
			return false
		}
		e = &inventoryEntry{}
		inv.entries[key] = e
	}
	e.account, e.removed = account, false
	e.binding = inv.sequence.Add(1)
	return true
}

// remove turns a tuple into a tombstone that keeps its last account binding.
func (inv *authInventory) remove(key inventoryKey) {
	e := inv.entries[key]
	if e == nil || e.removed {
		return
	}
	e.removed = true
	e.binding = inv.sequence.Add(1)
}

// AuthInventoryCheckpoint returns the first-party Claude inventory ledger of
// this manager incarnation. It reads bounded in-memory state only.
func (m *Manager) AuthInventoryCheckpoint(provider string) pluginapi.HostAuthInventoryCheckpointResponse {
	resp := pluginapi.HostAuthInventoryCheckpointResponse{
		Version:  1,
		Provider: provider,
		Coverage: "unknown",
		Reason:   "not_ready",
		Tuples:   []pluginapi.HostAuthInventoryTuple{},
	}
	if provider != "claude" {
		resp.Coverage, resp.Reason = "unsupported", "unsupported_provider"
	}
	if m == nil {
		resp.At = time.Now().UTC()
		return resp
	}
	inv := m.inventory()
	resp.ManagerIncarnationID = inv.incarnation
	if resp.Coverage == "unsupported" {
		resp.Sequence = inv.sequence.Load()
		resp.At = time.Now().UTC()
		return resp
	}
	home := m.HomeEnabled()
	// Same ordering as CatalogSnapshot: never wait behind a Load holding m.mu for store I/O.
	m.catalogStartMu.RLock()
	defer m.catalogStartMu.RUnlock()
	if load := m.catalogLoad.Load(); load != nil && load.state == "loading" {
		resp.Sequence = inv.sequence.Load()
		resp.At = time.Now().UTC()
		return resp
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	resp.Sequence = inv.sequence.Load()
	resp.At = time.Now().UTC()
	if load := m.catalogLoad.Load(); home || load == nil || load.state != "ready" {
		return resp
	}
	if inv.overflow {
		resp.Reason = "overflow"
		return resp
	}
	tuples := make([]pluginapi.HostAuthInventoryTuple, 0, len(inv.entries))
	for key, e := range inv.entries {
		tuple := pluginapi.HostAuthInventoryTuple{
			AuthID:          key.authID,
			AuthIndex:       key.authIndex,
			AccountRef:      inventoryAccountRef(e.account),
			BindingSequence: e.binding,
			State:           "active",
		}
		if e.removed {
			tuple.State = "removed"
		}
		tuples = append(tuples, tuple)
	}
	sort.Slice(tuples, func(i, j int) bool {
		if tuples[i].AuthID != tuples[j].AuthID {
			return tuples[i].AuthID < tuples[j].AuthID
		}
		return tuples[i].AuthIndex < tuples[j].AuthIndex
	})
	resp.Coverage, resp.Reason, resp.Tuples = "known", "", tuples
	return resp
}

var (
	inventoryKeyOnce sync.Once
	inventoryHostKey []byte
)

// inventoryAccountRef is HMAC-SHA256 of the verified account UUID under a
// per-process random key that is never logged or exported. Refs from
// different processes are incomparable, like the manager incarnation.
func inventoryAccountRef(accountUUID string) string {
	if accountUUID == "" {
		return ""
	}
	inventoryKeyOnce.Do(func() {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err == nil {
			inventoryHostKey = key
		}
	})
	if inventoryHostKey == nil {
		return ""
	}
	mac := hmac.New(sha256.New, inventoryHostKey)
	mac.Write([]byte(accountUUID))
	return hex.EncodeToString(mac.Sum(nil))
}
