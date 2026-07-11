package kiro

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
)

const DefaultMaxEventStreamFrame = 16 << 20

type Header struct {
	Name  string
	Type  byte
	Value interface{}
}
type Frame struct {
	Headers map[string]Header
	Payload []byte
}

func (f Frame) HeaderString(name string) string {
	if h, ok := f.Headers[name]; ok {
		if v, ok := h.Value.(string); ok {
			return v
		}
	}
	return ""
}

type Decoder struct {
	buf          []byte
	MaxFrameSize int
}

func NewDecoder() *Decoder { return &Decoder{MaxFrameSize: DefaultMaxEventStreamFrame} }

// Finish validates that the transport ended exactly between frames. A partial final
// frame is a retryable upstream truncation, not a successful empty event.
func (d *Decoder) Finish() error {
	if len(d.buf) != 0 {
		return fmt.Errorf("eventstream truncated frame: %d buffered bytes", len(d.buf))
	}
	return nil
}

// Feed accepts arbitrary transport fragmentation and returns every complete AWS
// EventStream frame currently buffered. Prelude and message CRCs are both verified.
func (d *Decoder) Feed(chunk []byte) ([]Frame, error) {
	d.buf = append(d.buf, chunk...)
	var out []Frame
	for {
		if len(d.buf) < 12 {
			return out, nil
		}
		total := int(binary.BigEndian.Uint32(d.buf[:4]))
		headLen := int(binary.BigEndian.Uint32(d.buf[4:8]))
		if total < 16 || total > d.maxFrame() {
			return out, fmt.Errorf("eventstream invalid frame length %d", total)
		}
		if headLen < 0 || headLen > total-16 {
			return out, fmt.Errorf("eventstream invalid headers length %d", headLen)
		}
		if crc32.ChecksumIEEE(d.buf[:8]) != binary.BigEndian.Uint32(d.buf[8:12]) {
			return out, errors.New("eventstream prelude crc mismatch")
		}
		if len(d.buf) < total {
			return out, nil
		}
		frameBytes := d.buf[:total]
		if crc32.ChecksumIEEE(frameBytes[:total-4]) != binary.BigEndian.Uint32(frameBytes[total-4:]) {
			return out, errors.New("eventstream message crc mismatch")
		}
		headers, err := decodeHeaders(frameBytes[12 : 12+headLen])
		if err != nil {
			return out, err
		}
		payload := append([]byte(nil), frameBytes[12+headLen:total-4]...)
		out = append(out, Frame{Headers: headers, Payload: payload})
		d.buf = d.buf[total:]
	}
}

func (d *Decoder) maxFrame() int {
	if d.MaxFrameSize <= 0 {
		return DefaultMaxEventStreamFrame
	}
	return d.MaxFrameSize
}

func decodeHeaders(raw []byte) (map[string]Header, error) {
	out := map[string]Header{}
	for i := 0; i < len(raw); {
		nameLen := int(raw[i])
		i++
		if nameLen == 0 || i+nameLen+1 > len(raw) {
			return nil, errors.New("eventstream malformed header name")
		}
		name := string(raw[i : i+nameLen])
		i += nameLen
		typ := raw[i]
		i++
		var value interface{}
		need := func(n int) ([]byte, error) {
			if n < 0 || i+n > len(raw) {
				return nil, errors.New("eventstream truncated header")
			}
			b := raw[i : i+n]
			i += n
			return b, nil
		}
		switch typ {
		case 0:
			value = true
		case 1:
			value = false
		case 2:
			b, e := need(1)
			if e != nil {
				return nil, e
			}
			value = int8(b[0])
		case 3:
			b, e := need(2)
			if e != nil {
				return nil, e
			}
			value = int16(binary.BigEndian.Uint16(b))
		case 4:
			b, e := need(4)
			if e != nil {
				return nil, e
			}
			value = int32(binary.BigEndian.Uint32(b))
		case 5:
			b, e := need(8)
			if e != nil {
				return nil, e
			}
			value = int64(binary.BigEndian.Uint64(b))
		case 6, 7:
			b, e := need(2)
			if e != nil {
				return nil, e
			}
			n := int(binary.BigEndian.Uint16(b))
			data, e := need(n)
			if e != nil {
				return nil, e
			}
			if typ == 7 {
				value = string(data)
			} else {
				value = append([]byte(nil), data...)
			}
		case 8:
			b, e := need(8)
			if e != nil {
				return nil, e
			}
			value = time.UnixMilli(int64(binary.BigEndian.Uint64(b)))
		case 9:
			b, e := need(16)
			if e != nil {
				return nil, e
			}
			value = fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", binary.BigEndian.Uint32(b[:4]), binary.BigEndian.Uint16(b[4:6]), binary.BigEndian.Uint16(b[6:8]), binary.BigEndian.Uint16(b[8:10]), uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]))
		default:
			return nil, fmt.Errorf("eventstream unknown header type %d", typ)
		}
		out[name] = Header{Name: name, Type: typ, Value: value}
	}
	return out, nil
}
