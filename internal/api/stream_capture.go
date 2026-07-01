package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/leakfilter"
)

var errSSECaptureLimitExceeded = errors.New("sse capture limit exceeded")

type sseCaptureOptions struct {
	MemoryLimit int64
	DiskLimit   int64
	TempDir     string
}

type capturedSSEStream struct {
	buf         bytes.Buffer
	file        *os.File
	size        int64
	diskSize    int64
	memoryLimit int64
	diskLimit   int64
	tempDir     string
}

func captureCodexSSEForFailover(body io.Reader, opts sseCaptureOptions) (*capturedSSEStream, bool, error) {
	return captureSSEForFailoverWithOptions(body, leakfilter.IsRetryableCodexFailureFrame, opts)
}

func captureClaudeSSEForFailover(body io.Reader, opts sseCaptureOptions) (*capturedSSEStream, bool, error) {
	return captureSSEForFailoverWithOptions(body, leakfilter.IsRetryableClaudeErrorFrame, opts)
}

func captureSSEForFailoverWithOptions(body io.Reader, retryableFrame func([]byte) bool, opts sseCaptureOptions) (*capturedSSEStream, bool, error) {
	captured := newCapturedSSEStream(opts)
	frameBuf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(tmp)
		if n > 0 {
			chunk := tmp[:n]
			if _, err := captured.Write(chunk); err != nil {
				captured.Close()
				return nil, false, err
			}
			frameBuf = append(frameBuf, chunk...)
			for {
				idx := bytes.Index(frameBuf, []byte("\n\n"))
				if idx < 0 {
					break
				}
				frame := frameBuf[:idx+2]
				if retryableFrame(frame) {
					return captured, true, nil
				}
				frameBuf = frameBuf[idx+2:]
			}
			if len(frameBuf) > streamLedgerMaxPartialFrame {
				frameBuf = frameBuf[len(frameBuf)-streamLedgerMaxPartialFrame:]
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return captured, false, nil
			}
			return captured, false, readErr
		}
	}
}

func newCapturedSSEStream(opts sseCaptureOptions) *capturedSSEStream {
	if opts.MemoryLimit <= 0 {
		opts.MemoryLimit = config.DefaultStreamFailoverHoldMemoryBytes
	}
	if opts.DiskLimit < 0 {
		opts.DiskLimit = 0
	}
	return &capturedSSEStream{
		memoryLimit: opts.MemoryLimit,
		diskLimit:   opts.DiskLimit,
		tempDir:     opts.TempDir,
	}
}

func (s *Server) streamFailoverCaptureOptions(ctx context.Context) sseCaptureOptions {
	mem := s.settingInt64(ctx, "stream_failover_hold_memory_bytes", s.cfg.StreamFailoverHoldMemoryBytes)
	if mem <= 0 {
		mem = config.DefaultStreamFailoverHoldMemoryBytes
	}
	disk := s.settingInt64(ctx, "stream_failover_hold_disk_bytes", s.cfg.StreamFailoverHoldDiskBytes)
	if disk < 0 {
		disk = 0
	}
	return sseCaptureOptions{
		MemoryLimit: mem,
		DiskLimit:   disk,
		TempDir:     s.settingString(ctx, "stream_failover_hold_temp_dir", s.cfg.StreamFailoverHoldTempDir),
	}
}

func (c *capturedSSEStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.file == nil && c.size+int64(len(p)) <= c.memoryLimit {
		n, err := c.buf.Write(p)
		c.size += int64(n)
		return n, err
	}
	if c.diskLimit <= 0 {
		return 0, errSSECaptureLimitExceeded
	}
	if c.file == nil {
		if int64(c.buf.Len()) > c.diskLimit {
			return 0, errSSECaptureLimitExceeded
		}
		f, err := os.CreateTemp(c.tempDir, "codex-account-pool-sse-*")
		if err != nil {
			return 0, err
		}
		if _, err := f.Write(c.buf.Bytes()); err != nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return 0, err
		}
		c.diskSize = int64(c.buf.Len())
		c.buf.Reset()
		c.file = f
	}
	if c.diskSize+int64(len(p)) > c.diskLimit {
		return 0, errSSECaptureLimitExceeded
	}
	n, err := c.file.Write(p)
	c.size += int64(n)
	c.diskSize += int64(n)
	return n, err
}

func (c *capturedSSEStream) Reader() (io.Reader, error) {
	if c == nil {
		return bytes.NewReader(nil), nil
	}
	if c.file != nil {
		if _, err := c.file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return c.file, nil
	}
	return bytes.NewReader(c.buf.Bytes()), nil
}

func (c *capturedSSEStream) Head(limit int) []byte {
	if c == nil || limit <= 0 {
		return nil
	}
	if c.file == nil {
		b := c.buf.Bytes()
		if len(b) > limit {
			b = b[:limit]
		}
		return append([]byte(nil), b...)
	}
	cur, _ := c.file.Seek(0, io.SeekCurrent)
	defer func() { _, _ = c.file.Seek(cur, io.SeekStart) }()
	if _, err := c.file.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	buf := make([]byte, limit)
	n, _ := io.ReadFull(c.file, buf)
	return buf[:n]
}

func (c *capturedSSEStream) Close() {
	if c == nil || c.file == nil {
		return
	}
	name := c.file.Name()
	_ = c.file.Close()
	_ = os.Remove(name)
	c.file = nil
}
