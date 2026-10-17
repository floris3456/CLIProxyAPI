package pluginapi

import "time"

const ClaudeHTTPUsageSource = "cpa.claude.http.first-party"
const ClaudeHTTPUsageSourceVersion = 1

// UsageSourceScope identifies a host runtime credential, not a provider billing
// account. Different credential tuples are never assumed to share quota.
type UsageSourceScope struct {
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	AuthIndex string `json:"auth_index"`
	Kind      string `json:"kind"`
	Version   int    `json:"version"`
}

// UsageSourceEvidence is assigned by the host queue from a private live ticket;
// publisher-supplied evidence is discarded. Times are host observation clocks.
type UsageSourceEvidence struct {
	Physical             *UsagePhysicalDispatch `json:"physical,omitempty"`
	Scope                UsageSourceScope       `json:"scope"`
	ManagerIncarnationID string                 `json:"manager_incarnation_id"`
	Tracked              bool                   `json:"tracked"`
	RequestedAt          time.Time              `json:"requested_at"`
	UpstreamStartedAt    *time.Time             `json:"upstream_started_at,omitempty"`
	FinalUsageAt         time.Time              `json:"final_usage_at"`
	Gap                  *UsageDeliveryGap      `json:"gap,omitempty"`
}

type UsageDeliveryGap struct {
	Sequence   uint64     `json:"sequence"`
	Reason     string     `json:"reason"`
	From       *time.Time `json:"from,omitempty"`    // nil means unknown beginning
	Through    *time.Time `json:"through,omitempty"` // nil means open-ended
	ObservedAt time.Time  `json:"observed_at"`
}

type HostUsageCheckpointRequest struct {
	Scope UsageSourceScope `json:"scope"`
}

// A known checkpoint covers source RequestedAt in [From,Through), exclusively
// for Scope. It is not proof of total account traffic or sufficient quota data.
type HostUsageCheckpointResponse struct {
	Physical             *UsagePhysicalCheckpoint `json:"physical,omitempty"`
	Scope                UsageSourceScope         `json:"scope"`
	Coverage             string                   `json:"coverage"` // known | unknown | unsupported
	Reason               string                   `json:"reason,omitempty"`
	ManagerIncarnationID string                   `json:"manager_incarnation_id"`
	IncarnationID        string                   `json:"incarnation_id,omitempty"`
	Sequence             uint64                   `json:"sequence"`
	From                 *time.Time               `json:"from,omitempty"`
	Through              *time.Time               `json:"through,omitempty"`
	Active               uint64                   `json:"active"`
	Pending              uint64                   `json:"pending"`
	GapCount             uint64                   `json:"gap_count"`
	InvalidFrom          *time.Time               `json:"invalid_from,omitempty"` // nil with gaps means unknown beginning
	Gaps                 []UsageDeliveryGap       `json:"gaps"`
	GapsTruncated        bool                     `json:"gaps_truncated"`
}
