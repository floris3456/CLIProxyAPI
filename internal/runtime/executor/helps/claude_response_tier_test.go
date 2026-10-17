package helps

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const tierUsageCounters = `"input_tokens":10,"output_tokens":4,"output_tokens_details":{"thinking_tokens":2},"cache_read_input_tokens":500,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}`

type claudeTierCapture struct {
	model   string
	records chan usage.Record
}

func (c claudeTierCapture) HandleUsage(_ context.Context, r usage.Record) {
	if c.records != nil && r.Model == c.model {
		c.records <- r
	}
}

func TestClaudeResponseTierParserBufferPublisher(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tier := range []string{"standard", "priority", "batch"} {
			t.Run(fmt.Sprintf("%v/%s", stream, tier), func(t *testing.T) {
				ctx := usage.WithServiceTier(context.Background(), "auto")
				capture := claudeTierCapture{t.Name(), make(chan usage.Record, 1)}
				usage.RegisterNamedPlugin(t.Name(), capture)
				t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), claudeTierCapture{}) })
				r := NewUsageReporter(ctx, "claude", t.Name(), nil)
				defer r.FinishUsageDelivery()
				payload := []byte(`{"usage":{` + tierUsageCounters + `,"service_tier":"` + tier + `"}}`)
				if stream {
					var b StreamUsageBuffer
					b.ObserveClaudeStream([]byte(`data: {"type":"message_start","message":` + string(payload) + `}`))
					b.ObserveClaudeStream([]byte(`data: {"type":"message_delta","usage":{"output_tokens":4}}`))
					if !b.Publish(ctx, r) {
						t.Fatal("missing stream publication")
					}
				} else {
					r.Publish(ctx, ParseClaudeUsage(payload))
				}
				select {
				case record := <-capture.records:
					if record.ServiceTier != "auto" || record.ResponseServiceTier != tier {
						t.Fatalf("assigned response tier lost in actual publisher: requested=%q assigned=%q", record.ServiceTier, record.ResponseServiceTier)
					}
					assertClaudeTierCounters(t, record.Detail)
				case <-time.After(5 * time.Second):
					t.Fatal("no published record")
				}
			})
		}
	}
}

func assertClaudeTierCounters(t *testing.T, d usage.Detail) {
	t.Helper()
	if d.InputTokens != 10 || d.OutputTokens != 4 || d.ReasoningTokens != 2 || d.CacheReadTokens != 500 || d.CacheCreationTokens != 100 || d.TotalTokens != 614 || d.CacheCreation == nil || !equalCounter(d.CacheCreation.Ephemeral5mInputTokens, cacheInt(25)) || !equalCounter(d.CacheCreation.Ephemeral1hInputTokens, cacheInt(75)) || d.TokenBreakdown.Output.NonReasoningTokens != 2 {
		t.Fatalf("tier changed independent accounting: %+v", d)
	}
}

func TestClaudeResponseTierUnknownAndConflicts(t *testing.T) {
	for _, field := range []string{"", `,"service_tier":null`, `,"service_tier":1`, `,"service_tier":false`, `,"service_tier":{}`, `,"service_tier":[]`, `,"service_tier":"auto"`, `,"service_tier":"standard_only"`, `,"service_tier":"default"`, `,"service_tier":"STANDARD"`, `,"service_tier":" standard "`, `,"service_tier":"future-tier"`} {
		d := ParseClaudeUsage([]byte(`{"service_tier":"standard","usage":{` + tierUsageCounters + field + `}}`))
		if d.ResponseServiceTier != "" {
			t.Fatalf("invalid/missing assigned tier became known: %s %+v", field, d)
		}
		assertClaudeTierCounters(t, d)
	}
	for _, bad := range []string{`"priority"`, `null`, `"STANDARD"`, `42`, `"future-tier"`} {
		t.Run(bad, func(t *testing.T) {
			var b StreamUsageBuffer
			b.ObserveClaudeStream([]byte(`data: {"message":{"usage":{` + tierUsageCounters + `,"service_tier":"standard"}}}`))
			b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":` + bad + `}}`))
			b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":"standard","output_tokens":4}}`))
			d, ok := b.Detail()
			if !ok || d.ResponseServiceTier != "" {
				t.Fatalf("conflicting/invalid tier healed to an arbitrary winner: %+v", d)
			}
			assertClaudeTierCounters(t, d)
		})
	}
}

