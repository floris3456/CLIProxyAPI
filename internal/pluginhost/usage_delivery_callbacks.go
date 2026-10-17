package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// An old queued adapter must not silently deliver to a replacement plugin and
// certify its acknowledgement as belonging to the previous registration.
func sameUsagePlugin(a, b pluginapi.UsagePlugin) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	t := reflect.TypeOf(a)
	return t == reflect.TypeOf(b) && reflect.ValueOf(a).Comparable() && reflect.ValueOf(b).Comparable() && a == b
}

func (h *Host) callHostUsageDelivery(ctx context.Context, raw []byte) ([]byte, error) {
	pluginID := hostCallbackPluginIDFromContext(ctx)
	if pluginID == "" {
		return nil, fmt.Errorf("usage delivery requires native callback identity")
	}
	if len(raw) > 1024 {
		return nil, fmt.Errorf("invalid usage delivery request")
	}
	var req pluginapi.HostUsageDeliveryRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid usage delivery request")
	}
	if req.Provider != "claude" && req.Provider != "codex" {
		return nil, fmt.Errorf("unsupported usage delivery provider")
	}
	resp := coreusage.DefaultManager().DeliverySnapshot("plugin:"+pluginID, req.Provider)
	if h.isPluginFused(pluginID) {
		resp.Reason = "sink_fused"
	} else if h.currentUsagePlugin(pluginID) == nil {
		resp.Reason = "sink_missing"
	}
	return marshalRPCResult(resp)
}
