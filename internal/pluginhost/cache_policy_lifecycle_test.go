package pluginhost

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type pacerCompletionFixture struct {
	active     atomic.Bool
	completion chan bool
}

func (p *pacerCompletionFixture) HandleRequestComplete(ctx context.Context, _ pluginapi.RequestCompletion) error {
	p.completion <- p.active.Load() || ctx.Err() != nil
	return nil
}

func TestPacerCompletionWaitsForPhysicalShutdown(t *testing.T) {
	h := New()
	p := &pacerCompletionFixture{completion: make(chan bool, 1)}
	p.active.Store(true)
	h.RegisterPluginForTest("pacer", pluginapi.Plugin{SchemaVersion: 7, Capabilities: pluginapi.Capabilities{RequestLifecyclePlugin: p}})
	h.trackCachePolicyRequest("logical")
	h.trackCachePolicyAttempt("logical", "physical")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.CompleteRequestExcept(ctx, pluginapi.RequestCompletion{RequestID: "logical", Outcome: pluginapi.RequestCompletionCanceled}, "pacer")
	h.mu.Lock()
	queued := h.cachePolicyRequests["logical"].completion != nil
	h.mu.Unlock()
	if !queued {
		t.Fatal("completion did not wait for stopped transport")
	}
	if h.trackCachePolicyAttempt("logical", "late") {
		t.Fatal("attempt admitted after completion was queued")
	}
	p.active.Store(false)
	h.endCachePolicyAttempt("logical", "physical")
	select {
	case early := <-p.completion:
		if early {
			t.Fatal("completion ran with active transport or canceled callback context")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no settled completion")
	}
	h.mu.Lock()
	_, retained := h.cachePolicyRequests["logical"]
	h.mu.Unlock()
	if retained {
		t.Fatal("settled process-local guard leaked")
	}
	if h.trackCachePolicyAttempt("logical", "later") {
		t.Fatal("attempt admitted after logical completion")
	}
}
