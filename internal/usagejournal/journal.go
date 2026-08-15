package usagejournal

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"codex-account-pool/internal/storage"
)

const (
	defaultSegmentBytes = int64(8 << 20)
	maxRecordBytes      = uint32(16 << 20)
	segmentPrefix       = "usage-"
	segmentSuffix       = ".journal"
	cursorName          = "cursor"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)
var errReplayLimit = errors.New("usage journal replay limit reached")

type Record struct {
	Sequence        uint64                        `json:"sequence"`
	Usage           *storage.UsageRecordWrite     `json:"usage,omitempty"`
	Hold            *storage.BillingHoldWrite     `json:"hold,omitempty"`
	UpstreamAttempt *storage.CodexUpstreamAttempt `json:"upstream_attempt,omitempty"`
}

type Snapshot struct {
	AckedSequence uint64 `json:"acked_sequence"`
	NextSequence  uint64 `json:"next_sequence"`
	Pending       uint64 `json:"pending"`
	Bytes         int64  `json:"bytes"`
	Segments      int    `json:"segments"`
}

type Journal struct {
	dir          string
	segmentBytes int64
	mu           sync.Mutex
	file         *os.File
	fileStart    uint64
	fileSize     int64
	next         uint64
	acked        uint64
	closed       bool
}

func Open(dir string, segmentBytes int64) (*Journal, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || dir == "" {
		return nil, errors.New("usage journal directory is empty")
	}
	if segmentBytes <= 0 {
		segmentBytes = defaultSegmentBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create usage journal: %w", err)
	}
	acked, err := readCursor(dir)
	if err != nil {
		return nil, err
	}
	j := &Journal{dir: dir, segmentBytes: segmentBytes, next: 1, acked: acked}
	segments, err := listSegments(dir)
	if err != nil {
		return nil, err
	}
	for index, segment := range segments {
		last, size, scanErr := scanSegment(segment.path, index == len(segments)-1, true, nil)
		if scanErr != nil {
			return nil, scanErr
		}
		if last >= j.next {
			j.next = last + 1
		}
		if segment.start > j.next {
			j.next = segment.start
		}
		if index == len(segments)-1 {
			j.fileStart, j.fileSize = segment.start, size
		}
	}
	if j.acked >= j.next {
		return nil, fmt.Errorf("usage journal cursor %d exceeds last sequence %d", j.acked, j.next-1)
	}
	if len(segments) == 0 || j.fileSize >= j.segmentBytes {
		if err := j.openNewSegment(); err != nil {
			return nil, err
		}
	} else {
		j.file, err = os.OpenFile(segments[len(segments)-1].path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
	}
	return j, nil
}

func (j *Journal) Append(record Record) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.file == nil {
		return 0, errors.New("usage journal is closed")
	}
	record.Sequence = j.next
	payload, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	if len(payload) == 0 || uint64(len(payload)) > uint64(maxRecordBytes) {
		return 0, fmt.Errorf("usage journal record size %d is invalid", len(payload))
	}
	frameBytes := int64(8 + len(payload))
	if j.fileSize > 0 && j.fileSize+frameBytes > j.segmentBytes {
		if err = j.rotate(); err != nil {
			return 0, err
		}
	}
	var header [8]byte
	binary.LittleEndian.PutUint32(header[:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[4:], crc32.Checksum(payload, crcTable))
	start := j.fileSize
	if err = writeAll(j.file, header[:]); err == nil {
		err = writeAll(j.file, payload)
	}
	if err != nil {
		_ = j.file.Truncate(start)
		_, _ = j.file.Seek(0, io.SeekEnd)
		return 0, err
	}
	j.fileSize += frameBytes
	j.next++
	return record.Sequence, nil
}

func (j *Journal) Replay(limit int) ([]Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, errors.New("usage journal is closed")
	}
	if limit <= 0 {
		limit = 256
	}
	segments, err := listSegments(j.dir)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, limit)
	for index, segment := range segments {
		_, _, err = scanSegment(segment.path, index == len(segments)-1, false, func(record Record) error {
			if record.Sequence > j.acked {
				out = append(out, record)
				if len(out) >= limit {
					return errReplayLimit
				}
			}
			return nil
		})
		if errors.Is(err, errReplayLimit) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (j *Journal) Sync() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.file == nil {
		return errors.New("usage journal is closed")
	}
	return j.file.Sync()
}

func (j *Journal) Ack(sequence uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.file == nil {
		return errors.New("usage journal is closed")
	}
	if sequence <= j.acked {
		return nil
	}
	if sequence >= j.next {
		return fmt.Errorf("usage journal ack %d exceeds last sequence %d", sequence, j.next-1)
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	if err := writeCursor(j.dir, sequence); err != nil {
		return err
	}
	j.acked = sequence
	return j.removeAckedSegments()
}

func (j *Journal) Snapshot() (Snapshot, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	segments, err := listSegments(j.dir)
	if err != nil {
		return Snapshot{}, err
	}
	var bytes int64
	for _, segment := range segments {
		info, statErr := os.Stat(segment.path)
		if statErr != nil {
			return Snapshot{}, statErr
		}
		bytes += info.Size()
	}
	pending := uint64(0)
	if j.next > j.acked+1 {
		pending = j.next - j.acked - 1
	}
	return Snapshot{AckedSequence: j.acked, NextSequence: j.next, Pending: pending, Bytes: bytes, Segments: len(segments)}, nil
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if j.file == nil {
		return nil
	}
	err := j.file.Sync()
	return errors.Join(err, j.file.Close())
}

func (j *Journal) rotate() error {
	if err := j.file.Sync(); err != nil {
		return err
	}
	if err := j.file.Close(); err != nil {
		return err
	}
	j.file = nil
	return j.openNewSegment()
}

func (j *Journal) openNewSegment() error {
	j.fileStart = j.next
	j.fileSize = 0
	path := filepath.Join(j.dir, segmentName(j.fileStart))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	}
	if err != nil {
		return err
	}
	j.file = file
	return nil
}

