package pluginapi

import (
	"context"
	"encoding/json"
	"time"
)

// CachePolicyProvider is deliberately limited to Pacer's inference-key-owned policy API.
// The plugin owns durable state, capabilities, revisions, and compaction admission.
type CachePolicyProvider interface {
	HandleCachePolicy(context.Context, CachePolicyRequest) (CachePolicyResponse, error)
	ResolveCachePolicy(context.Context, CachePolicyResolveRequest) (CachePolicy, error)
	CachePolicyAttempt(context.Context, CachePolicyAttemptRequest) (CachePolicyAttemptResponse, error)
}

type CachePolicyScope struct {
	CallerScope    string `json:"caller_scope"`
	InstallationID string `json:"installation_id"`
	SessionID      string `json:"session_id"`
	ProviderRoute  string `json:"provider_route"`
}

// CachePolicyOperation is the client body. Caller identity and capability are never body fields.
type CachePolicyOperation struct {
	Protocol              int                           `json:"protocol"`
	Operation             string                        `json:"operation"`
	InstallationID        string                        `json:"installation_id"`
	SessionID             string                        `json:"session_id"`
	ProviderRoute         string                        `json:"provider_route"`
	ContextEpoch          string                        `json:"context_epoch,omitempty"`
	ExpectedRevision      int64                         `json:"expected_revision"`
	TTL                   string                        `json:"ttl,omitempty"`
	OperationID           string                        `json:"operation_id,omitempty"`
	TransitionID          string                        `json:"transition_id,omitempty"`
	Enrollment            string                        `json:"enrollment,omitempty"`
	Checkpoint            string                        `json:"checkpoint,omitempty"`
	CheckpointReceipt     *CachePolicyCheckpointReceipt `json:"checkpoint_receipt,omitempty"`
	CompactionStartedAt   *time.Time                    `json:"compaction_started_at,omitempty"`
	CheckpointCommittedAt *time.Time                    `json:"checkpoint_committed_at,omitempty"`
}

// CachePolicyCheckpointReceipt is the client's durable Core acknowledgement,
// bound to a prepare transition. Pacer validates it against durable pending state.
type CachePolicyCheckpointReceipt struct {
	Version            int    `json:"version"`
	TransitionID       string `json:"transition_id"`
	PreviousCheckpoint string `json:"previous_checkpoint"`
	Checkpoint         string `json:"checkpoint"`
	Committed          bool   `json:"committed"`
}

type CachePolicyRequest struct {
	Method     string               `json:"method"`
	Scope      CachePolicyScope     `json:"scope"`
	Capability string               `json:"capability,omitempty"`
	Operation  CachePolicyOperation `json:"operation"`
}

// Body is JSON, not a base64-encoded HTTP body. No arbitrary response headers are accepted.
type CachePolicyResponse struct {
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body"`
}

type CachePolicyResolveRequest struct {
	Protocol     int              `json:"protocol"`
	Scope        CachePolicyScope `json:"scope"`
	Capability   string           `json:"capability"`
	ContextEpoch string           `json:"context_epoch"`
	Revision     int64            `json:"revision"`
	Kind         string           `json:"kind"`
	RequestID    string           `json:"request_id"`
	Model        string           `json:"model"`
	SourceFormat string           `json:"source_format"`
	Stream       bool             `json:"stream"`
}

// CachePolicy is a validated, immutable selection. It contains no private capability.
type CachePolicy struct {
	Protocol     int              `json:"protocol"`
	Scope        CachePolicyScope `json:"scope"`
	ContextEpoch string           `json:"context_epoch"`
	Revision     int64            `json:"revision"`
	TTL          string           `json:"ttl"`
	Kind         string           `json:"kind"`
}

// CachePolicyAttemptRequest is synchronous; begin/validate failures prohibit dispatch.
// Capability is supplied by a private host closure, never by mutable executor metadata.
type CachePolicyAttemptRequest struct {
	Operation  string      `json:"operation"` // begin | validate | finish
	RequestID  string      `json:"request_id"`
	AttemptID  string      `json:"attempt_id"`
	Capability string      `json:"capability"`
	Policy     CachePolicy `json:"policy"`
	StartedAt  time.Time   `json:"started_at"`
	Outcome    string      `json:"outcome,omitempty"` // succeeded | failed | canceled
	Dispatched bool        `json:"dispatched"`
}

type CachePolicyAttemptResponse struct {
	Policy    CachePolicy `json:"policy"`
	StartedAt time.Time   `json:"started_at"`
}

// CachePolicyEvidence describes one physical attempt, never the chat's current state.
// EffectiveTTL covers caller/CPA breakpoints only; provider-added writes can have other TTLs.
type CachePolicyEvidence struct {
	AttemptStartedAt  time.Time            `json:"attempt_started_at,omitempty"`
	Selected          *CachePolicy         `json:"selected,omitempty"`
	EffectiveTTL      string               `json:"effective_ttl,omitempty"`
	BreakpointCount   int                  `json:"breakpoint_count"`
	Dispatched        bool                 `json:"dispatched"`
	UpstreamStartedAt time.Time            `json:"upstream_started_at,omitempty"`
	Prefix            *CachePrefixSnapshot `json:"prefix,omitempty"`
}

// CachePrefixSnapshot is a content-free, keyed fingerprint of the final signed Claude wire
// request, computed once before dispatch. Tags are HMACs under a per-process random key, so
// they are comparable only when KeyID matches; a restart rotates the key. Shape is
// "text_single_pass" only when Reasons is empty.
type CachePrefixSnapshot struct {
	Version     int                   `json:"version"`
	KeyID       string                `json:"key_id"`
	SettingsTag string                `json:"settings_tag"`
	Endpoints   []CachePrefixEndpoint `json:"endpoints"`
	Automatic   bool                  `json:"automatic"`
	Shape       string                `json:"shape"`
	Reasons     []string              `json:"reasons,omitempty"`
}

// CachePrefixEndpoint is one explicit cache_control marker in final-wire order.
// Tag covers the cumulative prefix through the marked block.
type CachePrefixEndpoint struct {
	Ordinal int    `json:"ordinal"`
	Section string `json:"section"`
	TTL     string `json:"ttl"`
	Tag     string `json:"tag"`
}

// CacheCreationUsage preserves optional raw provider counters and their classification.
// nil counters mean absent, not zero. UnknownTokens is nil when the total is absent/invalid.
type CacheCreationUsage struct {
	TotalTokens            *int64 `json:"total_tokens,omitempty"`
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens,omitempty"`
	UnknownTokens          *int64 `json:"unknown_tokens,omitempty"`
	Quality                string `json:"quality"`
}

// UsageTokenBreakdown mirrors accounting schema v2 without importing host implementation packages.
type UsageTokenBreakdown struct {
	SchemaVersion      int              `json:"schema_version"`
	Quality            string           `json:"quality"`
	TotalTokens        int64            `json:"total_tokens"`
	Input              UsageTokenInput  `json:"input"`
	Output             UsageTokenOutput `json:"output"`
	UnclassifiedTokens int64            `json:"unclassified_tokens"`
}
type UsageTokenInput struct {
	TotalTokens      int64 `json:"total_tokens"`
	UncachedTokens   int64 `json:"uncached_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}
type UsageTokenOutput struct {
	TotalTokens        int64 `json:"total_tokens"`
	NonReasoningTokens int64 `json:"non_reasoning_tokens"`
	ReasoningTokens    int64 `json:"reasoning_tokens"`
}
