package api

import (
	"context"
	"io"

	"codex-account-pool/internal/bodysource"
)

const (
	defaultStreamAccumulatorMemoryBytes = int64(8 << 20)
	defaultStreamAccumulatorMaxBytes    = int64(1 << 30)
)

// streamAccumulator keeps growing protocol values off the heap once they cross the
// request memory threshold. It is sealed when Bytes or String is called.
type streamAccumulator struct {
	buffer *bodysource.SpoolBuffer
	err    error
}

func newStreamAccumulator(ctx context.Context, options bodysource.CaptureOptions, prefix string) *streamAccumulator {
	options = normalizeStreamAccumulatorOptions(options, prefix)
	buffer, err := bodysource.NewSpoolBuffer(ctx, options)
	return &streamAccumulator{buffer: buffer, err: err}
}

func normalizeStreamAccumulatorOptions(options bodysource.CaptureOptions, prefix string) bodysource.CaptureOptions {
	defaulted := options.MaxBytes <= 0
	if options.MaxBytes <= 0 {
		options.MaxBytes = defaultStreamAccumulatorMaxBytes
	}
	if options.MemoryThreshold < 0 {
		options.MemoryThreshold = 0
	} else if defaulted && options.MemoryThreshold == 0 {
		options.MemoryThreshold = defaultStreamAccumulatorMemoryBytes
	}
	if prefix != "" {
		options.TempFileNamePrefix = prefix
	}
	return options
}

func (a *streamAccumulator) WriteString(value string) error {
	if a == nil {
		return nil
	}
	if a.err != nil {
		return a.err
	}
	_, a.err = io.WriteString(a.buffer, value)
	return a.err
}

func (a *streamAccumulator) Bytes() ([]byte, error) {
	if a == nil {
		return nil, nil
	}
	if a.err != nil {
		return nil, a.err
	}
	if view, ok := bodysource.ByteView(a.buffer); ok {
		return view, nil
	}
	return bodysource.ReadAll(a.buffer)
}

func (a *streamAccumulator) String() (string, error) {
	raw, err := a.Bytes()
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (a *streamAccumulator) Size() int64 {
	if a == nil || a.buffer == nil {
		return 0
	}
	return a.buffer.Size()
}

func (a *streamAccumulator) Close() error {
	if a == nil || a.buffer == nil {
		return nil
	}
	return a.buffer.Close()
}

// forEachSSEFrameWithOptions invokes fn while the current frame's bounded memory or
// read-only spool view is valid. Callers must not retain frame after fn returns.
func forEachSSEFrameWithOptions(ctx context.Context, src io.Reader, options bodysource.CaptureOptions, fn func(frame []byte) error) error {
	options = normalizeStreamAccumulatorOptions(options, "codex-pool-sse-frame-*")
	var frame *bodysource.SpoolBuffer
	lineNonCR := false
	closeFrame := func() {
		if frame != nil {
			_ = frame.Close()
			frame = nil
		}
	}
	defer closeFrame()
	write := func(payload []byte) error {
		if len(payload) == 0 {
			return nil
		}
		if frame == nil {
			var err error
			frame, err = bodysource.NewSpoolBuffer(ctx, options)
			if err != nil {
				return err
			}
		}
		_, err := frame.Write(payload)
		return err
	}
	flush := func() error {
		if frame == nil || frame.Size() == 0 {
			closeFrame()
			return nil
		}
		current := frame
		frame = nil
		defer current.Close()
		if view, ok := bodysource.ByteView(current); ok {
			return fn(view)
		}
		raw, err := bodysource.ReadAll(current)
		if err != nil {
			return err
		}
		return fn(raw)
	}

	chunk := make([]byte, 64<<10)
	for {
		n, readErr := src.Read(chunk)
		if n > 0 {
			start := 0
			for index, value := range chunk[:n] {
				if value != '\n' {
					if value != '\r' {
						lineNonCR = true
					}
					continue
				}
				boundary := !lineNonCR
				lineNonCR = false
				if !boundary {
					continue
				}
				if err := write(chunk[start : index+1]); err != nil {
					return err
				}
				start = index + 1
				if err := flush(); err != nil {
					return err
				}
			}
			if err := write(chunk[start:n]); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return flush()
			}
			return readErr
		}
	}
}
