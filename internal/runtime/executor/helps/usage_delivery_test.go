package helps

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// These exercise the real reporter wiring; no fake completion is inferred from
// latency, terminal usage, cancellation or lack of tokens.
func TestPacerDeliveryReporterFinalUsageAndPhysicalFinish(t *testing.T) {
	m := usage.DefaultManager()
	before := m.DeliverySnapshot("unused-test-sink", "claude")
	r := NewUsageReporter(context.Background(), "claude", "delivery-test", nil)
	started := m.DeliverySnapshot("unused-test-sink", "claude")
	if started.Sources.Started != before.Sources.Started+1 || started.Sources.OpenReporters != before.Sources.OpenReporters+1 {
		t.Fatal("real reporter start not tracked")
	}
	r.Publish(context.Background(), usage.Detail{InputTokens: 4})
	r.EnsurePublished(context.Background())
	published := m.DeliverySnapshot("unused-test-sink", "claude")
	if published.Sources.Published != before.Sources.Published+1 || published.Sources.OpenReporters != started.Sources.OpenReporters {
		t.Fatal("final usage released physical guard or published twice")
	}
	if err := r.FinishPolicyAttempt(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	finished := m.DeliverySnapshot("unused-test-sink", "claude")
	if finished.Sources.OpenReporters != before.Sources.OpenReporters || finished.Through != nil || finished.Supported {
		t.Fatal("physical finish/unsupported coverage incorrect")
	}
}

func TestPacerDeliveryReporterCanceledTruncatedPartialUsage(t *testing.T) {
	m := usage.DefaultManager()
	before := m.DeliverySnapshot("unused-test-sink", "codex")
	r := NewUsageReporter(context.Background(), "codex", "delivery-test", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.PublishFailureWithDetail(ctx, usage.Detail{InputTokens: 7, OutputTokens: 3}, context.Canceled)
	if s := m.DeliverySnapshot("unused-test-sink", "codex"); s.Sources.OpenReporters != before.Sources.OpenReporters+1 {
		t.Fatal("partial failure erased physical guard")
	}
	err := context.Canceled
	r.TrackFailure(ctx, &err)
	s := m.DeliverySnapshot("unused-test-sink", "codex")
	if s.Sources.OpenReporters != before.Sources.OpenReporters || s.Sources.Published != before.Sources.Published+1 || s.Through != nil {
		t.Fatal("failure tracking duplicated or lost final record")
	}
}
