package helps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const extendedCacheBeta = "extended-cache-ttl-2025-04-11"

// EnforceClaudeCachePolicy is called on the finished body, immediately before CCH signing.
// Only official Anthropic and loopback test upstreams implement the initial contract.
func EnforceClaudeCachePolicy(body []byte, opts executor.Options, upstream string) ([]byte, error) {
	if opts.CachePolicy == nil {
		if cachepolicy.Managed(opts.Headers) {
			return nil, cachepolicy.Unavailable()
		}
		return body, nil
	}
	if err := cachepolicy.Validate(*opts.CachePolicy); err != nil {
		return nil, err
	}
	u, err := url.Parse(upstream)
	if err != nil || u.User != nil || !((u.Scheme == "https" && u.Host == "api.anthropic.com") || (net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback() && (u.Scheme == "http" || u.Scheme == "https"))) {
		return nil, cachepolicy.Unsupported()
	}
	if !gjson.ValidBytes(body) || IsClaudeProbeOrHelperRequest(body) {
		return nil, cachepolicy.Unsupported()
	}
	paths, err := claudeCacheMarkerPaths(body)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		body, err = sjson.SetBytes(body, path+".ttl", opts.CachePolicy.TTL)
		if err != nil {
			return nil, fmt.Errorf("enforce Claude cache policy: %w", err)
		}
	}
	return body, nil
}

// Inspect only actual API cache locations; never rewrite cache_control properties in
// tool schemas, tool input, or JSON text. Top-level automatic caching is also covered.
func claudeCacheMarkerPaths(body []byte) ([]string, error) {
	paths := make([]string, 0, 5)
	var invalid bool
	add := func(path string) {
		v := gjson.GetBytes(body, path)
		if !v.Exists() {
			return
		}
		if !v.IsObject() || v.Get("type").String() != "ephemeral" {
			invalid = true
			return
		}
		paths = append(paths, path)
	}
	add("cache_control")
	for _, section := range []string{"tools", "system"} {
		for i := range gjson.GetBytes(body, section).Array() {
			add(section + "." + strconv.Itoa(i) + ".cache_control")
		}
	}
	for i, message := range gjson.GetBytes(body, "messages").Array() {
		for j := range message.Get("content").Array() {
			add(fmt.Sprintf("messages.%d.content.%d.cache_control", i, j))
		}
	}
	if invalid || len(paths) > 4 {
		return nil, cachepolicy.Unsupported()
	}
	return paths, nil
}

// Managed dispatch cannot silently redirect to an unvalidated provider or create
// multiple billed attempts behind a single usage reporter.
func CachePolicyHTTPClient(client *http.Client, opts executor.Options) *http.Client {
	if opts.CachePolicy == nil || client == nil {
		return client
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copyClient
}

// FinalizeClaudeCachePolicyHeaders validates the final signed body and pairs the wire header.
// It also captures unmanaged wire evidence without imposing a TTL on existing clients.
func FinalizeClaudeCachePolicyHeaders(req *http.Request, body []byte, opts executor.Options, reporter *UsageReporter) error {
	for k := range req.Header {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(cachepolicy.HeaderPrefix)) {
			delete(req.Header, k)
		}
	}
	paths, err := claudeCacheMarkerPaths(body)
	if err != nil && opts.CachePolicy != nil {
		return err
	}
	evidence := &pluginapi.CachePolicyEvidence{Selected: cachepolicy.Clone(opts.CachePolicy), BreakpointCount: len(paths)}
	if reporter != nil {
		evidence.AttemptStartedAt = reporter.policyAttemptStartedAt
	}
	for _, path := range paths {
		ttl := gjson.GetBytes(body, path+".ttl").String()
		if ttl == "" {
			ttl = "5m"
		}
		if opts.CachePolicy != nil && ttl != opts.CachePolicy.TTL {
			return cachepolicy.Unsupported()
		}
		if evidence.EffectiveTTL == "" {
			evidence.EffectiveTTL = ttl
		} else if evidence.EffectiveTTL != ttl {
			evidence.EffectiveTTL = "mixed"
		}
	}
	if opts.CachePolicy != nil && opts.CachePolicy.TTL == "1h" && len(paths) > 0 {
		beta := req.Header.Get("Anthropic-Beta")
		found := false
		for _, v := range strings.Split(beta, ",") {
			if strings.TrimSpace(v) == extendedCacheBeta {
				found = true
			}
		}
		if !found {
			if beta != "" {
				beta += ","
			}
			req.Header.Set("Anthropic-Beta", beta+extendedCacheBeta)
		}
	}
	// Frozen before dispatch: the snapshot sees the same final body and headers that will be sent.
	evidence.Prefix = claudePrefixSnapshot(body, req.Header)
	if reporter != nil {
		reporter.cachePolicy = evidence
	}
	return nil
}

