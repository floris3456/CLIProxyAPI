package pluginapi

import (
	"context"
	"time"
)

// AcknowledgedUsagePlugin is optional. A nil return acknowledges completion of
// the sink's write, not merely receipt/enqueue. Legacy void callbacks cannot ack.
type AcknowledgedUsagePlugin interface {
	HandleUsageAcknowledged(context.Context, UsageRecord) error
}

// HostUsageDeliveryRequest uses the calling native plugin's identity. There is
// deliberately no plugin ID, flush, wait, reset, or caller-supplied watermark.
type HostUsageDeliveryRequest struct {
	Provider string `json:"provider"`
}

// HostUsageDeliveryResponse is diagnostics-only until complete physical source
// lifecycle coverage is available. Through MUST be absent unless Coverage=known.
type HostUsageDeliveryResponse struct {
	Supported            bool                       `json:"supported"`
	Coverage             string                     `json:"coverage"`
	Reason               string                     `json:"reason"`
	Provider             string                     `json:"provider"`
	Through              *time.Time                 `json:"through,omitempty"`
	ManagerIncarnationID string                     `json:"manager_incarnation_id"`
	IncarnationID        string                     `json:"incarnation_id,omitempty"`
	Sequence             uint64                     `json:"sequence"`
	RegisteredAt         *time.Time                 `json:"registered_at,omitempty"`
	RegistrationSequence uint64                     `json:"registration_sequence"`
	Closed               bool                       `json:"closed"`
	Sources              HostUsageSourceDiagnostics `json:"sources"`
	Delivery             HostUsageSinkDiagnostics   `json:"delivery"`
}

type HostUsageSourceDiagnostics struct {
	Started                     uint64     `json:"started"`
	OpenReporters               uint64     `json:"open_reporters"`
	FinishedUnpublished         uint64     `json:"finished_unpublished"`
	EarliestReporterStart       *time.Time `json:"earliest_reporter_start,omitempty"`
	Queued                      uint64     `json:"queued"`
	Dispatching                 uint64     `json:"dispatching"`
	EarliestPendingStart        *time.Time `json:"earliest_pending_start,omitempty"`
	Published                   uint64     `json:"published"`
	UntrackedPublications       uint64     `json:"untracked_publications"`
	RejectedPublications        uint64     `json:"rejected_publications"`
	MissingSinkRecords          uint64     `json:"missing_sink_records"`
	TrackingOverflows           uint64     `json:"tracking_overflows"`
	InvalidSourceTimes          uint64     `json:"invalid_source_times"`
	UnknownProviderPublications uint64     `json:"unknown_provider_publications"`
}

// Counts accumulate for the named sink for this manager lifetime, including
// across registrations. Incarnation/registration fields delimit the new sink.
type HostUsageSinkDiagnostics struct {
	CallbackStarted          uint64 `json:"callback_started"`
	CallbackAcknowledged     uint64 `json:"callback_acknowledged"`
	CallbackErrors           uint64 `json:"callback_errors"`
	CallbackUnacknowledged   uint64 `json:"callback_unacknowledged"`
	CallbackMissing          uint64 `json:"callback_missing"`
	CallbackFused            uint64 `json:"callback_fused"`
	StaleCompletions         uint64 `json:"stale_completions"`
	PreRegistrationRecords   uint64 `json:"pre_registration_records"`
	RegistrationGaps         uint64 `json:"registration_gaps"`
	CallbackInFlight         uint64 `json:"callback_in_flight"`
	LastAcknowledgedSequence uint64 `json:"last_acknowledged_sequence"`
}
