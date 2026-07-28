package bodysource

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
)

// SpoolBuffer is a replayable writer that spills from bounded memory to one file.
type SpoolBuffer struct {
	ctx            context.Context
	options        CaptureOptions
	memory         bytes.Buffer
	file           *os.File
	size           int64
	memoryReserved int64
	spoolReserved  int64
	sealed         bool
	closed         bool
	err            error
	viewOnce       sync.Once
	view           []byte
	viewErr        error
	mu             sync.Mutex
}

func NewSpoolBuffer(ctx context.Context, options CaptureOptions) (*SpoolBuffer, error) {
	if options.MaxBytes <= 0 {
		return nil, ErrBodyTooLarge
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if options.MemoryThreshold < 0 {
		options.MemoryThreshold = 0
	}
	if options.TempFileNamePrefix == "" {
		options.TempFileNamePrefix = "codex-pool-buffer-*"
	}
	return &SpoolBuffer{ctx: ctx, options: options}, nil
}

func (b *SpoolBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.sealed {
		return 0, ErrClosed
	}
	if b.err != nil {
		return 0, b.err
	}
	if err := b.ctx.Err(); err != nil {
		b.err = err
		return 0, err
	}
	if int64(len(p)) > b.options.MaxBytes-b.size {
		b.err = ErrBodyTooLarge
		return 0, b.err
	}
	if b.file == nil && b.size+int64(len(p)) <= b.options.MemoryThreshold && b.options.Budget.reserveMemory(int64(len(p))) {
		b.memoryReserved += int64(len(p))
		n, err := b.memory.Write(p)
		b.size += int64(n)
		if err != nil {
			b.err = err
		}
		return n, err
	}
	if b.file == nil {
		if err := b.spillLocked(); err != nil {
			b.err = err
			return 0, err
		}
	}
	if !b.options.Budget.reserveSpool(int64(len(p))) {
		b.err = ErrSpoolBudget
		return 0, b.err
	}
	b.spoolReserved += int64(len(p))
	n, err := writeFull(b.file, p)
	b.size += int64(n)
	if err != nil {
		b.err = err
	}
	return n, err
}

func (b *SpoolBuffer) spillLocked() error {
	if err := ensureDiskReserve(b.options.TempDir, b.options.MinDiskFreeBytes, b.options.MaxBytes); err != nil {
		return err
	}
	if !b.options.Budget.reserveSpool(b.size) {
		return ErrSpoolBudget
	}
	file, err := os.CreateTemp(b.options.TempDir, b.options.TempFileNamePrefix)
	if err != nil {
		b.options.Budget.releaseSpool(b.size)
		return err
	}
	if _, err = writeFull(file, b.memory.Bytes()); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		b.options.Budget.releaseSpool(b.size)
		return err
	}
	b.file = file
	b.spoolReserved = b.size
	b.memory.Reset()
	b.options.Budget.releaseMemory(b.memoryReserved)
	b.memoryReserved = 0
	return nil
}

func writeFull(w io.Writer, p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n, err := w.Write(p)
		written += n
		p = p[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (b *SpoolBuffer) Size() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

func (b *SpoolBuffer) Open() (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	if b.err != nil {
		return nil, b.err
	}
	b.sealed = true
	if b.file != nil {
		if err := b.file.Sync(); err != nil {
			return nil, err
		}
		return os.Open(b.file.Name())
	}
	return io.NopCloser(bytes.NewReader(b.memory.Bytes())), nil
}

func (b *SpoolBuffer) byteView() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.err != nil {
		return nil, false
	}
	b.sealed = true
	if b.file == nil {
		return b.memory.Bytes(), true
	}
	b.viewOnce.Do(func() {
		if err := b.file.Sync(); err != nil {
			b.viewErr = err
			return
		}
		b.view, b.viewErr = mapFileReadOnly(b.file.Name(), b.size)
	})
	return b.view, b.viewErr == nil
}

func (b *SpoolBuffer) Spilled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.file != nil
}

func (b *SpoolBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	var closeErr error
	if len(b.view) > 0 {
		closeErr = unmapFile(b.view)
		b.view = nil
	}
	if b.file != nil {
		path := b.file.Name()
		closeErr = errors.Join(closeErr, b.file.Close())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	b.options.Budget.releaseMemory(b.memoryReserved)
	b.options.Budget.releaseSpool(b.spoolReserved)
	b.memory.Reset()
	return closeErr
}
