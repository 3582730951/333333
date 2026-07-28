package bodysource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
)

type Patch struct {
	Offset int64
	Delete int64
	Insert BodySource
}

type JSONFieldPatch struct {
	Name   string
	Value  []byte
	Delete bool
}

// PatchTopLevel applies ordered top-level JSON field changes without materializing source.
func PatchTopLevel(source BodySource, meta BodyMeta, fields []JSONFieldPatch) (BodySource, error) {
	if source == nil || meta.ObjectEnd < 0 || meta.ObjectEnd > source.Size() {
		return nil, errors.New("invalid JSON patch source")
	}
	deletes := make([]Span, 0, len(fields))
	sets := make([]Patch, 0, len(fields)+1)
	inserted := make([]JSONFieldPatch, 0, len(fields))
	deleted := make(map[string]bool)
	seen := make(map[string]bool)
	for _, field := range fields {
		if field.Name == "" || seen[field.Name] {
			return nil, fmt.Errorf("duplicate JSON field patch %q", field.Name)
		}
		seen[field.Name] = true
		value, exists := meta.Fields[field.Name]
		if field.Delete {
			if member, ok := meta.Members[field.Name]; ok {
				deletes = append(deletes, member)
				deleted[field.Name] = true
			}
			continue
		}
		if !json.Valid(field.Value) {
			return nil, fmt.Errorf("invalid JSON value for field %q", field.Name)
		}
		if exists {
			sets = append(sets, Patch{Offset: value.Offset, Delete: value.Length, Insert: Bytes(field.Value)})
		} else {
			inserted = append(inserted, field)
		}
	}
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].Offset < deletes[j].Offset })
	merged := deletes[:0]
	for _, span := range deletes {
		if len(merged) == 0 || span.Offset > merged[len(merged)-1].Offset+merged[len(merged)-1].Length {
			merged = append(merged, span)
			continue
		}
		end := span.Offset + span.Length
		if currentEnd := merged[len(merged)-1].Offset + merged[len(merged)-1].Length; end > currentEnd {
			merged[len(merged)-1].Length = end - merged[len(merged)-1].Offset
		}
	}
	for _, span := range merged {
		sets = append(sets, Patch{Offset: span.Offset, Delete: span.Length})
	}
	if len(inserted) > 0 {
		var payload bytes.Buffer
		if meta.MemberCount-len(deleted) > 0 {
			payload.WriteByte(',')
		}
		for i, field := range inserted {
			if i > 0 {
				payload.WriteByte(',')
			}
			payload.WriteString(strconv.Quote(field.Name))
			payload.WriteByte(':')
			payload.Write(field.Value)
		}
		sets = append(sets, Patch{Offset: meta.ObjectEnd, Insert: Bytes(payload.Bytes())})
	}
	sort.SliceStable(sets, func(i, j int) bool { return sets[i].Offset < sets[j].Offset })
	return Patched(source, sets)
}

// Patched creates an owning composite source. Patches must be ordered and non-overlapping.
func Patched(base BodySource, patches []Patch) (BodySource, error) {
	if base == nil {
		return nil, errors.New("nil patch base")
	}
	copyPatches := append([]Patch(nil), patches...)
	var cursor, size int64
	for i, patch := range copyPatches {
		if patch.Offset < cursor || patch.Offset < 0 || patch.Delete < 0 || patch.Offset > base.Size() || patch.Delete > base.Size()-patch.Offset {
			return nil, fmt.Errorf("invalid body patch %d", i)
		}
		cursor = patch.Offset + patch.Delete
		size -= patch.Delete
		if patch.Insert != nil {
			size += patch.Insert.Size()
		}
	}
	return &patchedSource{base: base, patches: copyPatches, size: base.Size() + size}, nil
}

type patchedSource struct {
	base    BodySource
	patches []Patch
	size    int64
	once    sync.Once
	err     error
}

func (s *patchedSource) Size() int64 { return s.size }

func (s *patchedSource) Open() (io.ReadCloser, error) {
	base, err := s.base.Open()
	if err != nil {
		return nil, err
	}
	return &patchReader{base: base, patches: s.patches}, nil
}

func (s *patchedSource) Close() error {
	s.once.Do(func() {
		s.err = s.base.Close()
		for _, patch := range s.patches {
			if patch.Insert != nil {
				s.err = errors.Join(s.err, patch.Insert.Close())
			}
		}
	})
	return s.err
}

type patchReader struct {
	base       io.ReadCloser
	patches    []Patch
	patch      int
	baseOffset int64
	segment    io.ReadCloser
	remaining  int64
	done       bool
}

func (r *patchReader) Read(p []byte) (int, error) {
	for {
		if r.done {
			return 0, io.EOF
		}
		if r.segment != nil {
			n, err := r.segment.Read(p)
			if err == io.EOF {
				_ = r.segment.Close()
				r.segment = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		if r.remaining > 0 {
			limit := int64(len(p))
			if limit > r.remaining {
				limit = r.remaining
			}
			n, err := r.base.Read(p[:limit])
			r.baseOffset += int64(n)
			r.remaining -= int64(n)
			if err == io.EOF && r.remaining > 0 {
				return n, io.ErrUnexpectedEOF
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		if r.patch >= len(r.patches) {
			n, err := r.base.Read(p)
			r.baseOffset += int64(n)
			if err == io.EOF {
				r.done = true
			}
			return n, err
		}
		patch := r.patches[r.patch]
		if r.baseOffset < patch.Offset {
			r.remaining = patch.Offset - r.baseOffset
			continue
		}
		if patch.Delete > 0 {
			if _, err := io.CopyN(io.Discard, r.base, patch.Delete); err != nil {
				return 0, err
			}
			r.baseOffset += patch.Delete
		}
		r.patch++
		if patch.Insert != nil && patch.Insert.Size() > 0 {
			var err error
			r.segment, err = patch.Insert.Open()
			if err != nil {
				return 0, err
			}
		}
	}
}

func (r *patchReader) Close() error {
	var err error
	if r.segment != nil {
		err = r.segment.Close()
		r.segment = nil
	}
	return errors.Join(err, r.base.Close())
}
