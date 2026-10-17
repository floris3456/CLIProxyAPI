package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type cachePolicyHost interface {
	ResolveCachePolicy(context.Context, pluginapi.CachePolicyResolveRequest) (pluginapi.CachePolicy, error)
	CachePolicyAttempt(context.Context, pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error)
}

func (h *BaseAPIHandler) resolveCachePolicy(ctx context.Context, requestID, model string, opts coreexecutor.Options) (coreexecutor.Options, *interfaces.ErrorMessage) {
	opts.LifecycleRequestID = requestID
	if !cachepolicy.Managed(opts.Headers) {
		return opts, nil
	}
	if opts.CachePolicyProvider != "claude" || helps.IsClaudeProbeOrHelperRequest(opts.OriginalRequest) {
		return opts, executionErrorMessage(cachepolicy.Unsupported())
	}
	if h.AuthManager != nil && h.AuthManager.HomeEnabled() {
		return opts, executionErrorMessage(cachepolicy.Unsupported())
	}
	// Use authenticated HTTP context, never a client-supplied metadata scope or path.
	c, _ := ctx.Value("gin").(*gin.Context)
	if c == nil || c.Request == nil || c.Request.URL == nil || c.Request.Method != http.MethodPost ||
		(c.Request.URL.Path != "/v1/messages" && c.Request.URL.Path != "/v1/responses") || c.Request.Header.Get("Upgrade") != "" || opts.Alt != "" {
		return opts, executionErrorMessage(cachepolicy.Unsupported())
	}
	r, err := cachepolicy.ParseHeaders(opts.Headers, requestCallerScope(c))
	if err != nil {
		return opts, executionErrorMessage(err)
	}
	r.RequestID, r.Model, r.SourceFormat, r.Stream = requestID, model, opts.SourceFormat.String(), opts.Stream
	if r.SourceFormat != "claude" && r.SourceFormat != "openai-response" {
		return opts, executionErrorMessage(cachepolicy.Unsupported())
	}
	host, ok := h.interceptorHost().(cachePolicyHost)
	if !ok {
		return opts, executionErrorMessage(cachepolicy.Unavailable())
	}
	policy, err := host.ResolveCachePolicy(ctx, r)
	if err != nil {
		return opts, executionErrorMessage(err)
	}
	if !cachepolicy.Matches(policy, r) {
		return opts, executionErrorMessage(cachepolicy.Unavailable())
	}
	opts.CachePolicy = cachepolicy.Clone(&policy)
	opts.CachePolicyAttempt = func(ctx context.Context, attempt pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error) {
		attempt.RequestID, attempt.Capability = requestID, r.Capability
		return host.CachePolicyAttempt(ctx, attempt)
	}
	return opts, nil
}

func policyExecutionProvider(providers []string) string {
	if len(providers) == 1 && providers[0] == "claude" {
		return "claude"
	}
	return ""
}