func (j *Journal) removeAckedSegments() error {
	segments, err := listSegments(j.dir)
	if err != nil {
		return err
	}
	for index := 0; index+1 < len(segments); index++ {
		if segments[index+1].start-1 > j.acked {
			break
		}
		if err = os.Remove(segments[index].path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type segmentInfo struct {
	start uint64
	path  string
}

func listSegments(dir string) ([]segmentInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	segments := make([]segmentInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(name, segmentPrefix), segmentSuffix)
		start, parseErr := strconv.ParseUint(raw, 10, 64)
		if parseErr != nil || start == 0 {
			return nil, fmt.Errorf("invalid usage journal segment %q", name)
		}
		segments = append(segments, segmentInfo{start: start, path: filepath.Join(dir, name)})
	}
	sort.Slice(segments, func(i, k int) bool { return segments[i].start < segments[k].start })
	for index := 1; index < len(segments); index++ {
		if segments[index-1].start == segments[index].start {
			return nil, fmt.Errorf("duplicate usage journal segment start %d", segments[index].start)
		}
	}
	return segments, nil
}

func scanSegment(path string, last, repairTail bool, visit func(Record) error) (uint64, int64, error) {
	flags := os.O_RDONLY
	if repairTail {
		flags = os.O_RDWR
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	var offset int64
	var lastSequence uint64
	for {
		var header [8]byte
		_, err = io.ReadFull(file, header[:])
		if errors.Is(err, io.EOF) {
			return lastSequence, offset, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) && last && repairTail {
			return lastSequence, offset, file.Truncate(offset)
		}
		if err != nil {
			return 0, offset, fmt.Errorf("read usage journal header %s: %w", path, err)
		}
		length := binary.LittleEndian.Uint32(header[:4])
		checksum := binary.LittleEndian.Uint32(header[4:])
		if length == 0 || length > maxRecordBytes {
			return 0, offset, fmt.Errorf("invalid usage journal record length %d in %s", length, path)
		}
		payload := make([]byte, length)
		_, err = io.ReadFull(file, payload)
		if errors.Is(err, io.ErrUnexpectedEOF) && last && repairTail {
			return lastSequence, offset, file.Truncate(offset)
		}
		if err != nil {
			return 0, offset, fmt.Errorf("read usage journal payload %s: %w", path, err)
		}
		if crc32.Checksum(payload, crcTable) != checksum {
			return 0, offset, fmt.Errorf("usage journal checksum mismatch in %s at %d", path, offset)
		}
		var record Record
		if err = json.Unmarshal(payload, &record); err != nil {
			return 0, offset, fmt.Errorf("decode usage journal %s at %d: %w", path, offset, err)
		}
		if record.Sequence == 0 || record.Sequence <= lastSequence || (record.Usage == nil && record.Hold == nil && record.UpstreamAttempt == nil) {
			return 0, offset, fmt.Errorf("invalid usage journal sequence %d in %s", record.Sequence, path)
		}
		lastSequence = record.Sequence
		offset += int64(8 + length)
		if visit != nil {
			if err = visit(record); err != nil {
				return 0, offset, err
			}
		}
	}
}

func segmentName(start uint64) string {
	return fmt.Sprintf("%s%020d%s", segmentPrefix, start, segmentSuffix)
}

func readCursor(dir string) (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(dir, cursorName))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ":")
	if len(parts) != 2 {
		return 0, errors.New("invalid usage journal cursor")
	}
	sequence, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, errors.New("invalid usage journal cursor sequence")
	}
	want, err := hex.DecodeString(parts[1])
	if err != nil || len(want) != 4 {
		return 0, errors.New("invalid usage journal cursor checksum")
	}
	if binary.BigEndian.Uint32(want) != crc32.Checksum([]byte(parts[0]), crcTable) {
		return 0, errors.New("usage journal cursor checksum mismatch")
	}
	return sequence, nil
}

func writeCursor(dir string, sequence uint64) error {
	value := strconv.FormatUint(sequence, 10)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.Checksum([]byte(value), crcTable))
	payload := []byte(value + ":" + hex.EncodeToString(checksum[:]) + "\n")
	temp, err := os.CreateTemp(dir, ".cursor-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err == nil {
		err = writeAll(temp, payload)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tempPath, filepath.Join(dir, cursorName)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	return errors.Join(err, directory.Close())
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		payload = payload[written:]
	}
	return nil
}
