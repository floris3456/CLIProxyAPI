package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// callHostAuthInventoryCheckpoint has the same caller authorization as
// host.auth.list. It reads the auth manager's bounded in-memory ledger only.
func (h *Host) callHostAuthInventoryCheckpoint(ctx context.Context, raw []byte) ([]byte, error) {
	_ = ctx
	if len(raw) > 4096 {
		return nil, fmt.Errorf("invalid auth inventory checkpoint request")
	}
	var req pluginapi.HostAuthInventoryCheckpointRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid auth inventory checkpoint request")
	}
	return marshalRPCResult(h.currentAuthManager().AuthInventoryCheckpoint(req.Provider))
}
