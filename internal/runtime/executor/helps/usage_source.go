package helps

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func firstPartyClaudeURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "api.anthropic.com" && u.User == nil && (u.Path == "" || u.Path == "/" || u.Path == "/v1/messages")
}

// Used only by the two built-in Claude generation entry points. Unknown
// executors cannot acquire source proof by setting a usage record's marker.
func NewClaudeHTTPUsageReporter(ctx context.Context, executor usageExecutor, model string, auth *cliproxyauth.Auth, upstream string, selectedCredential ...string) *UsageReporter {
	if auth == nil || auth.Provider != "claude" || !firstPartyClaudeURL(upstream) {
		return NewExecutorUsageReporter(ctx, executor, model, auth)
	}
	m := usage.DefaultManager()
	ticket, start := m.BeginClaudeSource(ctx, auth.ID, auth.EnsureIndex(), true)
	if ctx != nil && ctx.Value("cliproxy.roundtripper") != nil {
		m.NoteSourceGap("claude", auth.ID, auth.Index, "custom_transport", start)
	}
	r := newUsageReporter(ctx, "claude", model, auth, ticket, start)
	r.executorType = ExecutorTypeName(executor)
	t := reflect.TypeOf(executor)
	r.physicalBuiltin = t != nil && t.Kind() == reflect.Pointer && t.Elem().PkgPath() == "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor" && t.Elem().Name() == "ClaudeExecutor"
	if r.physicalBuiltin && len(selectedCredential) == 1 && selectedCredential[0] != "" {
		r.physicalCredentialSet = true
		r.physicalCredential = sha256.Sum256([]byte(selectedCredential[0]))
	}
	return r
}

type physicalDispatchPermit struct{}

// Positive qualification comes from concrete built-in identity, a host-created
// terminal transport and the frozen selected credential, never usage JSON.
func (r *UsageReporter) guardPhysicalDispatch(req *http.Request, base http.RoundTripper) *usage.SourceDispatch {
	if r == nil || r.delivery == nil || req == nil {
		return nil
	}
	transport, ok := base.(*fallbackRoundTripper)
	qualified := ok && transport.physicalBuiltin && r.physicalBuiltin
	if qualified {
		_, qualified = transport.anthropic.(*http.Transport)
	}
	qualified = qualified && req.Method == http.MethodPost && firstPartyClaudeURL(req.URL.String()) && req.URL.Path == "/v1/messages" && (req.Host == "" || req.Host == "api.anthropic.com")
	credentials := 0
	credentialMatches := r.physicalCredentialSet
	for key, values := range req.Header {
		if strings.EqualFold(key, "Idempotency-Key") || strings.EqualFold(key, "X-Idempotency-Key") {
			qualified = false
		}
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-Api-Key") {
			for _, value := range values {
				credentials++
				// net/http trims field-value whitespace on the wire. A matching
				// pre-serialization hash must not silently bind a different key.
				if value != strings.TrimSpace(value) {
					credentialMatches = false
				}
				if strings.EqualFold(key, "Authorization") {
					if !strings.HasPrefix(value, "Bearer ") {
						credentialMatches = false
					}
					value = strings.TrimPrefix(value, "Bearer ")
				}
				if sha256.Sum256([]byte(value)) != r.physicalCredential {
					credentialMatches = false
				}
			}
		}
	}
	credentialMatches = credentialMatches && credentials == 1
	if r.physicalBuiltin && !credentialMatches {
		usage.DefaultManager().NotePhysicalIdentityGap()
	}
	qualified = qualified && credentialMatches
	var permit any
	if qualified {
		permit = physicalDispatchPermit{}
	}
	return r.delivery.BeginPhysicalDispatch(pluginapi.UsageSourceScope{Provider: r.provider, AuthID: r.authID, AuthIndex: r.authIndex, Kind: pluginapi.ClaudeHTTPUsageSource, Version: 1}, permit, req.Body != nil && req.Body != http.NoBody)
}

// Report only after the underlying Close returns. net/http may finish outgoing
// request-body work asynchronously after RoundTrip has returned.
type physicalSourceBody struct {
	io.ReadCloser
	once  sync.Once
	close func(bool)
	err   error
}

func (b *physicalSourceBody) Close() error {
	// Concurrent/repeated closes share the actual completion, so a second
	// no-op Close cannot retire the guard while the first Close is still busy.
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.close(b.err != nil)
	})
	return b.err
}

func (r *UsageReporter) markSourceDispatch(req *http.Request) {
	if r == nil || r.delivery == nil || req == nil {
		return
	}
	safe := req.Method == http.MethodPost && firstPartyClaudeURL(req.URL.String()) && req.URL.Path == "/v1/messages"
	// Idempotency permits hidden net/http retries of a replayable POST body.
	for k := range req.Header {
		if strings.EqualFold(k, "Idempotency-Key") || strings.EqualFold(k, "X-Idempotency-Key") {
			safe = false
		}
	}
	r.delivery.MarkSourceDispatch(safe)
}

// MarkUsageSourceIncomplete preserves missing protocol-completion evidence even
// when legacy unmanaged response forwarding treats a clean EOF as success.
func (r *UsageReporter) MarkUsageSourceIncomplete() {
	if r != nil {
		r.delivery.FailSource("incomplete_claude_response")
	}
}

func NoteUnsupportedClaudeHTTP(auth *cliproxyauth.Auth, reason string) {
	if auth == nil {
		usage.DefaultManager().NoteSourceGap("claude", "", "", reason, time.Now())
		return
	}
	usage.DefaultManager().NoteSourceGap("claude", auth.ID, auth.EnsureIndex(), reason, time.Now())
}
