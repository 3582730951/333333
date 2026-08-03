// Package incident connects the generic supervisor event framework to durable
// diagnostics. Its primary callback writes the diagnostic_events table; its
// fallback callback fsyncs a bounded, privacy-safe replay journal.
package incident

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	journalRecordLimit = 4096
	journalByteLimit   = int64(16 << 20)
	journalRecordBytes = int64(64 << 10)
	journalGapName     = "diagnostic-gap.json"
	replayInterval     = time.Second
)

var (
	ErrPrimaryUnavailable = errors.New("incident primary store is unavailable")
	eventIDPattern        = regexp.MustCompile(`^SEVT-[A-F0-9]{32}$`)
	requestIDPattern      = regexp.MustCompile(`^REQ-[A-F0-9]{16}$`)
	fingerprintPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{32}$`)
	incidentIDFallback    atomic.Uint64
)

// Record is the privacy-safe durable subset of supervisor.Event. Raw panic and
// error text never enter this structure, its journal, or diagnostic_events.
type Record struct {
	ID                string `json:"id"`
	CreatedAt         int64  `json:"created_at"`
	EventType         string `json:"event_type"`
	Severity          string `json:"severity"`
	Component         string `json:"component"`
	Operation         string `json:"operation,omitempty"`
	ErrorClass        string `json:"error_class,omitempty"`
	Fingerprint       string `json:"fingerprint"`
	RequestID         string `json:"request_id,omitempty"`
	Route             string `json:"route,omitempty"`
	Status            int    `json:"status,omitempty"`
	Recovered         bool   `json:"recovered,omitempty"`
	ResponseCommitted bool   `json:"response_committed,omitempty"`
	UptimeMillis      int64  `json:"uptime_millis,omitempty"`
	BackoffMillis     int64  `json:"backoff_millis,omitempty"`
	FallbackUsed      bool   `json:"fallback_used,omitempty"`
	DiagnosticGap     bool   `json:"diagnostic_gap,omitempty"`
	DroppedRecords    uint64 `json:"dropped_records,omitempty"`
	CorruptRecords    uint64 `json:"corrupt_records,omitempty"`
}

// Snapshot exposes bounded framework health without event payloads.
type Snapshot struct {
	Pending           uint64 `json:"pending"`
	JournalBytes      int64  `json:"journal_bytes"`
	FallbackWrites    uint64 `json:"fallback_writes"`
	Replayed          uint64 `json:"replayed"`
	ReplayFailures    uint64 `json:"replay_failures"`
	DroppedRecords    uint64 `json:"dropped_records"`
	CorruptRecords    uint64 `json:"corrupt_records"`
	GapPending        bool   `json:"gap_pending"`
	LastReplayUnix    int64  `json:"last_replay_unix,omitempty"`
	PrimaryConfigured bool   `json:"primary_configured"`
}

// Reporter implements the primary/fallback callbacks and replay worker.
type Reporter struct {
	dir     string
	mu      sync.Mutex
	storeMu sync.RWMutex
	store   *storage.Store
	wake    chan struct{}

	pending        atomic.Uint64
	journalBytes   atomic.Int64
	fallbackWrites atomic.Uint64
	replayed       atomic.Uint64
	replayFailures atomic.Uint64
	dropped        atomic.Uint64
	corrupt        atomic.Uint64
	gapPending     atomic.Bool
	lastReplayUnix atomic.Int64
	lastReplayLog  atomic.Int64
}

// Open validates the private journal directory and inventories any records left
// by an older process. A nil store is supported during early startup; events use
// the fallback until SetStore and Replay complete.
func Open(dir string, store *storage.Store) (*Reporter, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || dir == "" {
		return nil, errors.New("incident journal directory is empty")
	}
	if err := datadir.EnsureDirectory(dir); err != nil {
		return nil, fmt.Errorf("prepare incident journal: %w", err)
	}
	reporter := &Reporter{dir: dir, store: store, wake: make(chan struct{}, 1)}
	reporter.mu.Lock()
	err := reporter.refreshJournalStatsLocked()
	reporter.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return reporter, nil
}

// SetStore swaps the primary durable sink. It is safe during an active/standby
// role transition and immediately wakes the replay worker.
func (reporter *Reporter) SetStore(store *storage.Store) {
	if reporter == nil {
		return
	}
	reporter.storeMu.Lock()
	reporter.store = store
	reporter.storeMu.Unlock()
	reporter.wakeReplay()
}

// CallbackOptions returns the complete primary + fallback registration for the
// generic supervisor framework.
func (reporter *Reporter) CallbackOptions(name string) supervisor.CallbackOptions {
	return supervisor.CallbackOptions{
		Name: name, Timeout: 2 * time.Second, FallbackTimeout: 2 * time.Second,
		Callback: reporter.PrimaryCallback, Fallback: reporter.FallbackCallback,
	}
}

// PrimaryCallback writes one normalized event directly to diagnostic_events.
func (reporter *Reporter) PrimaryCallback(ctx context.Context, event supervisor.Event) error {
	if reporter == nil {
		return ErrPrimaryUnavailable
	}
	return reporter.persist(ctx, recordFromEvent(event, false))
}

// FallbackCallback atomically fsyncs one normalized event into the bounded
// replay journal. It is idempotent by supervisor event ID.
func (reporter *Reporter) FallbackCallback(ctx context.Context, event supervisor.Event) error {
	if reporter == nil {
		return errors.New("incident fallback reporter is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	record := recordFromEvent(event, true)
	reporter.mu.Lock()
	err := reporter.writeRecordLocked(record)
	if err == nil {
		err = reporter.enforceBoundsLocked(record.ID)
	}
	if statsErr := reporter.refreshJournalStatsLocked(); err == nil {
		err = statsErr
	}
	reporter.mu.Unlock()
	if err != nil {
		return err
	}
	reporter.fallbackWrites.Add(1)
	reporter.wakeReplay()
	return nil
}

// Run continuously retries journal replay and wakes immediately after a fallback
// write. It returns only when ctx is cancelled.
func (reporter *Reporter) Run(ctx context.Context) {
	if reporter == nil {
		return
	}
	ticker := time.NewTicker(replayInterval)
	defer ticker.Stop()
	for {
		_, err := reporter.Replay(ctx)
		if err != nil && !errors.Is(err, ErrPrimaryUnavailable) && !errors.Is(err, context.Canceled) {
			now := time.Now().Unix()
			last := reporter.lastReplayLog.Load()
			if now-last >= 60 && reporter.lastReplayLog.CompareAndSwap(last, now) {
				log.Printf("[INCIDENT-REPLAY] pending=%d error_class=%T", reporter.pending.Load(), err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-reporter.wake:
		case <-ticker.C:
		}
	}
}

// Replay persists every complete fallback record in creation order. Successful
// database insertion is followed by unlink + directory fsync, so a crash yields
// either a pending file or an idempotent duplicate, never silent loss.
func (reporter *Reporter) Replay(ctx context.Context) (int, error) {
	if reporter == nil {
		return 0, ErrPrimaryUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if reporter.currentStore() == nil {
		return 0, ErrPrimaryUnavailable
	}
	if err := reporter.replayGapLocked(ctx); err != nil {
		reporter.replayFailures.Add(1)
		return 0, err
	}
	files, err := reporter.journalFilesLocked()
	if err != nil {
		reporter.replayFailures.Add(1)
		return 0, err
	}
	replayed := 0
	for _, file := range files {
		if err = ctx.Err(); err != nil {
			break
		}
		record, readErr := reporter.readRecordLocked(file.path)
		if readErr != nil {
			reporter.corrupt.Add(1)
			if gapErr := reporter.markGapLocked(0, 1); gapErr != nil {
				err = errors.Join(readErr, gapErr)
				break
			}
			if removeErr := os.Remove(file.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(readErr, removeErr)
				break
			}
			continue
		}
		record.FallbackUsed = true
		if persistErr := reporter.persist(ctx, record); persistErr != nil {
			err = persistErr
			break
		}
		if removeErr := os.Remove(file.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = removeErr
			break
		}
		replayed++
	}
	if syncErr := syncDirectory(reporter.dir); err == nil {
		err = syncErr
	}
	if refreshErr := reporter.refreshJournalStatsLocked(); err == nil {
		err = refreshErr
	}
	if replayed > 0 {
		reporter.replayed.Add(uint64(replayed))
	}
	reporter.lastReplayUnix.Store(time.Now().Unix())
	if err != nil {
		reporter.replayFailures.Add(1)
	}
	return replayed, err
}

// Snapshot reports framework health without reading event payloads.
func (reporter *Reporter) Snapshot() Snapshot {
	if reporter == nil {
		return Snapshot{}
	}
	return Snapshot{
		Pending: reporter.pending.Load(), JournalBytes: reporter.journalBytes.Load(),
		FallbackWrites: reporter.fallbackWrites.Load(), Replayed: reporter.replayed.Load(),
		ReplayFailures: reporter.replayFailures.Load(), DroppedRecords: reporter.dropped.Load(),
		CorruptRecords: reporter.corrupt.Load(), GapPending: reporter.gapPending.Load(),
		LastReplayUnix:    reporter.lastReplayUnix.Load(),
		PrimaryConfigured: reporter.currentStore() != nil,
	}
}

func (reporter *Reporter) persist(ctx context.Context, record Record) error {
	store := reporter.currentStore()
	if store == nil {
		return ErrPrimaryUnavailable
	}
	detail := map[string]interface{}{
		"component": record.Component, "operation": record.Operation,
		"error_class": record.ErrorClass, "fingerprint": record.Fingerprint,
		"route": record.Route, "status": record.Status, "recovered": record.Recovered,
		"response_committed": record.ResponseCommitted,
		"uptime_millis":      record.UptimeMillis, "backoff_millis": record.BackoffMillis,
		"delivery": "primary",
	}
	if record.FallbackUsed {
		detail["delivery"] = "fallback_replayed"
	}
	if record.DiagnosticGap {
		detail["reason"] = "journal_gap"
		detail["dropped_records"] = record.DroppedRecords
		detail["corrupt_records"] = record.CorruptRecords
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	entityType, entityAlias := "host", ""
	if record.RequestID != "" {
		entityType, entityAlias = "request", record.RequestID
	}
	return store.AddDiagnosticEvent(ctx, storage.DiagnosticEvent{
		ID: record.ID, EventType: record.EventType, Severity: record.Severity,
		EntityType: entityType, EntityAlias: entityAlias, DetailJSON: string(raw),
		DiagnosticGap: record.DiagnosticGap, CreatedAt: record.CreatedAt,
	})
}

func (reporter *Reporter) currentStore() *storage.Store {
	reporter.storeMu.RLock()
	defer reporter.storeMu.RUnlock()
	return reporter.store
}

func (reporter *Reporter) writeRecordLocked(record Record) error {
	path := filepath.Join(reporter.dir, journalRecordName(record.ID))
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("incident journal record %s is not regular", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if int64(len(raw)) > journalRecordBytes {
		return fmt.Errorf("incident journal record exceeds %d bytes", journalRecordBytes)
	}
	temporary, err := os.CreateTemp(reporter.dir, ".incident-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(datadir.SecretMode); err == nil {
		_, err = temporary.Write(append(raw, '\n'))
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(reporter.dir)
}

type journalFile struct {
	name    string
	path    string
	size    int64
	modTime time.Time
}

func (reporter *Reporter) journalFilesLocked() ([]journalFile, error) {
	entries, err := os.ReadDir(reporter.dir)
	if err != nil {
		return nil, err
	}
	files := make([]journalFile, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "event-SEVT-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, infoErr
		}
		files = append(files, journalFile{
			name: entry.Name(), path: filepath.Join(reporter.dir, entry.Name()),
			size: info.Size(), modTime: info.ModTime(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].name < files[j].name
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	return files, nil
}

func (reporter *Reporter) readRecordLocked(path string) (Record, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Record{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > journalRecordBytes {
		return Record{}, errors.New("invalid incident journal record")
	}
	file, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, journalRecordBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return Record{}, readErr
	}
	if closeErr != nil {
		return Record{}, closeErr
	}
	var record Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return Record{}, err
	}
	if !eventIDPattern.MatchString(record.ID) || journalRecordName(record.ID) != filepath.Base(path) {
		return Record{}, errors.New("incident journal record id mismatch")
	}
	return normalizeRecord(record), nil
}

func (reporter *Reporter) enforceBoundsLocked(currentID string) error {
	files, err := reporter.journalFilesLocked()
	if err != nil {
		return err
	}
	var total int64
	for _, file := range files {
		total += file.size
	}
	remove := make([]journalFile, 0)
	remaining := len(files)
	for _, file := range files {
		if remaining <= journalRecordLimit && total <= journalByteLimit {
			break
		}
		if strings.Contains(file.name, currentID) && remaining > 1 {
			continue
		}
		remove = append(remove, file)
		remaining--
		total -= file.size
	}
	if len(remove) == 0 {
		return nil
	}
	if err = reporter.markGapLocked(uint64(len(remove)), 0); err != nil {
		return err
	}
	for _, file := range remove {
		if err = os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	reporter.dropped.Add(uint64(len(remove)))
	return syncDirectory(reporter.dir)
}

type gapMarker struct {
	ID      string `json:"id"`
	Dropped uint64 `json:"dropped"`
	Corrupt uint64 `json:"corrupt"`
}

func (reporter *Reporter) markGapLocked(dropped, corrupt uint64) error {
	path := filepath.Join(reporter.dir, journalGapName)
	marker := gapMarker{ID: newIncidentID()}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &marker)
		if !eventIDPattern.MatchString(marker.ID) {
			marker.ID = newIncidentID()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	marker.Dropped += dropped
	marker.Corrupt += corrupt
	return writeAtomicJSON(reporter.dir, journalGapName, marker)
}

func (reporter *Reporter) replayGapLocked(ctx context.Context) error {
	path := filepath.Join(reporter.dir, journalGapName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var marker gapMarker
	if err = json.Unmarshal(raw, &marker); err != nil || !eventIDPattern.MatchString(marker.ID) {
		marker = gapMarker{ID: newIncidentID(), Corrupt: 1}
	}
	record := normalizeRecord(Record{
		ID: marker.ID, CreatedAt: time.Now().Unix(), EventType: "exception_journal_gap",
		Severity: "warning", Component: "exception_reporter", Operation: "replay",
		ErrorClass: "journal_gap", DiagnosticGap: true,
		DroppedRecords: marker.Dropped, CorruptRecords: marker.Corrupt,
	})
	if err = reporter.persist(ctx, record); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(reporter.dir)
}

func (reporter *Reporter) refreshJournalStatsLocked() error {
	files, err := reporter.journalFilesLocked()
	if err != nil {
		return err
	}
	var bytes int64
	for _, file := range files {
		bytes += file.size
	}
	gapInfo, gapErr := os.Lstat(filepath.Join(reporter.dir, journalGapName))
	if gapErr == nil {
		if !gapInfo.Mode().IsRegular() || gapInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("incident diagnostic gap marker is not regular")
		}
		bytes += gapInfo.Size()
		reporter.gapPending.Store(true)
	} else if errors.Is(gapErr, os.ErrNotExist) {
		reporter.gapPending.Store(false)
	} else {
		return gapErr
	}
	reporter.pending.Store(uint64(len(files)))
	reporter.journalBytes.Store(bytes)
	return nil
}

func (reporter *Reporter) wakeReplay() {
	select {
	case reporter.wake <- struct{}{}:
	default:
	}
}

func recordFromEvent(event supervisor.Event, fallback bool) Record {
	record := Record{
		ID: event.ID, CreatedAt: event.TimeUnix, EventType: event.Type,
		Severity: event.Severity, Component: event.Module, Operation: event.Operation,
		ErrorClass: event.ErrorClass, Fingerprint: event.Fingerprint,
		RequestID: event.RequestID, Route: event.Route, Status: event.Status,
		Recovered: event.Recovered, ResponseCommitted: event.ResponseCommitted,
		UptimeMillis: event.UptimeMillis, BackoffMillis: event.BackoffMillis,
		FallbackUsed: fallback,
	}
	if !fingerprintPattern.MatchString(strings.ToLower(strings.TrimSpace(record.Fingerprint))) {
		h := sha256.Sum256([]byte(strings.Join([]string{
			event.Type, event.Module, event.Operation, event.ErrorClass,
			event.Route, strconv.Itoa(event.Status),
		}, "\x00")))
		record.Fingerprint = "sha256:" + hex.EncodeToString(h[:16])
	}
	return normalizeRecord(record)
}

func normalizeRecord(record Record) Record {
	if !eventIDPattern.MatchString(strings.ToUpper(strings.TrimSpace(record.ID))) {
		record.ID = newIncidentID()
	} else {
		record.ID = strings.ToUpper(strings.TrimSpace(record.ID))
	}
	if record.CreatedAt <= 0 {
		record.CreatedAt = time.Now().Unix()
	}
	record.EventType = canonicalToken(record.EventType, "event")
	record.Severity = canonicalSeverity(record.Severity)
	record.Component = canonicalToken(record.Component, "background")
	record.Operation = canonicalToken(record.Operation, "unspecified")
	record.ErrorClass = canonicalToken(record.ErrorClass, "unknown")
	record.Route = canonicalToken(record.Route, "unknown")
	record.RequestID = strings.ToUpper(strings.TrimSpace(record.RequestID))
	if !requestIDPattern.MatchString(record.RequestID) {
		record.RequestID = ""
	}
	record.Fingerprint = strings.ToLower(strings.TrimSpace(record.Fingerprint))
	if !fingerprintPattern.MatchString(record.Fingerprint) {
		h := sha256.Sum256([]byte(strings.Join([]string{
			record.EventType, record.Component, record.Operation, record.ErrorClass,
			record.Route, strconv.Itoa(record.Status),
		}, "\x00")))
		record.Fingerprint = "sha256:" + hex.EncodeToString(h[:16])
	}
	if record.Status < 0 || record.Status > 999 {
		record.Status = 0
	}
	if record.UptimeMillis < 0 {
		record.UptimeMillis = 0
	}
	if record.BackoffMillis < 0 {
		record.BackoffMillis = 0
	}
	return record
}

func canonicalSeverity(value string) string {
	switch canonicalToken(value, "info") {
	case "debug", "info", "warning", "error", "critical", "emergency":
		return canonicalToken(value, "info")
	default:
		return "info"
	}
}

func canonicalToken(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastSeparator := false
	for _, char := range value {
		valid := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == ':' || char == '-'
		if !valid {
			if out.Len() > 0 && !lastSeparator {
				out.WriteByte('_')
				lastSeparator = true
			}
			continue
		}
		if out.Len() >= 64 {
			break
		}
		out.WriteRune(char)
		lastSeparator = false
	}
	result := strings.Trim(out.String(), "_.:-")
	if result == "" {
		result = fallback
	}
	if result[0] < 'a' || result[0] > 'z' {
		result = "x_" + result
	}
	if len(result) > 64 {
		result = result[:64]
	}
	return result
}

func journalRecordName(id string) string {
	return "event-" + id + ".json"
}

func newIncidentID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "SEVT-" + strings.ToUpper(hex.EncodeToString(value[:]))
	}
	return fmt.Sprintf("SEVT-%016X%016X", uint64(time.Now().UnixNano()), incidentIDFallback.Add(1))
}

func writeAtomicJSON(dir, name string, value interface{}) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".incident-meta-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err = temporary.Chmod(datadir.SecretMode); err == nil {
		_, err = temporary.Write(append(raw, '\n'))
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = file.Sync()
	return errors.Join(err, file.Close())
}
