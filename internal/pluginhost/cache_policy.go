package pluginhost

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/cachepolicy"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (h *Host) cachePolicyPlugin() (capabilityRecord, pluginapi.CachePolicyProvider) {
	if h != nil {
		for _, record := range h.activeRecords() {
			if record.id == "pacer" && record.plugin.SchemaVersion >= pluginabi.SchemaVersionCachePolicy && !h.isPluginFused(record.id) && h.recordCurrent(record) {
				return record, record.plugin.Capabilities.CachePolicy
			}
		}
	}
	return capabilityRecord{}, nil
}

func (h *Host) HandleCachePolicy(ctx context.Context, req pluginapi.CachePolicyRequest) (resp pluginapi.CachePolicyResponse, err error) {
	record, plugin := h.cachePolicyPlugin()
	if plugin == nil {
		return resp, cachepolicy.Unavailable()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "CachePolicy.Handle", "policy hook panic")
			resp = pluginapi.CachePolicyResponse{}
			err = cachepolicy.Unavailable()
		}
	}()
	resp, err = plugin.HandleCachePolicy(ctx, req)
	if err != nil {
		return pluginapi.CachePolicyResponse{}, policyPluginError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 599 || (resp.StatusCode >= 300 && resp.StatusCode < 400) || len(resp.Body) > cachepolicy.MaxBody || !json.Valid(resp.Body) {
		return pluginapi.CachePolicyResponse{}, cachepolicy.Unavailable()
	}
	resp.Body = append(json.RawMessage(nil), resp.Body...)
	return resp, nil
}

func (h *Host) ResolveCachePolicy(ctx context.Context, req pluginapi.CachePolicyResolveRequest) (resp pluginapi.CachePolicy, err error) {
	record, plugin := h.cachePolicyPlugin()
	if plugin == nil || record.plugin.Capabilities.RequestLifecyclePlugin == nil {
		return resp, cachepolicy.Unavailable()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "CachePolicy.Resolve", "policy hook panic")
			resp = pluginapi.CachePolicy{}
			err = cachepolicy.Unavailable()
		}
	}()
	resp, err = plugin.ResolveCachePolicy(ctx, req)
	if err != nil {
		return pluginapi.CachePolicy{}, policyPluginError(err)
	}
	if !cachepolicy.Matches(resp, req) {
		return pluginapi.CachePolicy{}, cachepolicy.Unavailable()
	}
	h.trackCachePolicyRequest(req.RequestID)
	return resp, nil
}

func policyPluginError(err error) error {
	if status, ok := err.(interface{ StatusCode() int }); ok && status.StatusCode() >= 400 && status.StatusCode() < 500 {
		return &cachepolicy.Error{Code: "pacer_cache_policy_rejected", Status: status.StatusCode()}
	}
	return cachepolicy.Unavailable()
}

func (a *rpcPluginAdapter) HandleCachePolicy(ctx context.Context, req pluginapi.CachePolicyRequest) (pluginapi.CachePolicyResponse, error) {
	return callPlugin[pluginapi.CachePolicyResponse](ctx, a.client, pluginabi.MethodCachePolicyHandle, req)
}

func (a *rpcPluginAdapter) ResolveCachePolicy(ctx context.Context, req pluginapi.CachePolicyResolveRequest) (pluginapi.CachePolicy, error) {
	return callPlugin[pluginapi.CachePolicy](ctx, a.client, pluginabi.MethodCachePolicyResolve, req)
}

func (h *Host) CachePolicyAttempt(ctx context.Context, req pluginapi.CachePolicyAttemptRequest) (resp pluginapi.CachePolicyAttemptResponse, err error) {
	if req.Operation == "begin" && !h.trackCachePolicyAttempt(req.RequestID, req.AttemptID) {
		return resp, cachepolicy.Unavailable()
	}
	defer func() {
		if req.Operation == "finish" || (req.Operation == "begin" && err != nil) {
			h.endCachePolicyAttempt(req.RequestID, req.AttemptID)
		}
	}()
	record, plugin := h.cachePolicyPlugin()
	if plugin == nil {
		return resp, cachepolicy.Unavailable()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "CachePolicy.Attempt", "policy hook panic")
			resp = pluginapi.CachePolicyAttemptResponse{}
			err = cachepolicy.Unavailable()
		}
	}()
	resp, err = plugin.CachePolicyAttempt(ctx, req)
	if err != nil {
		return pluginapi.CachePolicyAttemptResponse{}, policyPluginError(err)
	}
	if req.Operation == "begin" && (resp.Policy != req.Policy || resp.StartedAt.IsZero()) {
		return pluginapi.CachePolicyAttemptResponse{}, cachepolicy.Unavailable()
	}
	return resp, nil
}

// Request completion can race transport shutdown when a client cancels. Delay its
// notification until every physical attempt has stopped; this is process-local
// ordering only. The plugin still owns durable guards and restart reconciliation.
type cachePolicyRequestFlight struct {
	active     map[string]struct{}
	completion *pluginapi.RequestCompletion
	ctx        context.Context
	skip       string
}

func (h *Host) trackCachePolicyRequest(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cachePolicyRequests == nil {
		h.cachePolicyRequests = make(map[string]*cachePolicyRequestFlight)
	}
	if h.cachePolicyRequests[id] == nil {
		h.cachePolicyRequests[id] = &cachePolicyRequestFlight{active: make(map[string]struct{})}
	}
}

func (h *Host) trackCachePolicyAttempt(requestID, attemptID string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if flight := h.cachePolicyRequests[requestID]; flight != nil && flight.completion == nil {
		if _, exists := flight.active[attemptID]; exists {
			return false
		}
		flight.active[attemptID] = struct{}{}
		return true
	}
	return false
}

func (h *Host) endCachePolicyAttempt(requestID, attemptID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	flight := h.cachePolicyRequests[requestID]
	if flight == nil {
		h.mu.Unlock()
		return
	}
	delete(flight.active, attemptID)
	ready := len(flight.active) == 0 && flight.completion != nil
	if ready {
		delete(h.cachePolicyRequests, requestID)
	}
	h.mu.Unlock()
	if ready {
		h.completeRequestExceptNow(flight.ctx, *flight.completion, flight.skip)
	}
}

func (h *Host) queueCachePolicyCompletion(ctx context.Context, completion pluginapi.RequestCompletion, skip string) bool {
	h.mu.Lock()
	flight := h.cachePolicyRequests[completion.RequestID]
	if flight == nil {
		h.mu.Unlock()
		return false
	}
	if flight.completion != nil {
		h.mu.Unlock()
		return true
	}
	completion.Metadata = cloneInterceptorMetadata(completion.Metadata)
	flight.completion, flight.ctx, flight.skip = &completion, ctx, skip
	if skip == "pacer" {
		flight.skip = ""
	}
	ready := len(flight.active) == 0
	if ready {
		delete(h.cachePolicyRequests, completion.RequestID)
	}
	h.mu.Unlock()
	if ready {
		h.completeRequestExceptNow(ctx, completion, flight.skip)
	}
	return true
}

func (a *rpcPluginAdapter) CachePolicyAttempt(ctx context.Context, req pluginapi.CachePolicyAttemptRequest) (pluginapi.CachePolicyAttemptResponse, error) {
	return callPlugin[pluginapi.CachePolicyAttemptResponse](ctx, a.client, pluginabi.MethodCachePolicyAttempt, req)
}
