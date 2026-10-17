package usage

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type pacerObserver func(Record)

func (p pacerObserver) HandleUsage(_ context.Context, r Record) { p(r) }

func TestPacerUsageManagerImmutableBetweenObservers(t *testing.T) {
	m := NewManager(1)
	t.Cleanup(m.Stop)
	gate := make(chan struct{})
	received := make(chan Record, 1)
	m.Register(pacerObserver(func(r Record) {
		<-gate
		r.CachePolicy.Selected.TTL = "mutated"
		*r.Detail.CacheCreation.Ephemeral1hInputTokens = 999
	}))
	m.Register(pacerObserver(func(r Record) { received <- r }))
	n := int64(75)
	r := Record{RequestID: "attempt", LifecycleRequestID: "logical", CachePolicy: &pluginapi.CachePolicyEvidence{Selected: &pluginapi.CachePolicy{TTL: "1h", ContextEpoch: "old"}}, Detail: Detail{CacheCreation: &pluginapi.CacheCreationUsage{Ephemeral1hInputTokens: &n}}}
	m.Publish(context.Background(), r)
	r.CachePolicy.Selected.TTL = "new"
	n = 1000
	close(gate)
	select {
	case got := <-received:
		if got.CachePolicy.Selected.TTL != "1h" || *got.Detail.CacheCreation.Ephemeral1hInputTokens != 75 || got.LifecycleRequestID != "logical" {
			t.Fatalf("snapshot mutated: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no usage")
	}
}