func TestClaudeResponseTierOnlyUpdatesCannotInventAccounting(t *testing.T) {
	for _, suffix := range []string{"", `,"cache_creation":{}`, `,"output_tokens_details":{}`} {
		d := ParseClaudeUsage([]byte(`{"usage":{"service_tier":"standard"` + suffix + `}}`))
		if d.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified {
			t.Fatal("metadata-only container became complete zero usage", d)
		}
	}
	var b StreamUsageBuffer
	b.ObserveClaudeStream([]byte(`data: {"message":{"usage":{"service_tier":"standard"}}}`))
	d, ok := b.Detail()
	if !ok || d.ResponseServiceTier != "standard" || d.TokenBreakdown.Quality == usage.TokenAccountingQualityComplete {
		t.Fatalf("tier-only event became complete zero usage: %+v", d)
	}
	b.ObserveClaudeStream([]byte(`data: {"usage":{` + tierUsageCounters + `}}`))
	before, _ := b.Detail()
	b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":"standard"}}`))
	after, _ := b.Detail()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("tier-only update changed counters/splits/reasoning", before, after)
	}
	assertClaudeTierCounters(t, after)
	// An invalid accounting snapshot must not become complete through a metadata update.
	b.detail.TokenBreakdown = usage.NewUnclassifiedTokenBreakdown(614)
	before = b.detail
	b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":"standard"}}`))
	after, _ = b.Detail()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("tier-only event repaired accounting quality", before, after)
	}
}

func TestClaudeResponseTierAbsenceMalformedAndLateObservation(t *testing.T) {
	for _, first := range []string{"absent", "null", "invalid", "duplicate", "malformed"} {
		t.Run(first, func(t *testing.T) {
			var b StreamUsageBuffer
			suffix := ""
			switch first {
			case "null":
				suffix = `,"service_tier":null`
			case "invalid":
				suffix = `,"service_tier":"Standard"`
			case "duplicate":
				suffix = `,"service_tier":"standard","service_tier":"priority"`
			}
			b.ObserveClaudeStream([]byte(`data: {"message":{"usage":{` + tierUsageCounters + suffix + `}}}`))
			if first == "malformed" {
				b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":"standard"`))
			}
			b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":"standard"}}`))
			d, _ := b.Detail()
			want := ""
			if first == "absent" {
				want = "standard"
			}
			if d.ResponseServiceTier != want {
				t.Fatal("optional observation/invalidity boundary", first, d.ResponseServiceTier, want)
			}
			assertClaudeTierCounters(t, d)
		})
	}
	for _, raw := range []string{`{"usage":{"service_tier":"standard"`, `{"usage":{"service_tier":"standard","service_tier":"priority"}}`} {
		if d := ParseClaudeUsage([]byte(raw)); d.ResponseServiceTier != "" {
			t.Fatal("malformed/ambiguous tier became known", d)
		}
	}
	// The actual publisher must also keep metadata-only accounting unclassified.
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx := usage.WithServiceTier(context.Background(), "auto")
			capture := claudeTierCapture{t.Name(), make(chan usage.Record, 1)}
			usage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), claudeTierCapture{}) })
			r := NewUsageReporter(ctx, "claude", t.Name(), nil)
			defer r.FinishUsageDelivery()
			if stream {
				var b StreamUsageBuffer
				b.ObserveClaudeStream([]byte(`data: {"usage":{"service_tier":"standard"}}`))
				b.Publish(ctx, r)
			} else {
				r.Publish(ctx, ParseClaudeUsage([]byte(`{"usage":{"service_tier":"standard"}}`)))
			}
			select {
			case record := <-capture.records:
				if record.ResponseServiceTier != "standard" || record.Detail.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified {
					t.Fatal("metadata-only publication claimed complete counters", record)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing metadata-only record")
			}
		})
	}
}
