package helps

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type physicalAck struct{}

func (physicalAck) HandleUsage(context.Context, usage.Record)                   {}
func (physicalAck) HandleUsageAcknowledged(context.Context, usage.Record) error { return nil }

func TestPacerPhysicalGuardChecksConcreteTransportCredentialAndScope(t *testing.T) {
	for _, mode := range []string{"valid", "token-changed", "wire-trim", "two-credentials", "missing-credential", "custom-transport", "idempotency", "gateway", "spoofed-executor"} {
		t.Run(mode, func(t *testing.T) {
			m := usage.DefaultManager()
			name := t.Name()
			m.RegisterNamed(name, physicalAck{})
			a := &auth.Auth{ID: name, Index: name, Provider: "claude", Attributes: map[string]string{"api_key": "dummy-key"}}
			r := NewClaudeHTTPUsageReporter(context.Background(), sourceExecutorIdentity{}, "physical-test", a, "https://api.anthropic.com/v1/messages")
			// Exercise boundary checks in isolation. Production sets these private
			// fields only after the concrete built-in type check; no network is run.
			r.physicalBuiltin = mode != "spoofed-executor"
			r.physicalCredentialSet = true
			r.physicalCredential = sha256.Sum256([]byte("dummy-key"))
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader("dummy"))
			req.Header.Set("X-Api-Key", "dummy-key")
			client := NewUtlsHTTPClient(context.Background(), &config.Config{}, a, 0)
			var base http.RoundTripper = client.Transport
			switch mode {
			case "wire-trim":
				r.physicalCredential = sha256.Sum256([]byte(" dummy-key "))
				req.Header.Set("X-Api-Key", " dummy-key ")
			case "token-changed":
				req.Header.Set("X-Api-Key", "changed-key")
			case "two-credentials":
				req.Header.Set("Authorization", "Bearer dummy-key")
			case "missing-credential":
				req.Header.Del("X-Api-Key")
			case "custom-transport":
				base = http.DefaultTransport
			case "idempotency":
				req.Header.Set("Idempotency-Key", "retry")
			case "gateway":
				req.Host = "gateway.invalid"
			}
			scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
			before := m.SourceCheckpoint(name, scope).Physical.GapCount
			guard := r.guardPhysicalDispatch(req, base)
			if guard == nil {
				t.Fatal("source dispatch not retained")
			}
			after := m.SourceCheckpoint(name, scope).Physical.GapCount
			if mode == "valid" && after != before || mode != "valid" && after <= before {
				t.Fatalf("qualification %s before=%d after=%d", mode, before, after)
			}
			guard.RequestBodyClosed(false)
			guard.RoundTripReturned(false, false)
			r.Publish(context.Background(), usage.Detail{InputTokens: 1})
			r.FinishUsageDelivery()
		})
	}
}

type physicalBlockingClose struct{ entered, release chan struct{} }

func (*physicalBlockingClose) Read([]byte) (int, error) { return 0, io.EOF }
func (b *physicalBlockingClose) Close() error           { close(b.entered); <-b.release; return nil }

func TestPacerPhysicalBodyGuardWaitsForActualCloseReturn(t *testing.T) {
	base := &physicalBlockingClose{entered: make(chan struct{}), release: make(chan struct{})}
	var closed atomic.Bool
	b := &physicalSourceBody{ReadCloser: base, close: func(failed bool) {
		if failed {
			t.Error("unexpected failure")
		}
		closed.Store(true)
	}}
	done := make(chan struct{})
	go func() { defer close(done); _ = b.Close() }()
	select {
	case <-base.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("close not entered")
	}
	if closed.Load() {
		t.Fatal("guard closed before underlying body")
	}
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); _ = b.Close() }()
	close(base.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close stuck")
	}
	if !closed.Load() {
		t.Fatal("guard was not closed")
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("repeated close did not share completion")
	}
}

type physicalReplayTransport func(*http.Request) (*http.Response, error)

func (f physicalReplayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPacerPhysicalTransportGetBodyInvalidatesBeforeReturningReplacement(t *testing.T) {
	m := usage.DefaultManager()
	name := t.Name()
	m.RegisterNamed(name, physicalAck{})
	a := &auth.Auth{ID: name, Index: name, Provider: "claude"}
	r := NewClaudeHTTPUsageReporter(context.Background(), sourceExecutorIdentity{}, "physical-replay", a, "https://api.anthropic.com/v1/messages")
	scope := pluginapi.UsageSourceScope{Provider: "claude", AuthID: a.ID, AuthIndex: a.Index, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}
	transport := physicalReplayTransport(func(req *http.Request) (*http.Response, error) {
		before := m.SourceCheckpoint(name, scope).Physical.GapCount
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		if after := m.SourceCheckpoint(name, scope).Physical.GapCount; after <= before {
			t.Error("replacement body escaped physical invalidation")
		}
		_ = body.Close()
		_ = req.Body.Close()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: req}, nil
	})
	client := r.TrackHTTPClient(&http.Client{Transport: transport})
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader("dummy-body"))
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	r.Publish(context.Background(), usage.Detail{InputTokens: 1})
	r.FinishUsageDelivery()
	s := m.SourceCheckpoint(name, scope)
	if s.Physical.Coverage != "unknown" || s.Physical.ThroughCut != nil {
		t.Fatal("replayed transport certified", s.Physical)
	}
}
