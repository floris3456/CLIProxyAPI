package pluginapi

import "time"

// HostAuthInventoryCheckpointRequest selects the provider inventory. Only "claude"
// is supported; anything else returns coverage "unsupported".
type HostAuthInventoryCheckpointRequest struct {
	Provider string `json:"provider"`
}

// HostAuthInventoryCheckpointResponse is the host's first-party credential
// membership ledger for one auth manager incarnation. It never proves that
// nothing outside this CPA used an account. See docs/claude-auth-inventory-checkpoint.md.
type HostAuthInventoryCheckpointResponse struct {
	Version              int                      `json:"version"`
	Provider             string                   `json:"provider"`
	Coverage             string                   `json:"coverage"`         // known | unknown | unsupported
	Reason               string                   `json:"reason,omitempty"` // overflow | not_ready | unsupported_provider
	ManagerIncarnationID string                   `json:"manager_incarnation_id"`
	Sequence             uint64                   `json:"sequence"`
	At                   time.Time                `json:"at"`
	Tuples               []HostAuthInventoryTuple `json:"tuples"`
}

// HostAuthInventoryTuple is one credential tuple. AccountRef is absent when the
// provider account was not returned by the provider to this host process.
type HostAuthInventoryTuple struct {
	AuthID          string `json:"auth_id"`
	AuthIndex       string `json:"auth_index"`
	AccountRef      string `json:"account_ref,omitempty"`
	BindingSequence uint64 `json:"binding_sequence"`
	State           string `json:"state"` // active | removed
}
