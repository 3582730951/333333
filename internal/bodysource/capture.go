package bodysource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"codex-account-pool/internal/datadir"
)

const DefaultChunkSize = 64 << 10

var (
	ErrBodyTooLarge = errors.New("request body too large")
	ErrMemoryBudget = errors.New("request body memory budget exhausted")
	ErrSpoolBudget  = errors.New("request body spool budget exhausted")
	ErrDiskReserve  = errors.New("request body spool disk reserve reached")
)

type Budget struct {
	// Split budgets are lightweight views over one root. Request and response
	// captures share the same hard ceiling while each side retains a quarter of
	// the configured memory when the other side is busy.
	parent             *Budget
	class              budgetClass
	memoryMu           sync.Mutex
	requestMemoryMin   int64
	responseMemoryMin  int64
	requestMemoryUsed  atomic.Int64
	responseMemoryUsed atomic.Int64
	diskMu             sync.RWMutex
	diskReserver       *DiskReserver
	memoryLimit        int64
	spoolLimit         int64
	memoryUsed         atomic.Int64
	spoolUsed          atomic.Int64
	captures           atomic.Int64
	capturedBytes      atomic.Int64
	memoryBytes        atomic.Int64
	spooledBytes       atomic.Int64
	replayOpens        atomic.Int64
	activeSources      atomic.Int64
	memoryFallbacks    atomic.Int64
	spoolRejections    atomic.Int64
	spillCount         atomic.Int64
}

type BudgetSnapshot struct {
	MemoryLimit           int64 `json:"memory_limit"`
	MemoryUsed            int64 `json:"memory_used"`
	RequestMemoryMinimum  int64 `json:"request_memory_minimum"`
	RequestMemoryUsed     int64 `json:"request_memory_used"`
	ResponseMemoryMinimum int64 `json:"response_memory_minimum"`
	ResponseMemoryUsed    int64 `json:"response_memory_used"`
	SpoolLimit            int64 `json:"spool_limit"`
	SpoolUsed             int64 `json:"spool_used"`
	Captures              int64 `json:"captures"`
	CapturedBytes         int64 `json:"captured_bytes"`
	MemoryBytes           int64 `json:"memory_captured_bytes"`
	SpooledBytes          int64 `json:"spooled_bytes"`
	ReplayOpens           int64 `json:"replay_opens"`
	ActiveSources         int64 `json:"active_sources"`
	MemoryFallbacks       int64 `json:"memory_fallbacks"`
	SpoolRejections       int64 `json:"spool_rejections"`
	SpillCount            int64 `json:"spill_count"`
}

type budgetClass uint8

const (
	budgetClassShared budgetClass = iota
	budgetClassRequest
	budgetClassResponse
)

func NewBudget(memoryLimit, spoolLimit int64) *Budget {
	return &Budget{memoryLimit: max64(memoryLimit, 0), spoolLimit: max64(spoolLimit, 0)}
}

// NewSplitBudget returns request and response views backed by one aggregate
// memory/spool budget. Each class retains reserveFraction of the memory ceiling;
// the remainder is borrowable in either direction. Values outside (0, .5] use
// the production default of 25 percent.
func NewSplitBudget(memoryLimit, spoolLimit int64, reserveFraction float64) (request, response *Budget) {
	if reserveFraction <= 0 || reserveFraction > .5 {
		reserveFraction = .25
	}
	root := NewBudget(memoryLimit, spoolLimit)
	minimum := int64(float64(root.memoryLimit) * reserveFraction)
	root.requestMemoryMin = minimum
	root.responseMemoryMin = minimum
	return &Budget{parent: root, class: budgetClassRequest}, &Budget{parent: root, class: budgetClassResponse}
}

func (b *Budget) root() *Budget {
	if b == nil {
		return nil
	}
	if b.parent != nil {
		return b.parent
	}
	return b
}

// SetDiskReserver associates captures using this budget (and all of its split
// views) with one filesystem-scoped reserver.
func (b *Budget) SetDiskReserver(reserver *DiskReserver) {
	root := b.root()
	if root == nil {
		return
	}
	root.diskMu.Lock()
	root.diskReserver = reserver
	root.diskMu.Unlock()
}

