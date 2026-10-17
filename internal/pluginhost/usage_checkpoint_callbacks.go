package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func (h *Host) callHostUsageCheckpoint(ctx context.Context, raw []byte) ([]byte, error) {
	id := hostCallbackPluginIDFromContext(ctx)
	if id == "" || len(raw) > 4096 {
		return nil, fmt.Errorf("invalid usage checkpoint caller/request")
	}
	var req pluginapi.HostUsageCheckpointRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid usage checkpoint request")
	}
	a, err := h.authByIndex(req.Scope.AuthIndex)
	if err != nil {
		return nil, err
	}
	if a.ID != req.Scope.AuthID || a.Provider != req.Scope.Provider {
		return nil, fmt.Errorf("usage checkpoint credential scope mismatch")
	}
	managerUsage := coreusage.DefaultManager()
	registered := managerUsage.NamedPlugin("plugin:" + id)
	resp := managerUsage.SourceCheckpoint("plugin:"+id, req.Scope, registered)
	current := h.currentUsagePlugin(id)
	adapter, ok := registered.(*usageAdapter)
	identityCurrent := ok && adapter.host == h && sameUsagePlugin(adapter.plugin, current)
	if instance := hostCallbackInstanceFromContext(ctx); instance != nil {
		rpc, ok := current.(*rpcPluginAdapter)
		identityCurrent = identityCurrent && ok && pluginCallbackInstance(rpc.client) == instance
	}
	if !identityCurrent {
		resp.Coverage = "unknown"
		resp.Reason = "sink_instance_mismatch"
		resp.Through = nil
		resp.IncarnationID = ""
	}
	if manager := h.currentAuthManager(); manager == nil || manager.HomeEnabled() {
		resp.Coverage = "unsupported"
		resp.Reason = "unsupported_runtime"
		resp.Through = nil
	}
	if h.isPluginFused(id) || h.currentUsagePlugin(id) == nil {
		resp.Coverage = "unknown"
		resp.Reason = "sink_unavailable"
		resp.Through = nil
	}
	if resp.Physical != nil && (resp.Reason == "sink_instance_mismatch" || resp.Reason == "unsupported_runtime" || resp.Reason == "sink_unavailable") {
		resp.Physical.Coverage = "unknown"
		resp.Physical.Reason = resp.Reason
		resp.Physical.FromCut = nil
		resp.Physical.ThroughCut = nil
	}
	return marshalRPCResult(resp)
}
