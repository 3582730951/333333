package bodysource

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"testing"
)

func TestCaptureKnownLengthReservesActualSizeNotMaximum(t *testing.T) {
	dir := t.TempDir()
	_, available, err := diskFilesystemStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	const (
		actual   = int64(64 << 10)
		maximum  = int64(1 << 30)
		headroom = int64(512 << 20)
	)
	if available <= actual+headroom {
		t.Skipf("filesystem has only %d available bytes", available)
	}
	minimumFree := available - actual - headroom
	if available <= minimumFree+actual || available >= minimumFree+maximum {
		t.Fatalf("invalid fixture: available=%d reserve=%d actual=%d maximum=%d", available, minimumFree, actual, maximum)
	}
	// This is the obsolete admission decision: charging MaxBytes would reject
	// the request even though the complete known body fits above the reserve.
	if err := ensureDiskReserve(dir, minimumFree, maximum); !errors.Is(err, ErrDiskReserve) {
		t.Fatalf("maximum-sized reservation unexpectedly admitted: %v", err)
	}

	reserver := NewDiskReserver(dir, minimumFree, maximum)
	budget := NewBudget(0, maximum)
	budget.SetDiskReserver(reserver)
	payload := bytes.Repeat([]byte("k"), int(actual))
	source, err := Capture(context.Background(), bytes.NewReader(payload), CaptureOptions{
		MaxBytes: maximum, MemoryThreshold: 0, ExpectedBytes: actual, TempDir: dir, Budget: budget,
	})
	if err != nil {
		t.Fatalf("capture using actual Content-Length: %v", err)
	}
	if got, readErr := ReadAll(source); readErr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("replay len=%d err=%v", len(got), readErr)
	}
	if snapshot := reserver.Snapshot(); snapshot.ReservedBytes != actual || snapshot.ActiveReservations != 1 {
		t.Fatalf("reservation charged something other than actual bytes: %+v", snapshot)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoDiskReservationLeak(t, reserver)
}

func TestDiskReserverConcurrentReservationsAreAtomic(t *testing.T) {
	const (
		workers = 64
		size    = int64(4096)
	)
	reserver := NewDiskReserver(t.TempDir(), 0, workers*size)
	start := make(chan struct{})
	release := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reservation, err := reserver.NewReservation(size, true)
			results <- err
			if err != nil {
				return
			}
			<-release
			reservation.Release()
			reservation.Release()
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	if snapshot := reserver.Snapshot(); snapshot.ReservedBytes != workers*size || snapshot.ActiveReservations != workers {
		t.Fatalf("reservations were not atomically accounted: %+v", snapshot)
	}
	if reservation, err := reserver.NewReservation(1, true); !errors.Is(err, ErrSpoolBudget) || reservation != nil {
		t.Fatalf("over-limit reservation=%v err=%v", reservation, err)
	}
	close(release)
	wg.Wait()
	assertNoDiskReservationLeak(t, reserver)
	if snapshot := reserver.Snapshot(); snapshot.Releases != workers {
		t.Fatalf("idempotent releases counted more than once: %+v", snapshot)
	}
}