// MarkCachePolicyDispatch freezes request-start evidence before transport begins.
func (r *UsageReporter) MarkCachePolicyDispatch() {
	if r != nil && r.cachePolicy != nil {
		r.cachePolicy.Dispatched = true
		r.cachePolicy.UpstreamStartedAt = time.Now()
	}
}

// SetLifecycleRequestID binds usage to the host request without selecting a cache policy.
func (r *UsageReporter) SetLifecycleRequestID(id string) {
	if r != nil {
		r.lifecycleRequestID = id
	}
}

func (r *UsageReporter) SetExecutionPolicy(opts executor.Options) {
	if r == nil {
		return
	}
	r.lifecycleRequestID = opts.LifecycleRequestID
	r.policyAttempt = opts.CachePolicyAttempt
	if opts.CachePolicy != nil {
		r.cachePolicy = &pluginapi.CachePolicyEvidence{Selected: cachepolicy.Clone(opts.CachePolicy)}
	}
}

// BeginPolicyAttempt owns a fresh physical UUID even when a logical request retries.
func (r *UsageReporter) BeginPolicyAttempt(ctx context.Context) error {
	if r == nil || r.cachePolicy == nil || r.cachePolicy.Selected == nil {
		return nil
	}
	if r.policyAttempt == nil {
		return cachepolicy.Unavailable()
	}
	resp, err := r.policyAttempt(ctx, r.policyAttemptRequest("begin", ""))
	if err != nil {
		return err
	}
	if resp.Policy != *r.cachePolicy.Selected || resp.StartedAt.IsZero() {
		return cachepolicy.Unavailable()
	}
	r.policyAttemptStarted = true
	r.policyAttemptStartedAt = resp.StartedAt
	r.cachePolicy.AttemptStartedAt = resp.StartedAt
	return nil
}

func (r *UsageReporter) policyAttemptRequest(operation, outcome string) pluginapi.CachePolicyAttemptRequest {
	return pluginapi.CachePolicyAttemptRequest{Operation: operation, RequestID: r.lifecycleRequestID, AttemptID: r.requestID,
		Policy: *r.cachePolicy.Selected, StartedAt: r.policyAttemptStartedAt, Outcome: outcome, Dispatched: r.cachePolicy.Dispatched}
}

func (r *UsageReporter) ValidatePolicyAttempt(ctx context.Context) error {
	if r == nil || r.cachePolicy == nil || r.cachePolicy.Selected == nil {
		return nil
	}
	if !r.policyAttemptStarted {
		return cachepolicy.Unavailable()
	}
	_, err := r.policyAttempt(ctx, r.policyAttemptRequest("validate", ""))
	return err
}

// FinishPolicyAttempt runs only once dispatch has returned and the upstream body has closed.
// Failed settlement leaves the plugin's durable guard outstanding and surfaces an error.
func (r *UsageReporter) FinishPolicyAttempt(ctx context.Context, executionErr error) error {
	defer r.FinishUsageDelivery()
	if r != nil && (executionErr != nil || (ctx != nil && ctx.Err() != nil)) {
		r.delivery.FailSource("producer_error_or_cancellation")
	}
	if r == nil || !r.policyAttemptStarted || r.policyAttemptFinished.Swap(true) {
		return nil
	}
	outcome := "succeeded"
	if executionErr != nil || r.policyAttemptFailed.Load() {
		outcome = "failed"
	}
	if errors.Is(executionErr, context.Canceled) || ctx.Err() != nil {
		outcome = "canceled"
	}
	_, err := r.policyAttempt(context.WithoutCancel(ctx), r.policyAttemptRequest("finish", outcome))
	return err
}
