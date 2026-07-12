package kiro

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
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
	tooLong := testFrame(map[string]string{":message-type": "exception", ":exception-type": "ContentLengthExceededException"}, []byte(`{"message":"Input is too long."}`))
	if _, err := DecodeResponse(bytes.NewReader(tooLong), nil); !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("content length exception = %v, want ErrContextTooLong", err)
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

func TestKiroMeteringDistinguishesMissingZeroNestedAndUnknown(t *testing.T) {
	missingRaw := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":7,"outputTokens":0}`))
	missing, err := DecodeResponse(bytes.NewReader(missingRaw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Metering.CacheReadTokens.Present || missing.Metering.CacheCreationTokens.Present {
		t.Fatalf("missing cache fields became reported zero: %+v", missing.Metering)
	}
	if strings.Contains(string(AnthropicJSON(missing, "claude-sonnet-4.6", "m")), "cache_read_input_tokens") {
		t.Fatal("unreported cache field leaked into downstream usage")
	}

	reportedRaw := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":{"inputTokenCount":9,"cacheReadInputTokens":0,"cacheCreationInputTokens":3},"cacheFooTokens":12}`))
	reported, err := DecodeResponse(bytes.NewReader(reportedRaw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reported.Metering.CacheReadTokens.Present || reported.Metering.CacheReadTokens.Value != 0 || !reported.Metering.CacheCreationTokens.Present || reported.CacheCreationTokens != 3 {
		t.Fatalf("nested/present metering lost: %+v", reported.Metering)
	}
	if len(reported.Metering.UnknownCacheFields) != 1 || reported.Metering.UnknownCacheFields[0].Name != "cacheFooTokens" || reported.Metering.UnknownCacheFields[0].SchemaHash == "" {
		t.Fatalf("unknown cache schema metadata = %+v", reported.Metering.UnknownCacheFields)
	}
	usageBody := string(AnthropicJSON(reported, "claude-sonnet-4.6", "m"))
	if !strings.Contains(usageBody, `"cache_read_input_tokens":0`) || !strings.Contains(usageBody, `"cache_creation_input_tokens":3`) {
		t.Fatalf("explicit cache values missing: %s", usageBody)
	}
}

func TestKiroMeteringRejectsNegativeAndFractionalTokenCounts(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":-1,"outputTokens":1.5,"cacheReadTokens":-2}`))
	got, err := DecodeResponse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metering.InputTokens.Present || got.Metering.OutputTokens.Present || got.Metering.CacheReadTokens.Present {
		t.Fatalf("invalid metering values were accepted: %+v", got.Metering)
	}
}

func TestKiroMetadataTokenUsageAndCreditsAreIndependent(t *testing.T) {
	metadata := testFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"tokenUsage":{"uncachedInputTokens":11,"cacheReadInputTokens":7,"cacheWriteInputTokens":3,"outputTokens":5,"totalTokens":26}}`))
	metering := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":1.25}`))
	for _, raw := range [][]byte{bytes.Join([][]byte{metadata, metering}, nil), bytes.Join([][]byte{metering, metadata}, nil)} {
		got, err := DecodeResponse(bytes.NewReader(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		m := got.Metering
		if got.UsageSource != UsageSourceUpstream || got.InputTokens != 11 || got.OutputTokens != 5 || got.CacheReadTokens != 7 || got.CacheCreationTokens != 3 {
			t.Fatalf("metadata token buckets = %+v data=%+v", m, got)
		}
		if !m.TotalInputTokens.Present || m.TotalInputTokens.Value != 21 || !m.TotalTokens.Present || m.TotalTokens.Value != 26 {
			t.Fatalf("metadata totals = %+v", m)
		}
		if !m.Credits.Present || m.Credits.Value != 1.25 || m.MetadataEventCount != 1 || m.MeteringEventCount != 1 || m.EventCount != 2 {
			t.Fatalf("event/credit metadata = %+v", m)
		}
	}
}

func TestKiroMetadataOverridesLegacyMeteringRegardlessOfOrder(t *testing.T) {
	metadata := testFrame(map[string]string{":message-type": "event", ":event-type": "metadataEvent"}, []byte(`{"usage":{"uncachedInputTokens":0,"cacheReadInputTokens":9,"cacheWriteInputTokens":2,"outputTokens":4}}`))
	legacy := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"inputTokens":999,"outputTokens":888,"cacheReadTokens":777}`))
	for _, raw := range [][]byte{bytes.Join([][]byte{metadata, legacy}, nil), bytes.Join([][]byte{legacy, metadata}, nil)} {
		got, err := DecodeResponse(bytes.NewReader(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Metering.InputTokens.Present || got.InputTokens != 0 || got.OutputTokens != 4 || got.CacheReadTokens != 9 || got.CacheCreationTokens != 2 || got.Metering.TotalInputTokens.Value != 11 {
			t.Fatalf("metadata did not override legacy fields: %+v", got.Metering)
		}
	}
}

func TestKiroCreditsOnlyIsNotUpstreamZeroTokenUsage(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"usage":0}`))
	got, err := DecodeResponse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageSource != UsageSourceUnreported || got.Metering.UsageSource() != UsageSourceUnreported {
		t.Fatalf("credits-only event became token usage: %+v", got.Metering)
	}
	if !got.Metering.Credits.Present || got.Metering.Credits.Value != 0 {
		t.Fatalf("reported zero credits lost presence: %+v", got.Metering.Credits)
	}
}

func TestKiroNestedMeteringCredits(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event", ":event-type": "meteringEvent"}, []byte(`{"meteringEvent":{"usage":0.75}}`))
	got, err := DecodeResponse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Metering.Credits.Present || got.Metering.Credits.Value != 0.75 {
		t.Fatalf("nested credits were not parsed: %+v", got.Metering.Credits)
	}
	if got.Metering.UsageSource() != UsageSourceUnreported {
		t.Fatalf("nested credits became token usage: %+v", got.Metering)
	}
}

func TestDecodeResponseRejectsInvalidToolJSON(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event", ":event-type": "toolUseEvent"}, []byte(`{"name":"bad","toolUseId":"t1","input":"{not-json"}`))
	_, err := DecodeResponse(bytes.NewReader(raw), nil)
	if !errors.Is(err, ErrInvalidToolInput) {
		t.Fatalf("err=%v, want ErrInvalidToolInput", err)
	}
}

func TestDecodeResponseThinkingNativeAndTagFallback(t *testing.T) {
	raw := bytes.Join([][]byte{
		testFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"thinking":"native","content":"before<think"}`)),
		testFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"content":"ing>fallback</thinking>after"}`)),
	}, nil)
	got, err := DecodeResponse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Thinking != "nativefallback" || got.Text != "beforeafter" || !containsString(got.CompatibilityLosses, LossThinkingTagFallback) {
		t.Fatalf("thinking fallback/native priority result = %+v", got)
	}
}

func TestDecodeResponseCanonicalizesActualModelWithoutVersionDrift(t *testing.T) {
	raw := testFrame(map[string]string{":message-type": "event", ":event-type": "assistantResponseEvent"}, []byte(`{"modelId":"claude-opus-4-9-20270101","content":"ok"}`))
	got, err := DecodeResponse(bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-opus-4.9" {
		t.Fatalf("actual model=%q, want same-version canonical id", got.Model)
	}
}
