package bodysource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
)

const DefaultChunkSize = 64 << 10

var (
	ErrBodyTooLarge = errors.New("request body too large")
	ErrMemoryBudget = errors.New("request body memory budget exhausted")
	ErrSpoolBudget  = errors.New("request body spool budget exhausted")
	ErrDiskReserve  = errors.New("request body spool disk reserve reached")
)

type Budget struct {
	memoryLimit     int64
	spoolLimit      int64
	memoryUsed      atomic.Int64
	spoolUsed       atomic.Int64
	captures        atomic.Int64
	capturedBytes   atomic.Int64
	memoryBytes     atomic.Int64
	spooledBytes    atomic.Int64
	replayOpens     atomic.Int64
	activeSources   atomic.Int64
	memoryFallbacks atomic.Int64
	spoolRejections atomic.Int64
	spillCount      atomic.Int64
}

type BudgetSnapshot struct {
	MemoryLimit     int64 `json:"memory_limit"`
	MemoryUsed      int64 `json:"memory_used"`
	SpoolLimit      int64 `json:"spool_limit"`
	SpoolUsed       int64 `json:"spool_used"`
	Captures        int64 `json:"captures"`
	CapturedBytes   int64 `json:"captured_bytes"`
	MemoryBytes     int64 `json:"memory_captured_bytes"`
	SpooledBytes    int64 `json:"spooled_bytes"`
	ReplayOpens     int64 `json:"replay_opens"`
	ActiveSources   int64 `json:"active_sources"`
	MemoryFallbacks int64 `json:"memory_fallbacks"`
	SpoolRejections int64 `json:"spool_rejections"`
	SpillCount      int64 `json:"spill_count"`
}

func NewBudget(memoryLimit, spoolLimit int64) *Budget {
	return &Budget{memoryLimit: max64(memoryLimit, 0), spoolLimit: max64(spoolLimit, 0)}
}

func (b *Budget) Snapshot() BudgetSnapshot {
	if b == nil {
		return BudgetSnapshot{}
	}
	return BudgetSnapshot{
		MemoryLimit: b.memoryLimit, MemoryUsed: b.memoryUsed.Load(), SpoolLimit: b.spoolLimit, SpoolUsed: b.spoolUsed.Load(),
		Captures: b.captures.Load(), CapturedBytes: b.capturedBytes.Load(), MemoryBytes: b.memoryBytes.Load(), SpooledBytes: b.spooledBytes.Load(),
		ReplayOpens: b.replayOpens.Load(), ActiveSources: b.activeSources.Load(), MemoryFallbacks: b.memoryFallbacks.Load(),
		SpoolRejections: b.spoolRejections.Load(), SpillCount: b.spillCount.Load(),
	}
}

func (b *Budget) reserveMemory(n int64) bool {
	if b == nil {
		return true
	}
	ok := reserve(&b.memoryUsed, b.memoryLimit, n)
	if !ok {
		b.memoryFallbacks.Add(1)
	}
	return ok
}
func (b *Budget) reserveSpool(n int64) bool {
	if b == nil {
		return true
	}
	ok := reserve(&b.spoolUsed, b.spoolLimit, n)
	if !ok {
		b.spoolRejections.Add(1)
	}
	return ok
}
func (b *Budget) releaseMemory(n int64) {
	if b != nil && n > 0 {
		b.memoryUsed.Add(-n)
	}
}
func (b *Budget) releaseSpool(n int64) {
	if b != nil && n > 0 {
		b.spoolUsed.Add(-n)
	}
}
func (b *Budget) recordCapture(size int64, spooled bool) {
	if b == nil {
		return
	}
	b.captures.Add(1)
	b.capturedBytes.Add(size)
	b.activeSources.Add(1)
	if spooled {
		b.spooledBytes.Add(size)
		b.spillCount.Add(1)
	} else {
		b.memoryBytes.Add(size)
	}
}
func (b *Budget) recordOpen() {
	if b != nil {
		b.replayOpens.Add(1)
	}
}
func (b *Budget) recordClose() {
	if b != nil {
		b.activeSources.Add(-1)
	}
}

func reserve(used *atomic.Int64, limit, n int64) bool {
	if n <= 0 {
		return true
	}
	for {
		current := used.Load()
		if limit > 0 && (current > limit-n) {
			return false
		}
		if used.CompareAndSwap(current, current+n) {
			return true
		}
	}
}

type CaptureOptions struct {
	MaxBytes           int64
	MemoryThreshold    int64
	ExpectedBytes      int64
	ChunkSize          int
	TempDir            string
	MinDiskFreeBytes   int64
	Budget             *Budget
	TempFileNamePrefix string
}

