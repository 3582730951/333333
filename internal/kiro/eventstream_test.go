package kiro

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

func testFrame(headers map[string]string, payload []byte) []byte {
	var h bytes.Buffer
	for name, value := range headers {
		h.WriteByte(byte(len(name)))
		h.WriteString(name)
		h.WriteByte(7)
		_ = binary.Write(&h, binary.BigEndian, uint16(len(value)))
		h.WriteString(value)
	}
	total := 16 + h.Len() + len(payload)
	raw := make([]byte, total)
	binary.BigEndian.PutUint32(raw, uint32(total))
	binary.BigEndian.PutUint32(raw[4:], uint32(h.Len()))
	binary.BigEndian.PutUint32(raw[8:], crc32.ChecksumIEEE(raw[:8]))
	copy(raw[12:], h.Bytes())
	copy(raw[12+h.Len():], payload)
	binary.BigEndian.PutUint32(raw[total-4:], crc32.ChecksumIEEE(raw[:total-4]))
	return raw
}

func TestDecoderRandomFragmentationAndConsecutiveFrames(t *testing.T) {
	one := testFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"你"}`))
	two := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":2}`))
	raw := append(one, two...)
	d := NewDecoder()
	var got []Frame
	for i := 0; i < len(raw); {
		n := (i % 7) + 1
		if i+n > len(raw) {
			n = len(raw) - i
		}
		frames, err := d.Feed(raw[i : i+n])
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, frames...)
		i += n
	}
	if len(got) != 2 {
		t.Fatalf("frames=%d", len(got))
	}
	if got[0].HeaderString(":event-type") != "assistantResponseEvent" {
		t.Fatalf("headers=%v", got[0].Headers)
	}
}
func TestDecoderRejectsCRCErrorsAndOversizedFrames(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event"}, nil)
	raw[len(raw)-1] ^= 1
	if _, err := NewDecoder().Feed(raw); err == nil {
		t.Fatal("expected message crc error")
	}
	d := NewDecoder()
	d.MaxFrameSize = 15
	if _, err := d.Feed(testFrame(nil, nil)); err == nil {
		t.Fatal("expected size error")
	}
}

func TestDecodeResponseRejectsTruncatedAndExceptionFrames(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"hello"}`))
	if _, err := DecodeResponse(bytes.NewReader(raw[:len(raw)-1]), nil); err == nil {
		t.Fatal("expected truncated frame error")
	}
	exception := testFrame(map[string]string{":message-type": "exception", ":exception-type": "ThrottlingException"}, []byte(`{"message":"slow down"}`))
	if _, err := DecodeResponse(bytes.NewReader(exception), nil); err == nil {
		t.Fatal("expected exception frame error")
	}
}

func TestDecodeResponseToolAndUsage(t *testing.T) {
	raw := bytes.Join([][]byte{testFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"hello"}`)), testFrame(map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}, []byte(`{"name":"short","toolUseId":"t1","input":"{\"x\":"}`)), testFrame(map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}, []byte(`{"name":"short","toolUseId":"t1","input":"1}"}`)), testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":7,"outputTokens":3,"cacheReadTokens":2}`))}, nil)
	got, err := DecodeResponse(bytes.NewReader(raw), map[string]string{"short": "original"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello" || len(got.Tools) != 1 || got.Tools[0].Name != "original" || got.Tools[0].Input != "{\"x\":1}" || got.InputTokens != 7 {
		t.Fatalf("got=%+v", got)
	}
}

func TestDecodeWebSearchResponseProducesAnthropicBlocks(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"Kiro\",\"url\":\"https://example.test\",\"snippet\":\"result\"}]}"}]}}`)
	data, err := DecodeWebSearchResponse(raw, WebSearchRequest{Query: "kiro", ToolUseID: "srvtoolu_1"}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if data.WebSearch == nil || len(data.WebSearch.Results) != 1 || data.WebSearch.Results[0].Title != "Kiro" {
		t.Fatalf("data=%+v", data)
	}
	jsonBody := AnthropicJSON(data, "claude-sonnet-4-6", "msg_1")
	streamBody := AnthropicSSE(data, "claude-sonnet-4-6", "msg_1")
	for _, want := range []string{"server_tool_use", "web_search_tool_result", "web_search_requests"} {
		if !bytes.Contains(jsonBody, []byte(want)) || !bytes.Contains(streamBody, []byte(want)) {
			t.Fatalf("missing %q in web search output", want)
		}
	}
}
