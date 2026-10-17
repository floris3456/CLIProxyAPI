package pluginapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAuthInventoryCheckpointWireShape(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	resp := HostAuthInventoryCheckpointResponse{
		Version:              1,
		Provider:             "claude",
		Coverage:             "known",
		ManagerIncarnationID: "inc",
		Sequence:             7,
		At:                   at,
		Tuples: []HostAuthInventoryTuple{
			{AuthID: "a", AuthIndex: "i1", AccountRef: "ref", BindingSequence: 3, State: "active"},
			{AuthID: "b", AuthIndex: "i2", BindingSequence: 7, State: "removed"},
		},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"provider":"claude","coverage":"known","manager_incarnation_id":"inc","sequence":7,"at":"2026-10-08T12:00:00Z","tuples":[` +
		`{"auth_id":"a","auth_index":"i1","account_ref":"ref","binding_sequence":3,"state":"active"},` +
		`{"auth_id":"b","auth_index":"i2","binding_sequence":7,"state":"removed"}]}`
	if string(raw) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", raw, want)
	}
	raw, err = json.Marshal(HostAuthInventoryCheckpointRequest{Provider: "claude"})
	if err != nil || string(raw) != `{"provider":"claude"}` {
		t.Fatalf("request wire = %s, %v", raw, err)
	}
	unknown := HostAuthInventoryCheckpointResponse{Version: 1, Provider: "claude", Coverage: "unknown", Reason: "overflow", Tuples: []HostAuthInventoryTuple{}}
	raw, err = json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err = json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if tuples, ok := generic["tuples"].([]any); !ok || len(tuples) != 0 || generic["reason"] != "overflow" {
		t.Fatalf("unknown wire = %s", raw)
	}
}
