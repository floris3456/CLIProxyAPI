package pluginapi

import "time"

// HostQuotaCooldownReadRequest reads runtime restrictions for exactly one credential.
type HostQuotaCooldownReadRequest struct {
	AuthIndex string `json:"auth_index"`
}

type HostQuotaCooldownReadResponse struct {
	Supported bool                `json:"supported"`
	Reason    string              `json:"reason,omitempty"`
	AuthIndex string              `json:"auth_index"`
	Provider  string              `json:"provider"`
	Cooldowns []HostQuotaCooldown `json:"cooldowns"`
}

// HostQuotaCooldown is a detached, redacted restriction. ModelKey is an exact
// stored runtime key, not an alias. Revision is opaque and process-local.
type HostQuotaCooldown struct {
	Scope             string    `json:"scope"` // credential | model
	ModelKey          string    `json:"model_key,omitempty"`
	Classification    string    `json:"classification"`  // quota | non_quota | unclear
	Basis             string    `json:"basis,omitempty"` // codex_usage_limit | claude_unified_window
	RequiredWindows   []string  `json:"required_windows,omitempty"`
	OriginScope       string    `json:"origin_scope,omitempty"`
	NextRetryAt       time.Time `json:"next_retry_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	FailureObservedAt time.Time `json:"failure_observed_at"`
	HTTPStatus        int       `json:"http_status,omitempty"`
	Revision          string    `json:"revision"`
	Clearable         bool      `json:"clearable"`
	RefusalReason     string    `json:"refusal_reason,omitempty"`
}

// HostQuotaRecoveryEvidence is an assertion by the trusted observation service,
// not a provider proof verified by CPA. A timestamp alone never authorizes clear.
type HostQuotaRecoveryEvidence struct {
	Reason                       string    `json:"reason"` // applicable_new_cycle
	Source                       string    `json:"source"` // inference_headers | provider_event | quota_endpoint
	Epoch                        string    `json:"epoch"`
	ObservedAt                   time.Time `json:"observed_at"`
	AllApplicableWindowsPositive bool      `json:"all_applicable_windows_positive"`
}

type HostQuotaCooldownClearRequest struct {
	AuthIndex string                    `json:"auth_index"`
	Scope     string                    `json:"scope"`
	ModelKey  string                    `json:"model_key,omitempty"`
	Revision  string                    `json:"revision"`
	Evidence  HostQuotaRecoveryEvidence `json:"evidence"`
}

type HostQuotaCooldownClearResponse struct {
	AuthIndex string             `json:"auth_index"`
	Scope     string             `json:"scope"`
	ModelKey  string             `json:"model_key,omitempty"`
	Cleared   bool               `json:"cleared"`
	Result    string             `json:"result"` // cleared | stale | not_found | not_clearable | invalid_evidence | unsupported
	Current   *HostQuotaCooldown `json:"current,omitempty"`
}