// Capture reads src once and chooses chunked memory or a single spool file.
func Capture(ctx context.Context, src io.Reader, options CaptureOptions) (BodySource, error) {
	if src == nil {
		return Empty(), nil
	}
	chunkSize := options.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	threshold := options.MemoryThreshold
	if threshold < 0 {
		threshold = 0
	}
	if options.MaxBytes <= 0 {
		return nil, ErrBodyTooLarge
	}
	if options.ExpectedBytes > options.MaxBytes {
		return nil, ErrBodyTooLarge
	}
	prefix := options.TempFileNamePrefix
	if prefix == "" {
		prefix = "codex-pool-body-*"
	}
	// Content-Length lets the common in-memory path reserve exactly once and fill
	// one contiguous allocation through bounded reads. A network reader may return
	// only a few KiB from the first Read even when given a MiB-sized destination;
	// retaining that short slice used to pin the full backing array, then force
	// requestBodyBytes to allocate and copy the entire body again.
	if expected := options.ExpectedBytes; expected > 0 && expected <= threshold && expected <= int64(int(^uint(0)>>1)) && options.Budget.reserveMemory(expected) {
		body := make([]byte, int(expected))
		if err := readExpected(ctx, src, body, chunkSize); err != nil {
			options.Budget.releaseMemory(expected)
			return nil, err
		}
		options.Budget.recordCapture(expected, false)
		return &memorySource{chunks: [][]byte{body}, size: expected, budget: options.Budget, reserved: expected}, nil
	} else if expected > 0 && expected <= threshold {
		// A failed whole-body reservation must go directly to the single spool
		// file. Reserving a partial prefix would add copying without increasing
		// the number of requests that can remain in memory.
		threshold = 0
	}
	var chunks [][]byte
	var size, memoryReserved, spoolReserved int64
	var file *os.File
	cleanup := func() {
		options.Budget.releaseMemory(memoryReserved)
		options.Budget.releaseSpool(spoolReserved)
		if file != nil {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name)
		}
	}
	spill := func() error {
		if file != nil {
			return nil
		}
		if err := ensureDiskReserve(options.TempDir, options.MinDiskFreeBytes, options.MaxBytes); err != nil {
			return err
		}
		if !options.Budget.reserveSpool(size) {
			return ErrSpoolBudget
		}
		spoolReserved = size
		var err error
		file, err = os.CreateTemp(options.TempDir, prefix)
		if err != nil {
			return err
		}
		for _, chunk := range chunks {
			if _, err = file.Write(chunk); err != nil {
				return err
			}
		}
		chunks = nil
		options.Budget.releaseMemory(memoryReserved)
		memoryReserved = 0
		return nil
	}
	var spoolBuffer []byte
	for {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, err
		}
		readSize := chunkSize
		if options.ExpectedBytes > 0 {
			switch {
			case size < options.ExpectedBytes && options.ExpectedBytes-size < int64(readSize):
				readSize = int(options.ExpectedBytes - size)
			case size == options.ExpectedBytes:
				readSize = 1
			}
		}
		var chunk []byte
		if file != nil {
			if len(spoolBuffer) < readSize {
				spoolBuffer = make([]byte, readSize)
			}
			chunk = spoolBuffer[:readSize]
		} else {
			chunk = make([]byte, readSize)
		}
		n, err := src.Read(chunk)
		if n > 0 {
			if int64(n) > options.MaxBytes-size {
				cleanup()
				return nil, ErrBodyTooLarge
			}
			chunk = chunk[:n]
			if file == nil && size+int64(n) <= threshold && options.Budget.reserveMemory(int64(n)) {
				memoryReserved += int64(n)
				chunks = append(chunks, chunk)
			} else {
				if spillErr := spill(); spillErr != nil {
					cleanup()
					return nil, spillErr
				}
				if !options.Budget.reserveSpool(int64(n)) {
					cleanup()
					return nil, ErrSpoolBudget
				}
				spoolReserved += int64(n)
				if _, writeErr := file.Write(chunk); writeErr != nil {
					cleanup()
					return nil, writeErr
				}
			}
			size += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		if n == 0 {
			continue
		}
	}
	if file == nil {
		options.Budget.recordCapture(size, false)
		return &memorySource{chunks: chunks, size: size, budget: options.Budget, reserved: memoryReserved}, nil
	}
	path := file.Name()
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, err
	}
	file = nil
	options.Budget.recordCapture(size, true)
	return &fileSource{path: path, size: size, remove: true, budget: options.Budget, reserved: spoolReserved}, nil
}

func readExpected(ctx context.Context, src io.Reader, body []byte, chunkSize int) error {
	offset := 0
	for offset < len(body) {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + chunkSize
		if end > len(body) {
			end = len(body)
		}
		n, err := src.Read(body[offset:end])
		offset += n
		if err != nil {
			if err == io.EOF && offset == len(body) {
				return nil
			}
			if err == io.EOF {
				return fmt.Errorf("body size changed: got %d want %d: %w", offset, len(body), io.ErrUnexpectedEOF)
			}
			return err
		}
	}
	var extra [1]byte
	n, err := src.Read(extra[:])
	if n > 0 {
		return fmt.Errorf("body size changed: got more than %d bytes", len(body))
	}
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}

func ensureDiskReserve(dir string, reserveBytes, incoming int64) error {
	if reserveBytes <= 0 {
		return nil
	}
	if dir == "" {
		dir = os.TempDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(dir, &stat); err != nil {
		return fmt.Errorf("stat spool filesystem: %w", err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available-incoming < reserveBytes {
		return ErrDiskReserve
	}
	return nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