func TestCaptureUnknownLengthGrowsReservationInEightMiBChunks(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("u"), int(DiskReservationChunkBytes+1))
	reserver := NewDiskReserver(dir, 0, 3*DiskReservationChunkBytes)
	budget := NewBudget(0, int64(len(payload)))
	budget.SetDiskReserver(reserver)
	source, err := Capture(context.Background(), bytes.NewReader(payload), CaptureOptions{
		MaxBytes: int64(len(payload)), MemoryThreshold: 0, TempDir: dir, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := reserver.Snapshot(); snapshot.GrowthOperations != 1 || snapshot.ReservedBytes != int64(len(payload)) || snapshot.ActiveReservations != 1 {
		t.Fatalf("unknown-length reservation did not grow and commit as expected: %+v", snapshot)
	}
	if got, readErr := ReadAll(source); readErr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("replay len=%d err=%v", len(got), readErr)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoDiskReservationLeak(t, reserver)
	assertDirectoryEmpty(t, dir)
}

func TestCaptureCancellationReleasesDiskReservationAndFile(t *testing.T) {
	dir := t.TempDir()
	reserver := NewDiskReserver(dir, 0, 2*DiskReservationChunkBytes)
	budget := NewBudget(0, 2*DiskReservationChunkBytes)
	budget.SetDiskReserver(reserver)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterRead{reader: bytes.NewReader(bytes.Repeat([]byte("c"), DefaultChunkSize*2)), cancel: cancel}
	_, err := Capture(ctx, reader, CaptureOptions{
		MaxBytes: 2 * DefaultChunkSize, MemoryThreshold: 0, TempDir: dir, Budget: budget,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("capture error=%v", err)
	}
	assertNoDiskReservationLeak(t, reserver)
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("cancellation leaked budget: %+v", snapshot)
	}
	assertDirectoryEmpty(t, dir)
}

func TestSpoolBufferWriteFailureAndDoubleCloseReleaseEverything(t *testing.T) {
	dir := t.TempDir()
	reserver := NewDiskReserver(dir, 0, 2*DiskReservationChunkBytes)
	budget := NewBudget(0, 2*DiskReservationChunkBytes)
	budget.SetDiskReserver(reserver)
	buffer, err := NewSpoolBuffer(context.Background(), CaptureOptions{
		MaxBytes: DiskReservationChunkBytes, MemoryThreshold: 0, TempDir: dir, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = buffer.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	// Force the actual file write path to fail after both admission layers have
	// charged the second write. Close must still release every charge and remove
	// the temporary file.
	if err = buffer.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = buffer.Write([]byte("b")); err == nil {
		t.Fatal("expected write to a closed spool file to fail")
	}
	_ = buffer.Close() // closing the deliberately closed descriptor reports an error
	if err = buffer.Close(); err != nil {
		t.Fatalf("second close was not idempotent: %v", err)
	}
	assertNoDiskReservationLeak(t, reserver)
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("write failure leaked budget: %+v", snapshot)
	}
	assertDirectoryEmpty(t, dir)
}

func TestSplitBudgetGuaranteesMinimumAndAllowsBorrowing(t *testing.T) {
	request, response := NewSplitBudget(100, 100, .25)
	if !request.reserveMemory(75) {
		t.Fatal("request did not borrow the shared 50-byte portion")
	}
	if request.reserveMemory(1) {
		t.Fatal("request consumed the response minimum")
	}
	if !response.reserveMemory(25) {
		t.Fatal("response minimum was not protected")
	}
	if response.reserveMemory(1) {
		t.Fatal("aggregate memory ceiling was exceeded")
	}
	if snapshot := request.Snapshot(); snapshot.MemoryUsed != 100 || snapshot.RequestMemoryMinimum != 25 || snapshot.ResponseMemoryMinimum != 25 || snapshot.RequestMemoryUsed != 75 || snapshot.ResponseMemoryUsed != 25 {
		t.Fatalf("split accounting mismatch: %+v", snapshot)
	}
	request.releaseMemory(75)
	response.releaseMemory(25)

	if !response.reserveMemory(75) {
		t.Fatal("response did not borrow the shared portion after release")
	}
	if request.reserveMemory(26) {
		t.Fatal("request exceeded its protected minimum while response borrowed")
	}
	if !request.reserveMemory(25) {
		t.Fatal("request minimum was not available while response borrowed")
	}
	response.releaseMemory(75)
	request.releaseMemory(25)
	if snapshot := response.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.RequestMemoryUsed != 0 || snapshot.ResponseMemoryUsed != 0 {
		t.Fatalf("split memory leaked: %+v", snapshot)
	}

	if !request.reserveSpool(60) || !response.reserveSpool(40) || request.reserveSpool(1) {
		t.Fatal("split views do not share one spool ceiling")
	}
	request.releaseSpool(60)
	response.releaseSpool(40)
	if snapshot := request.Snapshot(); snapshot.SpoolUsed != 0 {
		t.Fatalf("split spool leaked: %+v", snapshot)
	}
}

func TestRoundDiskReservationOverflowBoundary(t *testing.T) {
	boundary := int64(math.MaxInt64) - (DiskReservationChunkBytes - 1)
	for _, test := range []struct {
		value int64
		want  int64
	}{
		{0, 0},
		{1, DiskReservationChunkBytes},
		{DiskReservationChunkBytes, DiskReservationChunkBytes},
		{DiskReservationChunkBytes + 1, 2 * DiskReservationChunkBytes},
		{boundary, boundary},
		{boundary + 1, boundary + 1},
		{math.MaxInt64, math.MaxInt64},
	} {
		if got := roundDiskReservation(test.value); got != test.want || got < test.value {
			t.Errorf("roundDiskReservation(%d)=%d want=%d", test.value, got, test.want)
		}
	}

	budget := NewBudget(0, math.MaxInt64)
	if !budget.reserveSpool(math.MaxInt64) || budget.reserveSpool(1) {
		t.Fatal("spool reservation overflowed the configured limit")
	}
	budget.releaseSpool(math.MaxInt64)
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.once.Do(r.cancel)
	}
	return n, err
}

func assertNoDiskReservationLeak(t *testing.T, reserver *DiskReserver) {
	t.Helper()
	snapshot := reserver.Snapshot()
	if snapshot.ReservedBytes != 0 || snapshot.AllocatedBytes != 0 || snapshot.ActiveReservations != 0 {
		t.Fatalf("disk reservation leaked: %+v", snapshot)
	}
}

func assertDirectoryEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files remain: entries=%v err=%v", entries, err)
	}
}
