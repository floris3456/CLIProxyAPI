package cachepolicy

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const MaxBody = 16384

// DecodeHTTP checks the transport only. Stateful authorization belongs to Pacer.
func DecodeHTTP(r *http.Request, caller string) (pluginapi.CachePolicyRequest, error) {
	var out pluginapi.CachePolicyRequest
	if r == nil || r.URL == nil || r.URL.Path != Path || r.URL.RawPath != "" {
		return out, Invalid()
	}
	if caller == "" {
		return out, &Error{"pacer_cache_auth_required", http.StatusUnauthorized}
	}
	out.Method = r.Method
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return out, &Error{"pacer_cache_method_not_allowed", http.StatusMethodNotAllowed}
	}
	for k, v := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(HeaderPrefix)) {
			if !strings.EqualFold(k, CapabilityHeader) || len(v) != 1 || out.Capability != "" {
				return out, Invalid()
			}
			out.Capability = v[0]
		}
	}
	if out.Capability != "" && !ValidCapability(out.Capability) {
		return out, Invalid()
	}
	body := []byte(nil)
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			return out, Invalid()
		}
	}
	if len(body) > MaxBody {
		return out, &Error{"pacer_cache_body_too_large", http.StatusRequestEntityTooLarge}
	}
	if r.Method == http.MethodGet {
		if len(body) != 0 {
			return out, Invalid()
		}
		q, err := parsePolicyQuery(r)
		if err != nil {
			return out, err
		}
		protocol, _ := strconv.Atoi(q["protocol"])
		out.Operation = pluginapi.CachePolicyOperation{Protocol: protocol, Operation: "read", InstallationID: q["installation_id"], SessionID: q["session_id"], ProviderRoute: q["provider_route"]}
	} else {
		if r.URL.RawQuery != "" {
			return out, Invalid()
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			return out, &Error{"pacer_cache_json_required", http.StatusUnsupportedMediaType}
		}
		// A token pass rejects duplicate object keys as well as the strict struct decoder's unknown fields.
		dec := json.NewDecoder(strings.NewReader(string(body)))
		token, err := dec.Token()
		if err != nil || token != json.Delim('{') {
			return out, Invalid()
		}
		seen := make(map[string]bool)
		for dec.More() {
			key, errKey := dec.Token()
			if errKey != nil {
				return out, Invalid()
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return out, Invalid()
			}
			switch name {
			case "protocol", "operation", "installation_id", "session_id", "provider_route", "context_epoch", "expected_revision", "ttl", "operation_id", "transition_id", "enrollment", "checkpoint", "checkpoint_receipt", "compaction_started_at", "checkpoint_committed_at":
			default:
				return out, Invalid()
			}
			seen[name] = true
			var raw json.RawMessage
			if dec.Decode(&raw) != nil || string(raw) == "null" {
				return out, Invalid()
			}
			if name == "checkpoint_receipt" && !validCheckpointReceiptJSON(raw) {
				return out, Invalid()
			}
		}
		for _, field := range []string{"protocol", "operation", "installation_id", "session_id", "provider_route", "context_epoch", "expected_revision", "ttl", "operation_id"} {
			if !seen[field] {
				return out, Invalid()
			}
		}
		dec = json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if dec.Decode(&out.Operation) != nil {
			return out, Invalid()
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return out, Invalid()
		}
		switch out.Operation.Operation {
		case "ensure", "prepare", "commit", "cancel":
		default:
			return out, Invalid()
		}
		if !ValidID(out.Operation.OperationID) || !ValidID(out.Operation.ContextEpoch) || out.Operation.ExpectedRevision < 0 {
			return out, Invalid()
		}
		if out.Operation.TTL != "5m" && out.Operation.TTL != "1h" {
			return out, Invalid()
		}
		if out.Operation.TransitionID != "" && !ValidID(out.Operation.TransitionID) {
			return out, Invalid()
		}
		if out.Operation.Enrollment != "" && !((out.Operation.Operation == "ensure" && out.Operation.Enrollment == "new_empty") || (out.Operation.Operation == "prepare" && out.Operation.Enrollment == "existing_history")) {
			return out, Invalid()
		}
		if out.Operation.Operation == "commit" && (!ValidID(out.Operation.TransitionID) || out.Operation.Checkpoint != out.Operation.ContextEpoch || out.Operation.CheckpointReceipt == nil) {
			return out, Invalid()
		}
		if out.Operation.Operation == "cancel" && !ValidID(out.Operation.TransitionID) {
			return out, Invalid()
		}
		if out.Operation.Operation != "commit" && (out.Operation.Checkpoint != "" || out.Operation.CheckpointReceipt != nil || out.Operation.CompactionStartedAt != nil || out.Operation.CheckpointCommittedAt != nil) {
			return out, Invalid()
		}
		if out.Operation.Operation != "commit" && out.Operation.Operation != "cancel" && out.Operation.TransitionID != "" {
			return out, Invalid()
		}
	}
	out.Scope = pluginapi.CachePolicyScope{CallerScope: caller, InstallationID: out.Operation.InstallationID, SessionID: out.Operation.SessionID, ProviderRoute: out.Operation.ProviderRoute}
	if out.Operation.Protocol != 1 || !ValidScope(out.Scope) {
		return out, Invalid()
	}
	if out.Capability == "" {
		return out, &Error{"pacer_cache_capability_required", http.StatusForbidden}
	}
	return out, nil
}

func validCheckpointReceiptJSON(raw json.RawMessage) bool {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for dec.More() {
		key, errKey := dec.Token()
		if errKey != nil {
			return false
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return false
		}
		switch name {
		case "version", "transition_id", "previous_checkpoint", "checkpoint", "committed":
		default:
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil || string(value) == "null" {
			return false
		}
	}
	if len(seen) != 5 {
		return false
	}
	var receipt pluginapi.CachePolicyCheckpointReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		return false
	}
	return receipt.Version == 1 && receipt.Committed && ValidID(receipt.TransitionID) && ValidID(receipt.PreviousCheckpoint) && ValidID(receipt.Checkpoint)
}

func parsePolicyQuery(r *http.Request) (map[string]string, error) {
	if len(r.URL.RawQuery) > 2048 {
		return nil, Invalid()
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 4 || q.Get("protocol") != "1" {
		return nil, Invalid()
	}
	out := make(map[string]string)
	for _, k := range []string{"protocol", "installation_id", "session_id", "provider_route"} {
		v := q[k]
		if len(v) != 1 {
			return nil, Invalid()
		}
		out[k] = v[0]
	}
	return out, nil
}
