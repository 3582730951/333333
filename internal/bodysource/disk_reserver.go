package bodysource

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"codex-account-pool/internal/datadir"
)

const DiskReservationChunkBytes int64 = 8 << 20

// BodyStorageClass lets the HTTP boundary distinguish a persistent filesystem
// reserve failure from temporary process-local spool admission pressure.
type BodyStorageClass string

const (
	BodyStorageDiskReserve   BodyStorageClass = "disk_reserve"
	BodyStorageLocalCapacity BodyStorageClass = "local_capacity"
)

// BodyStorageError is returned before routing when a complete replayable body
// cannot be retained locally. Cause remains available through errors.Is/As.
type BodyStorageError struct {
	Class BodyStorageClass
	Op    string
	Cause error
}

func (e *BodyStorageError) Error() string {
	if e == nil {
		return "body storage error"
	}
	if e.Cause == nil {
		if e.Op != "" {
			return e.Op + ": body storage error"
		}
		return "body storage error"
	}
	if e.Op == "" {
		return e.Cause.Error()
	}
	return fmt.Sprintf("%s: %v", e.Op, e.Cause)
}

func (e *BodyStorageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func bodyStorageError(op string, err error) error {
	if err == nil {
		return nil
	}
	var existing *BodyStorageError
	if errors.As(err, &existing) {
		return err
	}
	class := BodyStorageLocalCapacity
	if errors.Is(err, ErrDiskReserve) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		class = BodyStorageDiskReserve
	}
	return &BodyStorageError{Class: class, Op: op, Cause: err}
}

// DiskReserver serializes commitments for all request, response and stream spool
// files on one filesystem. Reserved includes promised bytes; Allocated tracks the
// subset already materialized by fallocate so free-space checks never count it
// twice.
type DiskReserver struct {
	dir       string
	minFree   int64
	limit     int64
	mu        sync.Mutex
	reserved  int64
	allocated int64
	active    int64
	reserves  int64
	grows     int64
	releases  int64
	rejects   int64
	lastError string
	managed   bool
}

type DiskReserverSnapshot struct {
	FilesystemTotalBytes     int64  `json:"filesystem_total_bytes"`
	FilesystemAvailableBytes int64  `json:"filesystem_available_bytes"`
	MinimumFreeBytes         int64  `json:"minimum_free_bytes"`
	GlobalLimitBytes         int64  `json:"global_limit_bytes"`
	ReservedBytes            int64  `json:"reserved_bytes"`
	AllocatedBytes           int64  `json:"allocated_bytes"`
	ActiveReservations       int64  `json:"active_reservations"`
	Reservations             int64  `json:"reservations"`
	GrowthOperations         int64  `json:"growth_operations"`
	Releases                 int64  `json:"releases"`
	Rejections               int64  `json:"rejections"`
	LastError                string `json:"last_error,omitempty"`
}

func NewDiskReserver(dir string, minimumFreeBytes, globalLimitBytes int64) *DiskReserver {
	managed := dir != ""
	if stringsTrimmed := filepath.Clean(dir); dir != "" && stringsTrimmed != "." {
		dir = stringsTrimmed
	}
	if dir == "" {
		dir = os.TempDir()
	}
	return &DiskReserver{dir: dir, minFree: max64(minimumFreeBytes, 0), limit: max64(globalLimitBytes, 0), managed: managed}
}

func (d *DiskReserver) Snapshot() DiskReserverSnapshot {
	if d == nil {
		return DiskReserverSnapshot{}
	}
	total, available, statErr := diskFilesystemStats(d.dir)
	d.mu.Lock()
	defer d.mu.Unlock()
	lastError := d.lastError
	if statErr != nil {
		lastError = statErr.Error()
	}
	return DiskReserverSnapshot{
		FilesystemTotalBytes: total, FilesystemAvailableBytes: available,
		MinimumFreeBytes: d.minFree, GlobalLimitBytes: d.limit,
		ReservedBytes: d.reserved, AllocatedBytes: d.allocated,
		ActiveReservations: d.active, Reservations: d.reserves,
		GrowthOperations: d.grows, Releases: d.releases, Rejections: d.rejects,
		LastError: lastError,
	}
}

type DiskReservation struct {
	owner     *DiskReserver
	capacity  int64
	allocated int64
	closed    bool
}