func (b *Budget) DiskReserver() *DiskReserver {
	root := b.root()
	if root == nil {
		return nil
	}
	root.diskMu.RLock()
	defer root.diskMu.RUnlock()
	return root.diskReserver
}

// ReserveMemory and ReleaseMemory let bounded metadata probes participate in
// the same split memory admission without creating a replayable body source.
func (b *Budget) ReserveMemory(n int64) bool { return b.reserveMemory(n) }
func (b *Budget) ReleaseMemory(n int64)      { b.releaseMemory(n) }

func (b *Budget) Snapshot() BudgetSnapshot {
	root := b.root()
	if root == nil {
		return BudgetSnapshot{}
	}
	return BudgetSnapshot{
		MemoryLimit: root.memoryLimit, MemoryUsed: root.memoryUsed.Load(),
		RequestMemoryMinimum: root.requestMemoryMin, RequestMemoryUsed: root.requestMemoryUsed.Load(),
		ResponseMemoryMinimum: root.responseMemoryMin, ResponseMemoryUsed: root.responseMemoryUsed.Load(),
		SpoolLimit: root.spoolLimit, SpoolUsed: root.spoolUsed.Load(),
		Captures: root.captures.Load(), CapturedBytes: root.capturedBytes.Load(), MemoryBytes: root.memoryBytes.Load(), SpooledBytes: root.spooledBytes.Load(),
		ReplayOpens: root.replayOpens.Load(), ActiveSources: root.activeSources.Load(), MemoryFallbacks: root.memoryFallbacks.Load(),
		SpoolRejections: root.spoolRejections.Load(), SpillCount: root.spillCount.Load(),
	}
}

