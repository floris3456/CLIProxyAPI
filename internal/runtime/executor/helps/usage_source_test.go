package helps

import (
	"context"
	"net/http"
	"testing"
	"time"

	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type sourceCapture struct{ records chan usage.Record }

type sourceIgnore struct{}

func (sourceIgnore) HandleUsage(context.Context, usage.Record) {}

func (c *sourceCapture) HandleUsage(_ context.Context, r usage.Record) {
	if r.Model == "source-marker-test" {
		c.records <- r
	}
}

type sourceExecutorIdentity struct{}

func (sourceExecutorIdentity) Identifier() string { return "claude" }

func TestPacerSourceReporterUnmanagedSummaryAndPartialUsageMarker(t *testing.T) {
	capture := &sourceCapture{records: make(chan usage.Record, 8)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), sourceIgnore{}) })
	for _, stream := range []bool{false, true} {
		a := &auth.Auth{ID: t.Name(), Index: "index-source-marker", Provider: "claude"}
		r := NewClaudeHTTPUsageReporter(context.Background(), sourceExecutorIdentity{}, "source-marker-test", a, "https://api.anthropic.com/v1/messages?beta=true")
		r.SetStream(stream)
		req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
		r.markSourceDispatch(req) // the actual RoundTrip wrapper calls this before transport
		r.PublishFailureWithDetail(context.Background(), usage.Detail{InputTokens: 7, OutputTokens: 2}, context.Canceled)
		r.FinishUsageDelivery()
		select {
		case rec := <-capture.records:
			e := rec.SourceEvidence
			if e == nil || !e.Tracked || e.Scope.Kind != pluginapi.ClaudeHTTPUsageSource || e.Scope.Version != 1 || e.Scope.AuthID != a.ID || e.Scope.AuthIndex != a.Index || e.UpstreamStartedAt == nil || e.UpstreamStartedAt.Before(e.RequestedAt) || e.FinalUsageAt.Before(*e.UpstreamStartedAt) {
				t.Fatalf("source marker=%+v", e)
			}
			if rec.CachePolicy != nil || rec.Detail.InputTokens != 7 || !rec.Failed || rec.Stream != stream {
				t.Fatalf("marker changed unmanaged/partial usage=%+v", rec)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing source record")
		}
	}
}

func TestPacerSourceReporterRedirectAndThirdPartyAreGaps(t *testing.T) {
	capture := &sourceCapture{records: make(chan usage.Record, 8)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), sourceIgnore{}) })
	a := &auth.Auth{ID: t.Name(), Index: "index-source-redirect", Provider: "claude"}
	r := NewClaudeHTTPUsageReporter(context.Background(), sourceExecutorIdentity{}, "source-marker-test", a, "https://api.anthropic.com/v1/messages")
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	r.markSourceDispatch(req)
	redirect, _ := http.NewRequest(http.MethodPost, "https://untrusted.invalid/v1/messages", nil)
	r.markSourceDispatch(redirect)
	r.Publish(context.Background(), usage.Detail{InputTokens: 1})
	r.FinishUsageDelivery()
	select {
	case rec := <-capture.records:
		if rec.SourceEvidence == nil || rec.SourceEvidence.Gap == nil || rec.SourceEvidence.Gap.Reason != "multiple_dispatch" {
			t.Fatalf("redirect lacked gap=%+v", rec.SourceEvidence)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing redirected record")
	}
	r = NewClaudeHTTPUsageReporter(context.Background(), sourceExecutorIdentity{}, "source-marker-test", a, "http://127.0.0.1:1/v1/messages")
	r.Publish(context.Background(), usage.Detail{})
	r.FinishUsageDelivery()
	select {
	case rec := <-capture.records:
		if rec.SourceEvidence == nil || rec.SourceEvidence.Tracked || rec.SourceEvidence.Gap == nil {
			t.Fatalf("third party acquired known source=%+v", rec.SourceEvidence)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing third-party record")
	}
}

func TestPacerSourceReporterMissingUsageCannotCertifyZero(t *testing.T) {
	capture := &sourceCapture{records: make(chan usage.Record, 1)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), sourceIgnore{}) })
	a := &auth.Auth{ID: t.Name(), Index: "source-missing-usage", Provider: "claude"}
	r := NewClaudeHTTPUsageReporter(context.Background(), sourceExecutorIdentity{}, "source-marker-test", a, "https://api.anthropic.com/v1/messages")
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	r.markSourceDispatch(req)
	r.EnsurePublished(context.Background())
	r.FinishUsageDelivery()
	select {
	case rec := <-capture.records:
		if rec.SourceEvidence == nil || rec.SourceEvidence.Gap == nil || rec.SourceEvidence.Gap.Reason != "usage_unavailable_or_zero_only" {
			t.Fatalf("missing usage certified as zero: %+v", rec.SourceEvidence)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing unknown-usage record")
	}
}