func (d *DiskReserver) NewReservation(initialBytes int64, exact bool) (*DiskReservation, error) {
	if d == nil {
		return nil, nil
	}
	if initialBytes < 0 {
		initialBytes = 0
	}
	if !exact {
		initialBytes = roundDiskReservation(initialBytes)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.reserveLocked(initialBytes); err != nil {
		d.rejects++
		d.lastError = err.Error()
		return nil, bodyStorageError("reserve spool filesystem", err)
	}
	d.active++
	d.reserves++
	return &DiskReservation{owner: d, capacity: initialBytes}, nil
}

func (d *DiskReserver) reserveLocked(delta int64) error {
	if delta <= 0 {
		return nil
	}
	if d.limit > 0 {
		if delta > d.limit || d.reserved > d.limit-delta {
			return ErrSpoolBudget
		}
	}
	if d.managed {
		if err := datadir.RecoverDirectory(d.dir); err != nil {
			return fmt.Errorf("prepare spool directory: %w", err)
		}
	}
	_, available, err := diskFilesystemStats(d.dir)
	if err != nil {
		return fmt.Errorf("stat spool filesystem: %w", err)
	}
	outstanding := max64(d.reserved-d.allocated, 0)
	if outstanding > available {
		return ErrDiskReserve
	}
	usable := available - outstanding
	if d.minFree > usable || delta > usable-d.minFree {
		return ErrDiskReserve
	}
	d.reserved += delta
	return nil
}

// EnsureFileCapacity grows an unknown-length reservation in 8 MiB chunks and
// materializes the new range on Linux. Known-length reservations arrive with an
// exact initial capacity and are preallocated in one operation.
func (r *DiskReservation) EnsureFileCapacity(file *os.File, required int64) error {
	if r == nil || r.owner == nil || required <= 0 {
		return nil
	}
	d := r.owner
	d.mu.Lock()
	if r.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	if required > r.capacity {
		newCapacity := roundDiskReservation(required)
		delta := newCapacity - r.capacity
		if err := d.reserveLocked(delta); err != nil {
			d.rejects++
			d.lastError = err.Error()
			d.mu.Unlock()
			return bodyStorageError("grow spool reservation", err)
		}
		r.capacity = newCapacity
		d.grows++
	}
	start := r.allocated
	length := r.capacity - r.allocated
	d.mu.Unlock()

	if file == nil || length <= 0 {
		return nil
	}
	materialized, err := preallocateFile(file, start, length)
	if err != nil {
		return bodyStorageError("preallocate spool file", err)
	}
	if !materialized {
		return nil
	}

	d.mu.Lock()
	if !r.closed {
		// Another writer cannot use a reservation concurrently, but retain a
		// monotonic guard so an idempotent call never double-counts allocation.
		end := start + length
		if end > r.allocated {
			delta := end - r.allocated
			r.allocated = end
			d.allocated += delta
		}
	}
	_, available, statErr := diskFilesystemStats(d.dir)
	outstanding := max64(d.reserved-d.allocated, 0)
	if statErr == nil {
		if outstanding > available || d.minFree > available-outstanding {
			statErr = ErrDiskReserve
		}
	}
	if statErr != nil {
		d.rejects++
		d.lastError = statErr.Error()
	}
	d.mu.Unlock()
	if statErr != nil {
		return bodyStorageError("verify spool reserve after preallocation", statErr)
	}
	return nil
}

// CommitSize drops unused chunk slack after an unknown-length capture is sealed.
func (r *DiskReservation) CommitSize(actual int64) {
	if r == nil || r.owner == nil {
		return
	}
	if actual < 0 {
		actual = 0
	}
	d := r.owner
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.closed || actual >= r.capacity {
		return
	}
	d.reserved -= r.capacity - actual
	r.capacity = actual
	if r.allocated > actual {
		d.allocated -= r.allocated - actual
		r.allocated = actual
	}
}

func (r *DiskReservation) Release() {
	if r == nil || r.owner == nil {
		return
	}
	d := r.owner
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	d.reserved -= r.capacity
	d.allocated -= r.allocated
	if d.reserved < 0 {
		d.reserved = 0
	}
	if d.allocated < 0 {
		d.allocated = 0
	}
	if d.active > 0 {
		d.active--
	}
	d.releases++
}

func roundDiskReservation(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value > int64(^uint64(0)>>1)-(DiskReservationChunkBytes-1) {
		return value
	}
	return ((value + DiskReservationChunkBytes - 1) / DiskReservationChunkBytes) * DiskReservationChunkBytes
}

func diskFilesystemStats(dir string) (total, available int64, err error) {
	if dir == "" {
		dir = os.TempDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return 0, 0, err
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(abs, &stat); err != nil {
		return 0, 0, err
	}
	blockSize := int64(stat.Bsize)
	return int64(stat.Blocks) * blockSize, int64(stat.Bavail) * blockSize, nil
}
