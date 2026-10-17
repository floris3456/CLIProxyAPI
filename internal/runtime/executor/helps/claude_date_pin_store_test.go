package helps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useClaudeDatePinStoreForTest enables persistence at a fresh path and records
// background flush starts instead of running them.
func useClaudeDatePinStoreForTest(t *testing.T) (path string, starts *int) {
	t.Helper()
	resetClaudeDiagnosticsForTest()
	previous := startClaudeDatePinFlush
	starts = new(int)
	startClaudeDatePinFlush = func() { *starts++ }
	t.Cleanup(func() {
		startClaudeDatePinFlush = previous
		resetClaudeDiagnosticsForTest()
	})
	path = filepath.Join(t.TempDir(), "auths", claudeDatePinTestFile)
	if errEnable := EnableClaudeSessionDatePinPersistence(path); errEnable != nil {
		t.Fatalf("EnableClaudeSessionDatePinPersistence() error = %v", errEnable)
	}
	return path, starts
}

const claudeDatePinTestFile = "claude-session-date-pins.state"

func readClaudeDatePinsForTest(t *testing.T, path string) map[string]claudeDatePinRecord {
	t.Helper()
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read pin file: %v", errRead)
	}
	var file claudeDatePinFile
	if errDecode := json.Unmarshal(data, &file); errDecode != nil {
		t.Fatalf("decode pin file: %v", errDecode)
	}
	if file.Version != claudeDatePinFileVersion {
		t.Fatalf("pin file version = %d, want %d", file.Version, claudeDatePinFileVersion)
	}
	return file.Pins
}

func TestClaudeSessionDatePinSurvivesRestart(t *testing.T) {
	path, starts := useClaudeDatePinStoreForTest(t)
	retention := 7 * 24 * time.Hour
	key, _, _ := BeginClaudeDiagnostics("credential", "chat-1")
	if got := PinClaudeSessionDate(key, "chat-1", "2026-10-16", retention); got != "2026-10-16" {
		t.Fatalf("first pin = %q, want 2026-10-16", got)
	}
	if *starts != 1 {
		t.Fatalf("flush starts after a new pin = %d, want 1", *starts)
	}
	flushClaudeDatePins()
	pins := readClaudeDatePinsForTest(t, path)
	saved, ok := pins[ClaudeSessionDatePinKey("chat-1")]
	if len(pins) != 1 || !ok || saved.Date != "2026-10-16" {
		t.Fatalf("saved pins = %#v, want chat-1 on 2026-10-16", pins)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "chat-1") {
		t.Fatal("pin file contains the raw session ID")
	}
	if info, errStat := os.Stat(path); errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pin file mode = %v (%v), want 0600", info.Mode().Perm(), errStat)
	}

	// A restart after midnight: memory is empty, the store is enabled again.
	resetClaudeDiagnosticsForTest()
	if errEnable := EnableClaudeSessionDatePinPersistence(path); errEnable != nil {
		t.Fatalf("EnableClaudeSessionDatePinPersistence() after restart error = %v", errEnable)
	}
	if got := LookupClaudeSessionDatePin("chat-1", retention); got != "2026-10-16" {
		t.Fatalf("restored pin = %q, want 2026-10-16", got)
	}
	key, _, _ = BeginClaudeDiagnostics("credential", "chat-1")
	if got := PinClaudeSessionDate(key, "chat-1", "2026-10-17", retention); got != "2026-10-16" {
		t.Fatalf("pin after restart = %q, want the saved 2026-10-16", got)
	}
}

func TestClaudeSessionDatePinStoreSavesExpiryOnlyPerInterval(t *testing.T) {
	_, starts := useClaudeDatePinStoreForTest(t)
	retention := 7 * 24 * time.Hour
	key, _, _ := BeginClaudeDiagnostics("credential", "chat-2")
	PinClaudeSessionDate(key, "chat-2", "2026-10-16", retention)
	flushClaudeDatePins()
	*starts = 0

	// The same date again only slides the expiry: no write within the interval.
	PinClaudeSessionDate(key, "chat-2", "2026-10-17", retention)
	if *starts != 0 {
		t.Fatalf("flush starts for an unchanged pin = %d, want 0", *starts)
	}
	claudeDiagnosticsState.Lock()
	claudeDatePinPersistence.flushedAt = time.Now().Add(-claudeDatePinFlushInterval - time.Second)
	claudeDiagnosticsState.Unlock()
	PinClaudeSessionDate(key, "chat-2", "2026-10-17", retention)
	if *starts != 1 {
		t.Fatalf("flush starts after the interval = %d, want 1", *starts)
	}
}

func TestClaudeSessionDatePinStoreSkipsExpiredAndMalformedEntries(t *testing.T) {
	resetClaudeDiagnosticsForTest()
	t.Cleanup(resetClaudeDiagnosticsForTest)
	path := filepath.Join(t.TempDir(), claudeDatePinTestFile)
	future := time.Now().Add(time.Hour)
	if errWrite := writeClaudeDatePinFile(path, map[string]claudeDatePinRecord{
		ClaudeSessionDatePinKey("live"):    {Date: "2026-10-16", ExpiresAt: future},
		ClaudeSessionDatePinKey("expired"): {Date: "2026-10-01", ExpiresAt: time.Now().Add(-time.Hour)},
		ClaudeSessionDatePinKey("bad"):     {Date: "16 Oct", ExpiresAt: future},
		"not-a-digest":                     {Date: "2026-10-16", ExpiresAt: future},
	}); errWrite != nil {
		t.Fatalf("writeClaudeDatePinFile() error = %v", errWrite)
	}
	if errEnable := EnableClaudeSessionDatePinPersistence(path); errEnable != nil {
		t.Fatalf("EnableClaudeSessionDatePinPersistence() error = %v", errEnable)
	}
	claudeDiagnosticsState.Lock()
	restored := len(claudeDiagnosticsState.datePins)
	claudeDiagnosticsState.Unlock()
	if restored != 1 || LookupClaudeSessionDatePin("live", time.Hour) != "2026-10-16" {
		t.Fatalf("restored %d pins, want only the live one", restored)
	}

	// An unreadable file is reported and restores nothing, but saving still works.
	resetClaudeDiagnosticsForTest()
	if errWrite := os.WriteFile(path, []byte(`{"version":2,"pins":{}}`), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errEnable := EnableClaudeSessionDatePinPersistence(path); errEnable == nil {
		t.Fatal("EnableClaudeSessionDatePinPersistence() accepted an unknown version")
	}
	claudeDiagnosticsState.Lock()
	enabled := claudeDatePinPersistence.path == path
	claudeDiagnosticsState.Unlock()
	if !enabled {
		t.Fatal("persistence not enabled after an unreadable file")
	}
}
