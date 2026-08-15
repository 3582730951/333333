// Package corestate persists the small, database-independent state needed by the
// stable relay. It intentionally knows nothing about SQL migrations, the SPA, or
// optional workers: a relay can recover its last committed worker route by reading
// one of two authenticated snapshots even while every L2 module is unavailable.
package corestate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	SchemaVersion      = 1
	envelopeVersion    = uint16(1)
	snapshotMagic      = "CPSNAP01"
	journalName        = "core-state.journal"
	lockName           = "state-writer.lock"
	snapshotAName      = "core-state.a.snapshot"
	snapshotBName      = "core-state.b.snapshot"
	maxEnvelopeBytes   = 8 << 20
	compactJournalSize = 16 << 20
	maxRecentUpdates   = 256
)

var (
	ErrNoSnapshot = errors.New("no valid core state snapshot")
	crcTable      = crc32.MakeTable(crc32.Castagnoli)
)

// ContextRoute is deliberately opaque to the relay beyond the worker target. It
// gives context-routing code a durable home without teaching the survival kernel
// about provider credentials or mutable database schemas.
type ContextRoute struct {
	ContextHash string `json:"context_hash"`
	Worker      string `json:"worker"`
	Strict      bool   `json:"strict"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Snapshot is the complete versioned state consumed by the survival kernel.
// RecentUpdates bounds idempotency metadata; it is encrypted with the rest of the
// payload and never exposed by the handoff status endpoint.
type Snapshot struct {
	SchemaVersion  int                     `json:"schema_version"`
	Generation     uint64                  `json:"generation"`
	CommittedAt    int64                   `json:"committed_at"`
	ReleaseID      string                  `json:"release_id,omitempty"`
	FencingToken   int64                   `json:"fencing_token,omitempty"`
	ActiveWorker   string                  `json:"active_worker"`
	PreviousWorker string                  `json:"previous_worker,omitempty"`
	ContextRoutes  map[string]ContextRoute `json:"context_routes,omitempty"`
	RecentUpdates  []string                `json:"recent_updates,omitempty"`
}

func (s Snapshot) validate() error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported core state schema %d", s.SchemaVersion)
	}
	if s.Generation == 0 {
		return errors.New("core state generation is zero")
	}
	if !validWorkerPath(s.ActiveWorker) {
		return errors.New("core state active worker is not an absolute socket path")
	}
	if s.PreviousWorker != "" && !validWorkerPath(s.PreviousWorker) {
		return errors.New("core state previous worker is not an absolute socket path")
	}
	for key, route := range s.ContextRoutes {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(route.ContextHash) == "" || !validWorkerPath(route.Worker) {
			return fmt.Errorf("invalid core context route %q", key)
		}
	}
	return nil
}

func validWorkerPath(path string) bool {
	path = strings.TrimSpace(path)
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

type journalRecord struct {
	UpdateID string   `json:"update_id"`
	State    Snapshot `json:"state"`
}

// Reader performs a fresh A/B read for each Load. It carries no dependency on a
// worker process and never repairs or writes persistent state.
type Reader struct {
	dir  string
	aead cipher.AEAD
}

func NewReader(dir string, key []byte) (*Reader, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || dir == "" {
		return nil, errors.New("core state directory is empty")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Reader{dir: dir, aead: aead}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("core state key must contain exactly 32 bytes, got %d", len(key))
	}
	// Domain separation means a dedicated core-state envelope key is used even if
	// an operator temporarily provisions the same source bytes for another store.
	material := sha256.Sum256(append([]byte("codex-pool/core-state/v1\x00"), key...))
	block, err := aes.NewCipher(material[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (r *Reader) Load() (Snapshot, error) {
	return r.loadSnapshots()
}

func (r *Reader) loadSnapshots() (Snapshot, error) {
	var candidates []Snapshot
	var failures []error
	for _, name := range []string{snapshotAName, snapshotBName} {
		raw, err := os.ReadFile(filepath.Join(r.dir, name))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("read %s: %w", name, err))
			}
			continue
		}
		var snapshot Snapshot
		if err = r.openJSON(raw, &snapshot); err != nil {
			failures = append(failures, fmt.Errorf("open %s: %w", name, err))
			continue
		}
		if err = snapshot.validate(); err != nil {
			failures = append(failures, fmt.Errorf("validate %s: %w", name, err))
			continue
		}
		candidates = append(candidates, snapshot)
	}
	if len(candidates) == 0 {
		if len(failures) == 0 {
			return Snapshot{}, ErrNoSnapshot
		}
		return Snapshot{}, errors.Join(append([]error{ErrNoSnapshot}, failures...)...)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Generation > candidates[j].Generation })
	return cloneSnapshot(candidates[0]), nil
}

func (r *Reader) sealJSON(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, r.aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	header := make([]byte, len(snapshotMagic)+2)
	copy(header, snapshotMagic)
	binary.BigEndian.PutUint16(header[len(snapshotMagic):], envelopeVersion)
	ciphertext := r.aead.Seal(nil, nonce, payload, header)
	out := make([]byte, 0, len(header)+len(nonce)+len(ciphertext))
	out = append(out, header...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func (r *Reader) openJSON(raw []byte, target any) error {
	headerSize := len(snapshotMagic) + 2
	minimum := headerSize + r.aead.NonceSize() + r.aead.Overhead()
	if len(raw) < minimum || len(raw) > maxEnvelopeBytes {
		return fmt.Errorf("invalid core state envelope length %d", len(raw))
	}
	header := raw[:headerSize]
	if string(header[:len(snapshotMagic)]) != snapshotMagic || binary.BigEndian.Uint16(header[len(snapshotMagic):]) != envelopeVersion {
		return errors.New("invalid core state envelope header")
	}
	nonceEnd := headerSize + r.aead.NonceSize()
	payload, err := r.aead.Open(nil, raw[headerSize:nonceEnd], raw[nonceEnd:], header)
	if err != nil {
		return fmt.Errorf("authenticate core state: %w", err)
	}
	if err = json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode core state: %w", err)
	}
	return nil
}

// Writer is the sole mutation path. An in-process mutex and an advisory flock
// serialize active A/B worker processes sharing one data directory.
type Writer struct {
	mu     sync.Mutex
	reader *Reader
}

func OpenWriter(dir string, key []byte) (*Writer, error) {
	reader, err := NewReader(dir, key)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(reader.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create core state directory: %w", err)
	}
	if err = os.Chmod(reader.dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure core state directory: %w", err)
	}
	w := &Writer{reader: reader}
	// A journal append can be durable before its matching snapshot rename. Repair
	// that narrow crash window as soon as the next state-writer starts.
	if err = w.withLock(func() error {
		state, _, recovered, recoverErr := w.loadRecovered(true)
		if recoverErr != nil && !errors.Is(recoverErr, ErrNoSnapshot) {
			return recoverErr
		}
		if recovered {
			return w.writeSnapshot(state)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return w, nil
}

// Commit appends a durable idempotent update before atomically replacing one A/B
// slot. Repeating updateID returns the already committed generation unchanged.
func (w *Writer) Commit(updateID string, mutate func(*Snapshot) error) (Snapshot, error) {
	updateID = strings.TrimSpace(updateID)
	if updateID == "" {
		return Snapshot{}, errors.New("core state update id is empty")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var committed Snapshot
	err := w.withLock(func() error {
		current, seen, _, err := w.loadRecovered(true)
		if err != nil && !errors.Is(err, ErrNoSnapshot) {
			return err
		}
		if _, ok := seen[updateID]; ok {
			committed = current
			return nil
		}
		next := cloneSnapshot(current)
		if next.SchemaVersion == 0 {
			next.SchemaVersion = SchemaVersion
		}
		if mutate != nil {
			if err = mutate(&next); err != nil {
				return err
			}
		}
		next.SchemaVersion = SchemaVersion
		next.Generation = current.Generation + 1
		next.CommittedAt = time.Now().UTC().UnixNano()
		next.RecentUpdates = appendRecentUpdate(next.RecentUpdates, updateID)
		if err = next.validate(); err != nil {
			return err
		}
		record := journalRecord{UpdateID: updateID, State: next}
		if err = w.appendJournal(record); err != nil {
			return err
		}
		if err = w.writeSnapshot(next); err != nil {
			return err
		}
		if err = w.compactJournalIfNeeded(record); err != nil {
			return err
		}
		committed = cloneSnapshot(next)
		return nil
	})
	return committed, err
}

func (w *Writer) withLock(run func() error) error {
	file, err := os.OpenFile(filepath.Join(w.reader.dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open core state writer lock: %w", err)
	}
	defer file.Close()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock core state writer: %w", err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return run()
}

func (w *Writer) loadRecovered(repairTail bool) (Snapshot, map[string]struct{}, bool, error) {
	base, snapshotErr := w.reader.loadSnapshots()
	if snapshotErr != nil && !errors.Is(snapshotErr, ErrNoSnapshot) {
		// One completely invalid snapshot set must not hide a valid authenticated
		// journal. Replay below decides whether recovery is possible.
		base = Snapshot{}
	}
	records, err := w.readJournal(repairTail)
	if err != nil {
		return Snapshot{}, nil, false, err
	}
	seen := make(map[string]struct{}, len(base.RecentUpdates)+len(records))
	for _, id := range base.RecentUpdates {
		seen[id] = struct{}{}
	}
	recovered := false
	for _, record := range records {
		seen[record.UpdateID] = struct{}{}
		if record.State.Generation <= base.Generation {
			continue
		}
		if err = record.State.validate(); err != nil {
			return Snapshot{}, nil, false, fmt.Errorf("validate core state journal generation %d: %w", record.State.Generation, err)
		}
		base = cloneSnapshot(record.State)
		recovered = true
	}
	if base.Generation == 0 {
		if snapshotErr != nil {
			return Snapshot{}, seen, false, snapshotErr
		}
		return Snapshot{}, seen, false, ErrNoSnapshot
	}
	return base, seen, recovered, nil
}

func (w *Writer) appendJournal(record journalRecord) error {
	envelope, err := w.reader.sealJSON(record)
	if err != nil {
		return err
	}
	if len(envelope) > maxEnvelopeBytes {
		return errors.New("core state journal record is too large")
	}
	file, err := os.OpenFile(filepath.Join(w.reader.dir, journalName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open core state journal: %w", err)
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], uint32(len(envelope)))
	binary.BigEndian.PutUint32(header[4:], crc32.Checksum(envelope, crcTable))
	if err = writeFull(file, header[:]); err == nil {
		err = writeFull(file, envelope)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return fmt.Errorf("append core state journal: %w", err)
	}
	return nil
}

func (w *Writer) readJournal(repairTail bool) ([]journalRecord, error) {
	path := filepath.Join(w.reader.dir, journalName)
	flags := os.O_RDONLY
	if repairTail {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open core state journal: %w", err)
	}
	defer file.Close()
	var records []journalRecord
	var offset int64
	for {
		var header [8]byte
		n, readErr := io.ReadFull(file, header[:])
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if repairTail && (readErr == io.ErrUnexpectedEOF || n > 0) {
				if truncateErr := file.Truncate(offset); truncateErr != nil {
					return nil, truncateErr
				}
				break
			}
			return nil, fmt.Errorf("read core state journal header: %w", readErr)
		}
		length := binary.BigEndian.Uint32(header[:4])
		checksum := binary.BigEndian.Uint32(header[4:])
		if length == 0 || length > maxEnvelopeBytes {
			return nil, fmt.Errorf("invalid core state journal record length %d at %d", length, offset)
		}
		envelope := make([]byte, int(length))
		if _, readErr = io.ReadFull(file, envelope); readErr != nil {
			if repairTail && readErr == io.ErrUnexpectedEOF {
				if truncateErr := file.Truncate(offset); truncateErr != nil {
					return nil, truncateErr
				}
				break
			}
			return nil, fmt.Errorf("read core state journal payload: %w", readErr)
		}
		if crc32.Checksum(envelope, crcTable) != checksum {
			return nil, fmt.Errorf("core state journal checksum mismatch at %d", offset)
		}
		var record journalRecord
		if err = w.reader.openJSON(envelope, &record); err != nil {
			return nil, fmt.Errorf("open core state journal record at %d: %w", offset, err)
		}
		if strings.TrimSpace(record.UpdateID) == "" {
			return nil, fmt.Errorf("core state journal update id missing at %d", offset)
		}
		records = append(records, record)
		offset += int64(len(header)) + int64(length)
	}
	return records, nil
}

func (w *Writer) writeSnapshot(snapshot Snapshot) error {
	envelope, err := w.reader.sealJSON(snapshot)
	if err != nil {
		return err
	}
	name := snapshotAName
	if snapshot.Generation%2 == 0 {
		name = snapshotBName
	}
	return atomicWrite(filepath.Join(w.reader.dir, name), envelope)
}

func (w *Writer) compactJournalIfNeeded(record journalRecord) error {
	info, err := os.Stat(filepath.Join(w.reader.dir, journalName))
	if err != nil || info.Size() < compactJournalSize {
		return err
	}
	envelope, err := w.reader.sealJSON(record)
	if err != nil {
		return err
	}
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], uint32(len(envelope)))
	binary.BigEndian.PutUint32(header[4:], crc32.Checksum(envelope, crcTable))
	return atomicWrite(filepath.Join(w.reader.dir, journalName), append(header[:], envelope...))
}

func atomicWrite(path string, payload []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".core-state-*.next")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err == nil {
		err = writeFull(temp, payload)
	}
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	return errors.Join(err, directory.Close())
}

func writeFull(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func appendRecentUpdate(values []string, update string) []string {
	out := make([]string, 0, minInt(maxRecentUpdates, len(values)+1))
	for _, value := range values {
		if value != "" && value != update {
			out = append(out, value)
		}
	}
	out = append(out, update)
	if len(out) > maxRecentUpdates {
		out = out[len(out)-maxRecentUpdates:]
	}
	return out
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	copy := snapshot
	copy.RecentUpdates = append([]string(nil), snapshot.RecentUpdates...)
	if snapshot.ContextRoutes != nil {
		copy.ContextRoutes = make(map[string]ContextRoute, len(snapshot.ContextRoutes))
		for key, route := range snapshot.ContextRoutes {
			copy.ContextRoutes[key] = route
		}
	}
	return copy
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
