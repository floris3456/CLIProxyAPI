package pluginabi

import "testing"

func TestAuthInventoryCheckpointMethodName(t *testing.T) {
	if MethodHostAuthInventoryCheckpoint != "host.auth.inventory_checkpoint" {
		t.Fatalf("MethodHostAuthInventoryCheckpoint = %q", MethodHostAuthInventoryCheckpoint)
	}
	if SchemaVersion != 7 || ABIVersion != 1 {
		t.Fatalf("schema/ABI = %d/%d, want 7/1", SchemaVersion, ABIVersion)
	}
}
