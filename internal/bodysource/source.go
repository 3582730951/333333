// Package bodysource provides bounded, replayable request bodies.
package bodysource

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

var ErrClosed = errors.New("body source is closed")

// BodySource can be opened independently for every upstream attempt.
type BodySource interface {
	Size() int64
	Open() (io.ReadCloser, error)
	Close() error
}

// Bytes references b without copying it. The caller must not mutate b while the source is in use.
func Bytes(b []byte) BodySource { return &memorySource{chunks: [][]byte{b}, size: int64(len(b))} }

// Empty returns a reusable empty body.
func Empty() BodySource { return Bytes(nil) }

// ByteView returns an immutable contiguous view when the source can expose one
// without copying. A spool source uses a read-only file mapping on supported hosts.
// The view is valid only until source.Close; callers must not modify it.
func ByteView(source BodySource) ([]byte, bool) {
	view, ok := source.(interface{ byteView() ([]byte, bool) })
	if !ok {
		return nil, false
	}
	return view.byteView()
}

type memorySource struct {
	chunks   [][]byte
	size     int64
	budget   *Budget
	reserved int64
	once     sync.Once
	closed   bool
	mu       sync.RWMutex
}

func (s *memorySource) Size() int64 { return s.size }

func (s *memorySource) byteView() ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || len(s.chunks) != 1 {
		return nil, false
	}
	return s.chunks[0], true
}

func (s *memorySource) Open() (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	s.budget.recordOpen()
	return &chunkReader{chunks: s.chunks}, nil
}

func (s *memorySource) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.chunks = nil
		s.mu.Unlock()
		if s.budget != nil {
			s.budget.releaseMemory(s.reserved)
			s.budget.recordClose()
		}
	})
	return nil
}

type chunkReader struct {
	chunks [][]byte
	chunk  int
	offset int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for r.chunk < len(r.chunks) {
		n := copy(p, r.chunks[r.chunk][r.offset:])
		r.offset += n
		if r.offset == len(r.chunks[r.chunk]) {
			r.chunk++
			r.offset = 0
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, io.EOF
}

func (r *chunkReader) Close() error { return nil }

type fileSource struct {
	path            string
	size            int64
	remove          bool
	budget          *Budget
	reserved        int64
	diskReservation *DiskReservation
	once            sync.Once
	mu              sync.RWMutex
	closed          bool
	err             error
	viewOnce        sync.Once
	view            []byte
	viewErr         error
}

func (s *fileSource) Size() int64 { return s.size }

func (s *fileSource) byteView() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.size == 0 {
		return nil, false
	}
	s.viewOnce.Do(func() {
		s.view, s.viewErr = mapFileReadOnly(s.path, s.size)
	})
	return s.view, s.viewErr == nil
}

func (s *fileSource) Open() (io.ReadCloser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	s.budget.recordOpen()
	return os.Open(s.path)
}

func (s *fileSource) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		if len(s.view) > 0 {
			s.err = unmapFile(s.view)
			s.view = nil
		}
		s.mu.Unlock()
		if s.remove {
			removeErr := os.Remove(s.path)
			if errors.Is(removeErr, os.ErrNotExist) {
				removeErr = nil
			}
			s.err = errors.Join(s.err, removeErr)
		}
		if s.budget != nil {
			s.budget.releaseSpool(s.reserved)
			s.budget.recordClose()
		}
		if s.diskReservation != nil {
			s.diskReservation.Release()
			s.diskReservation = nil
		}
	})
	return s.err
}

// Slice returns a non-owning view of source. Closing the view does not close source.
func Slice(source BodySource, offset, size int64) (BodySource, error) {
	if source == nil || offset < 0 || size < 0 || offset > source.Size() || size > source.Size()-offset {
		return nil, fmt.Errorf("invalid body slice offset=%d size=%d", offset, size)
	}
	return &sliceSource{source: source, offset: offset, size: size}, nil
}

type sliceSource struct {
	source BodySource
	offset int64
	size   int64
}

func (s *sliceSource) Size() int64 { return s.size }

func (s *sliceSource) Open() (io.ReadCloser, error) {
	r, err := s.source.Open()
	if err != nil {
		return nil, err
	}
	if seeker, ok := r.(io.Seeker); ok {
		_, err = seeker.Seek(s.offset, io.SeekStart)
	} else {
		_, err = io.CopyN(io.Discard, r, s.offset)
	}
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return &limitedReadCloser{Reader: io.LimitReader(r, s.size), closer: r}, nil
}

func (s *sliceSource) Close() error { return nil }

type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *limitedReadCloser) Close() error { return r.closer.Close() }

// ReadAll opens source and returns its complete contents, rejecting impossible sizes first.
func ReadAll(source BodySource) ([]byte, error) {
	if source == nil || source.Size() == 0 {
		return nil, nil
	}
	if source.Size() > int64(int(^uint(0)>>1)) {
		return nil, errors.New("body is too large for memory")
	}
	r, err := source.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out bytes.Buffer
	out.Grow(int(source.Size()))
	if _, err = io.Copy(&out, r); err != nil {
		return nil, err
	}
	if int64(out.Len()) != source.Size() {
		return nil, fmt.Errorf("body size changed: got %d want %d", out.Len(), source.Size())
	}
	return out.Bytes(), nil
}
