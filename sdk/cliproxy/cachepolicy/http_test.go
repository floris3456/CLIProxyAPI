package cachepolicy

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPacerCommitCommandContract(t *testing.T) {
	receipt := `{"version":1,"transition_id":"transition","previous_checkpoint":"old-checkpoint","checkpoint":"new-checkpoint","committed":true}`
	body := `{"protocol":1,"operation":"commit","installation_id":"install","session_id":"chat","provider_route":"claude-main","context_epoch":"new-checkpoint","expected_revision":2,"ttl":"1h","operation_id":"commit-op","transition_id":"transition","checkpoint":"new-checkpoint","compaction_started_at":"2026-10-08T00:00:01Z","checkpoint_committed_at":"2026-10-08T00:00:02Z","checkpoint_receipt":` + receipt + `}`
	r := httptest.NewRequest("POST", Path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(CapabilityHeader, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	got, err := DecodeHTTP(r, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation.ContextEpoch != "new-checkpoint" || got.Operation.Checkpoint != got.Operation.ContextEpoch || got.Operation.ExpectedRevision != 2 || got.Operation.CompactionStartedAt == nil || !got.Operation.CompactionStartedAt.Equal(time.Date(2026, 10, 8, 0, 0, 1, 0, time.UTC)) {
		t.Fatalf("commit changed: %+v", got.Operation)
	}
	if got.Operation.CheckpointReceipt == nil || got.Operation.CheckpointReceipt.PreviousCheckpoint != "old-checkpoint" {
		t.Fatal("checkpoint receipt lost")
	}
	for _, bad := range []string{
		strings.Replace(body, `"expected_revision":2,`, "", 1),
		strings.Replace(body, `"checkpoint":"new-checkpoint"`, `"new_context_epoch":"new-checkpoint"`, 1),
		strings.TrimSuffix(body, "}") + `,"empty_context":true}`,
		strings.Replace(body, `,"checkpoint_receipt":`+receipt, "", 1),
		strings.Replace(body, `"committed":true`, `"committed":false`, 1),
		strings.Replace(body, `"committed":true`, `"committed":true,"committed":true`, 1),
		strings.Replace(body, `"committed":true`, `"Committed":true`, 1),
		strings.Replace(body, `"committed":true`, `"committed":null`, 1),
	} {
		r := httptest.NewRequest("POST", Path, strings.NewReader(bad))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(CapabilityHeader, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		if _, err := DecodeHTTP(r, strings.Repeat("a", 64)); err == nil {
			t.Fatal("obsolete or missing field accepted")
		}
	}
	withoutClocks := strings.Replace(body, `,"compaction_started_at":"2026-10-08T00:00:01Z","checkpoint_committed_at":"2026-10-08T00:00:02Z"`, "", 1)
	r = httptest.NewRequest("POST", Path, strings.NewReader(withoutClocks))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(CapabilityHeader, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if _, err := DecodeHTTP(r, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("valid receipt required client wall clocks: %v", err)
	}
}

func TestPacerExistingHistoryEnrollmentContract(t *testing.T) {
	body := `{"protocol":1,"operation":"prepare","installation_id":"install","session_id":"chat","provider_route":"claude-main","context_epoch":"old-checkpoint","expected_revision":0,"ttl":"1h","operation_id":"enroll-op","enrollment":"existing_history"}`
	r := httptest.NewRequest("POST", Path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(CapabilityHeader, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	got, err := DecodeHTTP(r, strings.Repeat("a", 64))
	if err != nil || got.Operation.Enrollment != "existing_history" {
		t.Fatalf("existing-history enrollment: %v", err)
	}
}
