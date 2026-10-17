package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func decodeQuotaCooldownCallback(raw []byte, target any) error {
	if len(raw) > 16384 {
		return fmt.Errorf("quota cooldown request too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid quota cooldown request")
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid quota cooldown request")
	}
	return nil
}

func (h *Host) callHostQuotaCooldownRead(ctx context.Context, raw []byte) ([]byte, error) {
	var req pluginapi.HostQuotaCooldownReadRequest
	if err := decodeQuotaCooldownCallback(raw, &req); err != nil {
		return nil, err
	}
	a, err := h.authByIndex(req.AuthIndex)
	if err != nil {
		return nil, err
	}
	m := h.currentAuthManager()
	if m == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	resp := m.ReadQuotaCooldowns(a.ID)
	if resp.AuthIndex != "" && resp.AuthIndex != req.AuthIndex {
		return nil, fmt.Errorf("credential changed during quota cooldown read")
	}
	return marshalRPCResult(resp)
}

func (h *Host) callHostQuotaCooldownClear(ctx context.Context, raw []byte) ([]byte, error) {
	var req pluginapi.HostQuotaCooldownClearRequest
	if err := decodeQuotaCooldownCallback(raw, &req); err != nil {
		return nil, err
	}
	a, err := h.authByIndex(req.AuthIndex)
	if err != nil {
		return nil, err
	}
	m := h.currentAuthManager()
	if m == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	return marshalRPCResult(m.ClearQuotaCooldown(ctx, a.ID, req))
}
