package helps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Session date pins are kept on disk so a restart does not re-date open chats.
// The pinned date sits in every cloaked request's first message: a new date after
// a restart past midnight rewrites each open chat's whole prompt cache, and
// Anthropic drops all of its earlier thinking blocks as bound to another prefix.

// claudeDatePinFlushInterval bounds how stale a saved expiry may be. New and
// re-dated pins are saved at once; a sliding expiry only needs to survive a
// restart approximately.
const claudeDatePinFlushInterval = 30 * time.Minute

const claudeDatePinFileVersion = 1

var (
	claudeDatePinKeyPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	claudeDatePinDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

type claudeDatePinFile struct {
	Version int                            `json:"version"`
	Pins    map[string]claudeDatePinRecord `json:"pins"`
}

type claudeDatePinRecord struct {
	Date      string    `json:"date"`
	ExpiresAt time.Time `json:"expires_at"`
}

// claudeDatePinPersistence is guarded by claudeDiagnosticsState's mutex.
var claudeDatePinPersistence struct {
	path      string
	dirty     bool
	flushing  bool
	flushedAt time.Time
}

// EnableClaudeSessionDatePinPersistence restores the session date pins saved at
// path and keeps that file current from now on. Pins already in memory win over
// saved ones; expired and malformed saved entries are skipped. A missing file is
// an empty store. Keys are session digests, never session IDs.
func EnableClaudeSessionDatePinPersistence(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("claude date pin store: empty path")
	}
	saved, errRead := readClaudeDatePinFile(path)
	now := time.Now()
	claudeDiagnosticsState.Lock()
	defer claudeDiagnosticsState.Unlock()
	claudeDatePinPersistence.path = path
	claudeDatePinPersistence.flushedAt = now
	for key, record := range saved {
		if _, exists := claudeDiagnosticsState.datePins[key]; exists {
			continue
		}
		if !claudeDatePinKeyPattern.MatchString(key) || !claudeDatePinDatePattern.MatchString(record.Date) || !now.Before(record.ExpiresAt) {
			continue
		}
		if len(claudeDiagnosticsState.datePins) >= claudeDiagnosticsMaxEntries {
			break
		}
		claudeDiagnosticsState.datePins[key] = claudeSessionDatePin{date: record.Date, expiresAt: record.ExpiresAt}
	}
	return errRead
}

// scheduleClaudeDatePinFlushLocked saves the pins in the background: at once
// when a pin was created or re-dated, otherwise at most every flush interval.
// The caller holds claudeDiagnosticsState's mutex.
func scheduleClaudeDatePinFlushLocked(now time.Time, changed bool) {
	if claudeDatePinPersistence.path == "" {
		return
	}
	claudeDatePinPersistence.dirty = true
	if !changed && now.Sub(claudeDatePinPersistence.flushedAt) < claudeDatePinFlushInterval {
		return
	}
	if claudeDatePinPersistence.flushing {
		return
	}
	claudeDatePinPersistence.flushing = true
	startClaudeDatePinFlush()
}

// startClaudeDatePinFlush is replaced in tests to run the flush deterministically.
var startClaudeDatePinFlush = func() { go flushClaudeDatePins() }

// flushClaudeDatePins writes snapshots until no change is pending. A failed
// write is retried with the next pin change rather than in a loop.
func flushClaudeDatePins() {
	for {
		claudeDiagnosticsState.Lock()
		if !claudeDatePinPersistence.dirty {
			claudeDatePinPersistence.flushing = false
			claudeDiagnosticsState.Unlock()
			return
		}
		now := time.Now()
		path := claudeDatePinPersistence.path
		snapshot := make(map[string]claudeDatePinRecord, len(claudeDiagnosticsState.datePins))
		for key, pin := range claudeDiagnosticsState.datePins {
			if now.Before(pin.expiresAt) {
				snapshot[key] = claudeDatePinRecord{Date: pin.date, ExpiresAt: pin.expiresAt}
			}
		}
		claudeDatePinPersistence.dirty = false
		claudeDatePinPersistence.flushedAt = now
		claudeDiagnosticsState.Unlock()
		if errWrite := writeClaudeDatePinFile(path, snapshot); errWrite != nil {
			log.Warnf("claude date pin store: %v", errWrite)
			claudeDiagnosticsState.Lock()
			claudeDatePinPersistence.dirty = true
			claudeDatePinPersistence.flushing = false
			claudeDiagnosticsState.Unlock()
			return
		}
	}
}

func readClaudeDatePinFile(path string) (map[string]claudeDatePinRecord, error) {
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("claude date pin store: read %s: %w", path, errRead)
	}
	var file claudeDatePinFile
	if errDecode := json.Unmarshal(data, &file); errDecode != nil {
		return nil, fmt.Errorf("claude date pin store: decode %s: %w", path, errDecode)
	}
	if file.Version != claudeDatePinFileVersion {
		return nil, fmt.Errorf("claude date pin store: %s has version %d, want %d", path, file.Version, claudeDatePinFileVersion)
	}
	return file.Pins, nil
}

// writeClaudeDatePinFile replaces the file atomically, so a crash mid-write
// leaves the previous pins.
func writeClaudeDatePinFile(path string, pins map[string]claudeDatePinRecord) error {
	data, errEncode := json.Marshal(claudeDatePinFile{Version: claudeDatePinFileVersion, Pins: pins})
	if errEncode != nil {
		return fmt.Errorf("encode: %w", errEncode)
	}
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create %s: %w", dir, errMkdir)
	}
	tmp, errCreate := os.CreateTemp(dir, ".claude-date-pins-*")
	if errCreate != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, errCreate)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, errWrite)
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, errSync)
	}
	if errClose := tmp.Close(); errClose != nil {
		return fmt.Errorf("close %s: %w", tmpName, errClose)
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		return fmt.Errorf("replace %s: %w", path, errRename)
	}
	return nil
}