func (b *Budget) reserveMemory(n int64) bool {
	if b == nil {
		return true
	}
	root := b.root()
	root.memoryMu.Lock()
	current := root.memoryUsed.Load()
	allowed := root.memoryLimit <= 0
	if !allowed {
		reservedForOther := int64(0)
		switch b.class {
		case budgetClassRequest:
			reservedForOther = max64(0, root.responseMemoryMin-root.responseMemoryUsed.Load())
		case budgetClassResponse:
			reservedForOther = max64(0, root.requestMemoryMin-root.requestMemoryUsed.Load())
		}
		allowed = n <= root.memoryLimit-current-reservedForOther
	}
	if allowed && n > 0 {
		root.memoryUsed.Store(current + n)
		switch b.class {
		case budgetClassRequest:
			root.requestMemoryUsed.Add(n)
		case budgetClassResponse:
			root.responseMemoryUsed.Add(n)
		}
	}
	root.memoryMu.Unlock()
	ok := allowed
	if !ok {
		root.memoryFallbacks.Add(1)
	}
	return ok
}
func (b *Budget) reserveSpool(n int64) bool {
	if b == nil {
		return true
	}
	root := b.root()
	ok := reserve(&root.spoolUsed, root.spoolLimit, n)
	if !ok {
		root.spoolRejections.Add(1)
	}
	return ok
}
func (b *Budget) releaseMemory(n int64) {
	if b != nil && n > 0 {
		root := b.root()
		root.memoryMu.Lock()
		root.memoryUsed.Add(-n)
		switch b.class {
		case budgetClassRequest:
			root.requestMemoryUsed.Add(-n)
		case budgetClassResponse:
			root.responseMemoryUsed.Add(-n)
		}
		root.memoryMu.Unlock()
	}
}
func (b *Budget) releaseSpool(n int64) {
	if b != nil && n > 0 {
		b.root().spoolUsed.Add(-n)
	}
}
func (b *Budget) recordCapture(size int64, spooled bool) {
	if b == nil {
		return
	}
	root := b.root()
	root.captures.Add(1)
	root.capturedBytes.Add(size)
	root.activeSources.Add(1)
	if spooled {
		root.spooledBytes.Add(size)
		root.spillCount.Add(1)
	} else {
		root.memoryBytes.Add(size)
	}
}
func (b *Budget) recordOpen() {
	if b != nil {
		b.root().replayOpens.Add(1)
	}
}
func (b *Budget) recordClose() {
	if b != nil {
		b.root().activeSources.Add(-1)
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
	DiskReserver       *DiskReserver
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
	if options.DiskReserver == nil && options.Budget != nil {
		options.DiskReserver = options.Budget.DiskReserver()
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
	// A known body larger than the memory threshold goes directly to one exactly
	// reserved spool file. Retaining an in-memory prefix would consume both classes
	// of storage and then copy it without avoiding any disk I/O.
	if options.ExpectedBytes > threshold {
		threshold = 0
	}
	var chunks [][]byte
	var size, memoryReserved, spoolReserved int64
	var file *os.File
	var diskReservation *DiskReservation
	cleanup := func() {
		options.Budget.releaseMemory(memoryReserved)
		options.Budget.releaseSpool(spoolReserved)
		if diskReservation != nil {
			diskReservation.Release()
			diskReservation = nil
		}
		if file != nil {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name)
		}
	}
	spill := func(required int64) error {
		if file != nil {
			if diskReservation != nil {
				return diskReservation.EnsureFileCapacity(file, required)
			}
			return nil
		}
		initial := required
		exact := false
		if options.ExpectedBytes > 0 {
			initial = options.ExpectedBytes
			exact = true
		}
		var err error
		if options.DiskReserver != nil {
			diskReservation, err = options.DiskReserver.NewReservation(initial, exact)
			if err != nil {
				return err
			}
		} else if err = ensureDiskReserve(options.TempDir, options.MinDiskFreeBytes, initial); err != nil {
			return bodyStorageError("reserve spool filesystem", err)
		}
		if !options.Budget.reserveSpool(size) {
			return bodyStorageError("reserve spool budget", ErrSpoolBudget)
		}
		spoolReserved = size
		file, err = os.CreateTemp(options.TempDir, prefix)
		if err != nil {
			return bodyStorageError("create spool file", err)
		}
		if diskReservation != nil {
			if err = diskReservation.EnsureFileCapacity(file, required); err != nil {
				return err
			}
		}
		for _, chunk := range chunks {
			if _, err = file.Write(chunk); err != nil {
				return bodyStorageError("write spool prefix", err)
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
			if options.ExpectedBytes > 0 && int64(n) > options.ExpectedBytes-size {
				cleanup()
				return nil, fmt.Errorf("body size changed: got more than %d bytes", options.ExpectedBytes)
			}
			chunk = chunk[:n]
			if file == nil && size+int64(n) <= threshold && options.Budget.reserveMemory(int64(n)) {
				memoryReserved += int64(n)
				chunks = append(chunks, chunk)
			} else {
				if spillErr := spill(size + int64(n)); spillErr != nil {
					cleanup()
					return nil, spillErr
				}
				if !options.Budget.reserveSpool(int64(n)) {
					cleanup()
					return nil, bodyStorageError("reserve spool budget", ErrSpoolBudget)
				}
				spoolReserved += int64(n)
				if _, writeErr := file.Write(chunk); writeErr != nil {
					cleanup()
					return nil, bodyStorageError("write spool body", writeErr)
				}
			}
			size += int64(n)
		}
		if err == io.EOF {
			if options.ExpectedBytes > 0 && size != options.ExpectedBytes {
				cleanup()
				return nil, fmt.Errorf("body size changed: got %d want %d: %w", size, options.ExpectedBytes, io.ErrUnexpectedEOF)
			}
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
	if err := file.Truncate(size); err != nil {
		cleanup()
		return nil, bodyStorageError("truncate spool file", err)
	}
	if diskReservation != nil {
		diskReservation.CommitSize(size)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, bodyStorageError("sync spool file", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, bodyStorageError("close spool file", err)
	}
	file = nil
	options.Budget.recordCapture(size, true)
	return &fileSource{path: path, size: size, remove: true, budget: options.Budget, reserved: spoolReserved, diskReservation: diskReservation}, nil
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
	} else if err := datadir.RecoverDirectory(dir); err != nil {
		return fmt.Errorf("prepare spool directory: %w", err)
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
