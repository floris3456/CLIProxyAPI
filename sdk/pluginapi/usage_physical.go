package pluginapi

import "time"

const ClaudePhysicalProofVersion = 1
const ClaudePhysicalProofDomain = "cpa.claude.http.local-dispatch-sequence"

// UsagePhysicalDispatch is host-assigned at the guarded transport boundary.
// It is not caller evidence. The enclosing SourceEvidence binds scope/manager.
// FinalUsageAt remains an observation, never a producer-close timestamp.
type UsagePhysicalDispatch struct {
	Version           int       `json:"version"`
	AdmissionSequence uint64    `json:"admission_sequence"`
	DispatchSequence  uint64    `json:"dispatch_sequence"`
	StartedAt         time.Time `json:"started_at"`
}

// UsagePhysicalCut is a causal boundary with no outstanding scoped admission,
// producer, transport-body lifetime, or pending callback. Time alone is NOT a
// physical watermark: equal wall-clock timestamps are ordered by Sequence.
type UsagePhysicalCut struct {
	Sequence uint64    `json:"sequence"`
	At       time.Time `json:"at"`
}

// UsagePhysicalCheckpoint certifies only dispatches strictly between two cuts
// in this manager/sink/source incarnation. It does not certify arbitrary scalar
// time subwindows, remote provider completion, account membership or pricing.
// Enclosing response scope, manager/sink IDs and sequence MUST be bound with it.
type UsagePhysicalCheckpoint struct {
	Version     int               `json:"version"`
	Domain      string            `json:"domain"`
	Coverage    string            `json:"coverage"`
	Reason      string            `json:"reason,omitempty"`
	FromCut     *UsagePhysicalCut `json:"from_cut,omitempty"`
	ThroughCut  *UsagePhysicalCut `json:"through_cut,omitempty"`
	Outstanding uint64            `json:"outstanding"`
	Pending     uint64            `json:"pending"`
	GapCount    uint64            `json:"gap_count"`
	// Nil with gaps revokes the entire physical incarnation; request-time gap
	// beginnings cannot be copied into this independent domain.
	InvalidFromSequence  *uint64    `json:"invalid_from_sequence,omitempty"`
	LastProducerClosedAt *time.Time `json:"last_producer_closed_at,omitempty"`
}
