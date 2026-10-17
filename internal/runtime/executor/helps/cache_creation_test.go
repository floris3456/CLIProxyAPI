package helps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPacerCacheCreationSnapshots(t *testing.T) {
	for _, tt := range []struct {
		name, raw, quality string
		w5, w1, unknown    *int64
	}{
		{"missing split", `{"cache_creation_input_tokens":100}`, "partial", nil, nil, cacheInt(100)},
		{"zero split", `{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":0}}`, "partial", cacheInt(0), nil, cacheInt(100)},
		{"complete mix", `{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}}`, "complete", cacheInt(25), cacheInt(75), cacheInt(0)},
		{"exceeds total", `{"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":25}}`, "inconsistent", cacheInt(25), nil, nil},
		{"negative", `{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_1h_input_tokens":-1}}`, "inconsistent", nil, cacheInt(-1), nil},
		{"no total", `{"cache_creation":{"ephemeral_1h_input_tokens":75}}`, "unclassified", nil, cacheInt(75), nil},
		{"invalid counter", `{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":"25"}}`, "inconsistent", nil, nil, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := ParseClaudeUsage([]byte(`{"usage":` + tt.raw + `}`))
			s := d.CacheCreation
			if s == nil || s.Quality != tt.quality || !equalCounter(s.Ephemeral5mInputTokens, tt.w5) || !equalCounter(s.Ephemeral1hInputTokens, tt.w1) || !equalCounter(s.UnknownTokens, tt.unknown) {
				t.Fatalf("split=%+v", s)
			}
		})
	}
	var buffer StreamUsageBuffer
	buffer.ObserveClaudeStream([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}}}}`))
	delta := []byte(`data: {"type":"message_delta","usage":{"output_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":25}}}`)
	buffer.ObserveClaudeStream(delta)
	buffer.ObserveClaudeStream(delta)
	d, _ := buffer.Detail()
	if d.TotalTokens != 114 || *d.CacheCreation.Ephemeral1hInputTokens != 75 || *d.CacheCreation.UnknownTokens != 0 {
		t.Fatalf("duplicate/partial snapshot merge=%+v", d)
	}
	buffer.ObserveClaudeStream([]byte(`data: {"usage":{"cache_creation_input_tokens":75,"cache_creation":{"ephemeral_5m_input_tokens":0}}}`))
	d, _ = buffer.Detail()
	if d.CacheCreationTokens != 75 || *d.CacheCreation.Ephemeral5mInputTokens != 0 || d.CacheCreation.Quality != "complete" {
		t.Fatalf("explicit zero was lost=%+v", d.CacheCreation)
	}
}
func cacheInt(v int64) *int64 { return &v }
func equalCounter(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type pacerUsageCapture struct{ records chan usage.Record }

func (p pacerUsageCapture) HandleUsage(_ context.Context, r usage.Record) {
	if r.Model == "pacer-usage-test" {
		p.records <- r
	}
}
func TestPacerPhysicalAttemptSnapshotAndPartialFailure(t *testing.T) {
	c := pacerUsageCapture{make(chan usage.Record, 4)}
	usage.RegisterNamedPlugin("test-pacer-attempt", c)
	t.Cleanup(func() { usage.RegisterNamedPlugin("test-pacer-attempt", pacerUsageCapture{make(chan usage.Record, 4)}) })
	policy := &pluginapi.CachePolicy{TTL: "1h", Revision: 1, ContextEpoch: "old"}
	first := NewUsageReporter(context.Background(), "claude", "pacer-usage-test", nil)
	first.SetExecutionPolicy(executor.Options{LifecycleRequestID: "logical", CachePolicy: policy})
	second := NewUsageReporter(context.Background(), "claude", "pacer-usage-test", nil)
	second.SetExecutionPolicy(executor.Options{LifecycleRequestID: "logical", CachePolicy: policy})
	policy.TTL = "5m"
	policy.ContextEpoch = "new"
	var b StreamUsageBuffer
	b.ObserveClaudeStream([]byte(`data: {"message":{"usage":{"input_tokens":11,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}}}}`))
	b.PublishFailure(context.Background(), first, context.Canceled)
	first.EnsurePublished(context.Background())
	second.Publish(context.Background(), ParseClaudeUsage([]byte(`{"usage":{"input_tokens":1,"cache_creation_input_tokens":3}}`)))
	var records []usage.Record
	for range 2 {
		select {
		case r := <-c.records:
			records = append(records, r)
		case <-time.After(3 * time.Second):
			t.Fatal("missing usage")
		}
	}
	if records[0].RequestID == records[1].RequestID || records[0].LifecycleRequestID != "logical" || records[1].LifecycleRequestID != "logical" {
		t.Fatal("physical attempt correlation")
	}
	if !records[0].Failed || *records[0].Detail.CacheCreation.Ephemeral1hInputTokens != 75 {
		t.Fatal("partial usage lost")
	}
	for _, r := range records {
		if r.CachePolicy.Selected.TTL != "1h" || r.CachePolicy.Selected.ContextEpoch != "old" {
			t.Fatal("late policy mutation")
		}
	}
	raw, _ := json.Marshal(records[1].Detail.CacheCreation)
	if strings.Contains(string(raw), "ephemeral_1h") {
		t.Fatal("selection fabricated reported split")
	}
	select {
	case <-c.records:
		t.Fatal("duplicate reporter publication")
	default:
	}
}
