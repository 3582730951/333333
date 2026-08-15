package bodysource

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"codex-account-pool/internal/supervisor"
)

const (
	stablePrefixLimit = 64 << 10
	maxJSONDepth      = 512
	maxScalarCapture  = 64 << 10
)

var ErrJSONDepth = errors.New("JSON nesting depth exceeded")

type Span struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type BodyMeta struct {
	Size               int64           `json:"size"`
	Type               string          `json:"type,omitempty"`
	ID                 string          `json:"id,omitempty"`
	Model              string          `json:"model,omitempty"`
	Status             string          `json:"status,omitempty"`
	ResponseID         string          `json:"response_id,omitempty"`
	ResponseModel      string          `json:"response_model,omitempty"`
	Stream             bool            `json:"stream"`
	StreamPresent      bool            `json:"stream_present"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	ConversationID     string          `json:"conversation_id,omitempty"`
	SessionID          string          `json:"session_id,omitempty"`
	ThreadID           string          `json:"thread_id,omitempty"`
	Fields             map[string]Span `json:"fields,omitempty"`
	StablePrefixHMAC   string          `json:"stable_prefix_hmac,omitempty"`
	StablePrefixBytes  int64           `json:"stable_prefix_bytes"`
	EstimatedTokens    int64           `json:"estimated_tokens"`
	CompactionTrigger  bool            `json:"compaction_trigger,omitempty"`
	ClientToolResult   bool            `json:"client_tool_result,omitempty"`
	ToolContext        bool            `json:"tool_context,omitempty"`
	// PromptCacheBreakpoint marks the unsupported client-only cache control found
	// anywhere below the request root. The Codex upstream sanitizer uses this bit
	// to select its targeted materialized fallback without rescanning every large
	// request body on the normal path.
	PromptCacheBreakpoint bool              `json:"prompt_cache_breakpoint,omitempty"`
	ObjectEnd             int64             `json:"-"`
	MemberCount           int               `json:"-"`
	InputItemCount        int               `json:"-"`
	LastInputRole         string            `json:"-"`
	LastInputType         string            `json:"-"`
	GoalSignalCandidate   bool              `json:"-"`
	GoalSignalQualified   bool              `json:"-"`
	EncryptedContentKey   bool              `json:"-"`
	LegacyInstructionMark bool              `json:"-"`
	Members               map[string]Span   `json:"-"`
	Kinds                 map[string]byte   `json:"-"`
	Scalars               map[string][]byte `json:"-"`
	FirstInputItem        Span              `json:"-"`
}

var trackedJSONFields = map[string]struct{}{
	"type": {}, "id": {}, "model": {}, "status": {}, "response": {}, "stream": {}, "prompt_cache_key": {}, "previous_response_id": {}, "conversation_id": {}, "session_id": {}, "thread_id": {},
	"instructions": {}, "system": {}, "tools": {}, "input": {}, "messages": {}, "max_output_tokens": {}, "max_tokens": {}, "reasoning": {}, "thinking": {},
	"store": {}, "tool_choice": {}, "parallel_tool_calls": {}, "prompt_cache_retention": {}, "prompt_cache_options": {}, "client_metadata": {}, "generate": {}, "include": {},
	"object": {}, "output": {}, "output_text": {}, "delta": {}, "item": {}, "usage": {}, "headers": {},
	"window_id": {}, "parent_thread_id": {}, "forked_from_thread_id": {}, "turn_metadata": {}, "turn_state": {},
	"compaction_trigger": {},
}

// ScanJSON validates a replayable JSON body while retaining only bounded top-level metadata.
func ScanJSON(ctx context.Context, source BodySource, hmacKey []byte) (BodyMeta, error) {
	meta := BodyMeta{Fields: make(map[string]Span)}
	if err := ctx.Err(); err != nil {
		return meta, err
	}
	if source == nil {
		return meta, errors.New("nil body source")
	}
	r, err := source.Open()
	if err != nil {
		return meta, err
	}
	defer r.Close()
	return scanJSONReader(ctx, r, source.Size(), hmacKey)
}

type jsonScanResult struct {
	meta BodyMeta
	err  error
}

// CaptureJSON captures and scans src concurrently, so the incoming stream is consumed once.
func CaptureJSON(ctx context.Context, src io.Reader, options CaptureOptions, hmacKey []byte) (BodySource, BodyMeta, error) {
	reader, writer := io.Pipe()
	result := make(chan jsonScanResult, 1)
	expectedSize := int64(-1)
	if options.ExpectedBytes > 0 {
		expectedSize = options.ExpectedBytes
	}
	go func() {
		defer func() {
			if panicValue := recover(); panicValue != nil {
				supervisor.LogPanic("body-json-scanner", panicValue)
				err := errors.New("body JSON scanner panicked")
				_ = reader.CloseWithError(err)
				result <- jsonScanResult{err: err}
			}
		}()
		meta, err := scanJSONReader(ctx, reader, expectedSize, hmacKey)
		_ = reader.CloseWithError(err)
		result <- jsonScanResult{meta: meta, err: err}
	}()
	source, captureErr := Capture(ctx, io.TeeReader(src, writer), options)
	_ = writer.CloseWithError(captureErr)
	scan := <-result
	if captureErr != nil {
		if source != nil {
			_ = source.Close()
		}
		return nil, BodyMeta{}, captureErr
	}
	if scan.err != nil {
		_ = source.Close()
		return nil, BodyMeta{}, scan.err
	}
	return source, scan.meta, nil
}

func scanJSONReader(ctx context.Context, r io.Reader, expectedSize int64, hmacKey []byte) (BodyMeta, error) {
	meta := BodyMeta{Fields: make(map[string]Span), Members: make(map[string]Span), Kinds: make(map[string]byte), Scalars: make(map[string][]byte)}
	if err := ctx.Err(); err != nil {
		return meta, err
	}
	prefixBytes := expectedSize
	if prefixBytes < 0 || prefixBytes > stablePrefixLimit {
		prefixBytes = stablePrefixLimit
	}
	var prefix hash.Hash
	if len(hmacKey) > 0 {
		prefix = hmac.New(sha256.New, hmacKey)
	}
	bufferSize := DefaultChunkSize
	if expectedSize > 0 && expectedSize < int64(bufferSize) {
		bufferSize = int(expectedSize)
		if bufferSize < 256 {
			bufferSize = 256
		}
	}
	s := &jsonMetaScanner{ctx: ctx, reader: bufio.NewReaderSize(r, bufferSize), prefix: prefix, prefixBytes: prefixBytes, meta: &meta}
	if err := s.scanRoot(); err != nil {
		return BodyMeta{}, err
	}
	s.flushPrefix()
	meta.Size = s.offset
	meta.StablePrefixBytes = prefixBytes
	if meta.StablePrefixBytes > meta.Size {
		meta.StablePrefixBytes = meta.Size
	}
	meta.EstimatedTokens = s.runes/4 + 1
	if prefix != nil {
		meta.StablePrefixHMAC = hex.EncodeToString(prefix.Sum(nil))
	}
	if expectedSize >= 0 && expectedSize != s.offset {
		return BodyMeta{}, fmt.Errorf("body size changed during JSON scan: got %d want %d", s.offset, expectedSize)
	}
	meta.GoalSignalQualified = meta.GoalSignalCandidate && (meta.GoalSignalQualified || s.goalStatusKey)
	return meta, nil
}

type jsonMetaScanner struct {
	ctx           context.Context
	reader        *bufio.Reader
	offset        int64
	runes         int64
	prefix        hash.Hash
	prefixBytes   int64
	prefixBuf     [4096]byte
	prefixN       int
	meta          *BodyMeta
	inputDepth    int
	inputRole     string
	inputType     string
	goalTail      [3]byte
	goalTailN     int
	goalStatusKey bool
}

type scannedValue struct {
	kind byte
	raw  []byte
}

func (s *jsonMetaScanner) scanRoot() error {
	if err := s.skipSpace(); err != nil {
		return err
	}
	b, err := s.peek()
	if err != nil {
		return s.syntax("empty JSON body")
	}
	if b != '{' {
		return s.syntax("top-level JSON value must be an object")
	}
	if err = s.scanTopObject(); err != nil {
		return err
	}
	if err = s.skipSpace(); err != nil {
		return err
	}
	if _, err = s.peek(); err != io.EOF {
		if err != nil {
			return err
		}
		return s.syntax("unexpected data after top-level object")
	}
	return nil
}

func (s *jsonMetaScanner) scanTopObject() error {
	if err := s.expect('{'); err != nil {
		return err
	}
	if err := s.skipSpace(); err != nil {
		return err
	}
	if ok, err := s.consumeIf('}'); ok || err != nil {
		if ok {
			s.meta.ObjectEnd = s.offset - 1
		}
		return err
	}
	previousComma := int64(-1)
	for {
		memberStart := s.offset
		keyValue, err := s.scanString(maxScalarCapture)
		if err != nil {
			return err
		}
		key, _ := decodeJSONString(keyValue.raw)
		if key == "prompt_cache_breakpoint" {
			s.meta.PromptCacheBreakpoint = true
		}
		if key == "status" {
			s.goalStatusKey = true
		}
		if key == "encrypted_content" {
			s.meta.EncryptedContentKey = true
		}
		if err = s.skipSpace(); err != nil {
			return err
		}
		if err = s.expect(':'); err != nil {
			return err
		}
		if err = s.skipSpace(); err != nil {
			return err
		}
		start := s.offset
		_, tracked := trackedJSONFields[key]
		var value scannedValue
		if key == "input" {
			if next, peekErr := s.peek(); peekErr == nil && next == '[' {
				value, err = scannedValue{kind: '['}, s.scanInputArray(2)
			} else {
				value, err = s.scanValue(1, tracked)
			}
		} else if key == "response" {
			if next, peekErr := s.peek(); peekErr == nil && next == '{' {
				value, err = scannedValue{kind: '{'}, s.scanResponseObject(2)
			} else {
				value, err = s.scanValue(1, tracked)
			}
		} else {
			value, err = s.scanValue(1, tracked)
		}
		if err != nil {
			return err
		}
		if tracked {
			s.meta.Fields[key] = Span{Offset: start, Length: s.offset - start}
			s.meta.Kinds[key] = value.kind
			if value.raw != nil {
				s.meta.Scalars[key] = append([]byte(nil), value.raw...)
			}
			s.applyField(key, value)
		}
		if err = s.skipSpace(); err != nil {
			return err
		}
		separator := s.offset
		next, err := s.peek()
		if err != nil {
			return err
		}
		switch next {
		case '}':
			if tracked {
				deleteStart := memberStart
				if previousComma >= 0 {
					deleteStart = previousComma
				}
				s.meta.Members[key] = Span{Offset: deleteStart, Length: separator - deleteStart}
			}
			_, _ = s.readByte()
			s.meta.ObjectEnd = separator
			s.meta.MemberCount++
			return nil
		case ',':
			_, _ = s.readByte()
			if err = s.skipSpace(); err != nil {
				return err
			}
			if tracked {
				s.meta.Members[key] = Span{Offset: memberStart, Length: s.offset - memberStart}
			}
			previousComma = separator
			s.meta.MemberCount++
		default:
			return s.syntax("expected ',' or '}'")
		}
	}
}

func (s *jsonMetaScanner) scanInputArray(depth int) error {
	if depth > maxJSONDepth {
		return ErrJSONDepth
	}
	if err := s.expect('['); err != nil {
		return err
	}
	if err := s.skipSpace(); err != nil {
		return err
	}
	if ok, err := s.consumeIf(']'); ok || err != nil {
		return err
	}
	first := true
	for {
		start := s.offset
		s.inputDepth = depth + 1
		s.inputRole, s.inputType = "", ""
		if _, err := s.scanValue(depth, false); err != nil {
			return err
		}
		s.meta.InputItemCount++
		s.meta.LastInputRole = s.inputRole
		s.meta.LastInputType = s.inputType
		s.inputDepth = 0
		if first {
			s.meta.FirstInputItem = Span{Offset: start, Length: s.offset - start}
			first = false
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
		if ok, err := s.consumeIf(']'); ok || err != nil {
			return err
		}
		if err := s.expect(','); err != nil {
			return err
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
	}
}

func (s *jsonMetaScanner) applyField(key string, value scannedValue) {
	switch key {
	case "stream":
		if value.kind == 't' {
			s.meta.Stream, s.meta.StreamPresent = true, true
		} else if value.kind == 'f' {
			s.meta.Stream, s.meta.StreamPresent = false, true
		}
	case "compaction_trigger":
		s.meta.CompactionTrigger = value.kind == 't'
	case "type", "id", "model", "status", "prompt_cache_key", "previous_response_id", "conversation_id", "session_id", "thread_id":
		decoded, ok := decodeJSONString(value.raw)
		if !ok {
			return
		}
		switch key {
		case "type":
			s.meta.Type = decoded
			s.applySemanticType(decoded)
		case "id":
			s.meta.ID = decoded
		case "model":
			s.meta.Model = decoded
		case "status":
			s.meta.Status = decoded
		case "prompt_cache_key":
			s.meta.PromptCacheKey = decoded
		case "previous_response_id":
			s.meta.PreviousResponseID = decoded
		case "conversation_id":
			s.meta.ConversationID = decoded
		case "session_id":
			s.meta.SessionID = decoded
		case "thread_id":
			s.meta.ThreadID = decoded
		}
	}
}

func decodeJSONString(raw []byte) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	decoded, err := strconv.Unquote(string(raw))
	return decoded, err == nil
}

func (s *jsonMetaScanner) scanValue(depth int, capture bool) (scannedValue, error) {
	if depth > maxJSONDepth {
		return scannedValue{}, ErrJSONDepth
	}
	b, err := s.peek()
	if err != nil {
		return scannedValue{}, s.syntax("unexpected end of JSON value")
	}
	switch b {
	case '"':
		limit := 0
		if capture {
			limit = maxScalarCapture
		}
		return s.scanString(limit)
	case '{':
		return scannedValue{kind: '{'}, s.scanObject(depth + 1)
	case '[':
		return scannedValue{kind: '['}, s.scanArray(depth + 1)
	case 't':
		return s.scanLiteral("true", capture)
	case 'f':
		return s.scanLiteral("false", capture)
	case 'n':
		return s.scanLiteral("null", capture)
	default:
		if b == '-' || b >= '0' && b <= '9' {
			return s.scanNumber(capture)
		}
		return scannedValue{}, s.syntax("invalid JSON value")
	}
}

func (s *jsonMetaScanner) scanObject(depth int) error {
	if depth > maxJSONDepth {
		return ErrJSONDepth
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	if err := s.skipSpace(); err != nil {
		return err
	}
	if ok, err := s.consumeIf('}'); ok || err != nil {
		return err
	}
	for {
		keyValue, err := s.scanString(256)
		if err != nil {
			return err
		}
		key, _ := decodeJSONString(keyValue.raw)
		if key == "prompt_cache_breakpoint" {
			s.meta.PromptCacheBreakpoint = true
		}
		if key == "status" {
			s.goalStatusKey = true
		}
		if key == "encrypted_content" {
			s.meta.EncryptedContentKey = true
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
		captureInputIdentity := depth == s.inputDepth && (key == "role" || key == "type")
		value, err := s.scanValue(depth, key == "type" || key == "name" || captureInputIdentity)
		if err != nil {
			return err
		}
		if key == "type" {
			if decoded, ok := decodeJSONString(value.raw); ok {
				s.applySemanticType(decoded)
				if depth == s.inputDepth {
					s.inputType = decoded
				}
			}
		} else if key == "name" {
			if decoded, ok := decodeJSONString(value.raw); ok && (decoded == "create_goal" || decoded == "update_goal") {
				s.meta.GoalSignalQualified = true
			}
		} else if key == "role" && depth == s.inputDepth {
			if decoded, ok := decodeJSONString(value.raw); ok {
				s.inputRole = decoded
			}
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
		if ok, err := s.consumeIf('}'); ok || err != nil {
			return err
		}
		if err := s.expect(','); err != nil {
			return err
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
	}
}

func (s *jsonMetaScanner) scanResponseObject(depth int) error {
	if depth > maxJSONDepth {
		return ErrJSONDepth
	}
	if err := s.expect('{'); err != nil {
		return err
	}
	if err := s.skipSpace(); err != nil {
		return err
	}
	if ok, err := s.consumeIf('}'); ok || err != nil {
		return err
	}
	for {
		keyValue, err := s.scanString(256)
		if err != nil {
			return err
		}
		key, _ := decodeJSONString(keyValue.raw)
		if err = s.skipSpace(); err != nil {
			return err
		}
		if err = s.expect(':'); err != nil {
			return err
		}
		if err = s.skipSpace(); err != nil {
			return err
		}
		value, valueErr := s.scanValue(depth, key == "id" || key == "model" || key == "status")
		if valueErr != nil {
			return valueErr
		}
		if decoded, ok := decodeJSONString(value.raw); ok {
			switch key {
			case "id":
				s.meta.ResponseID = decoded
			case "model":
				s.meta.ResponseModel = decoded
			case "status":
				if s.meta.Status == "" {
					s.meta.Status = decoded
				}
			}
		}
		if err = s.skipSpace(); err != nil {
			return err
		}
		if ok, closeErr := s.consumeIf('}'); ok || closeErr != nil {
			return closeErr
		}
		if err = s.expect(','); err != nil {
			return err
		}
		if err = s.skipSpace(); err != nil {
			return err
		}
	}
}

func (s *jsonMetaScanner) applySemanticType(value string) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "compaction_trigger":
		s.meta.CompactionTrigger = true
	case "function_call_output", "local_shell_call_output", "mcp_tool_call_output", "custom_tool_call_output", "tool_search_output", "tool_result", "tool_use_result":
		s.meta.ClientToolResult = true
		s.meta.ToolContext = true
	case "function_call", "custom_tool_call", "local_shell_call", "mcp_tool_call", "tool_search_call", "tool_use", "mcp_tool_use":
		s.meta.ToolContext = true
	}
}

func (s *jsonMetaScanner) scanArray(depth int) error {
	if depth > maxJSONDepth {
		return ErrJSONDepth
	}
	if err := s.expect('['); err != nil {
		return err
	}
	if err := s.skipSpace(); err != nil {
		return err
	}
	if ok, err := s.consumeIf(']'); ok || err != nil {
		return err
	}
	for {
		if _, err := s.scanValue(depth, false); err != nil {
			return err
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
		if ok, err := s.consumeIf(']'); ok || err != nil {
			return err
		}
		if err := s.expect(','); err != nil {
			return err
		}
		if err := s.skipSpace(); err != nil {
			return err
		}
	}
}

func (s *jsonMetaScanner) scanString(captureLimit int) (scannedValue, error) {
	value := scannedValue{kind: '"'}
	if err := s.expect('"'); err != nil {
		return value, err
	}
	if captureLimit > 0 {
		value.raw = append(value.raw, '"')
	}
	escaped := false
	unicodeDigits := 0
	s.goalTailN = 0
	stringGoal := false
	for {
		segment, readErr := s.reader.ReadSlice('"')
		goalInSegment := s.observeGoalSignalSegment(segment)
		if goalInSegment {
			stringGoal = true
			if bytes.Contains(segment, []byte("create_goal")) || bytes.Contains(segment, []byte("update_goal")) ||
				bytes.Contains(segment, []byte("Continue working toward the active thread goal.")) {
				s.meta.GoalSignalQualified = true
			}
		}
		// Goal tool output is JSON encoded inside one JSON string. Once its stable
		// goal token appears, look for the status envelope only in the remainder of
		// that same string instead of rescanning every million-token body.
		if stringGoal && bytes.Contains(segment, []byte("status")) {
			s.meta.GoalSignalQualified = true
		}
		runes := int64(0)
		for i, b := range segment {
			if b == '#' {
				s.meta.LegacyInstructionMark = true
			}
			if b&0xc0 != 0x80 {
				runes++
			}
			if unicodeDigits > 0 {
				if !isHex(b) {
					_ = s.consumeScannedBytes(segment[:i+1], runes)
					return scannedValue{}, s.syntax("invalid JSON unicode escape")
				}
				unicodeDigits--
				continue
			}
			if escaped {
				escaped = false
				switch b {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				case 'u':
					unicodeDigits = 4
				default:
					_ = s.consumeScannedBytes(segment[:i+1], runes)
					return scannedValue{}, s.syntax("invalid JSON escape")
				}
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				if err := s.consumeScannedBytes(segment[:i+1], runes); err != nil {
					return scannedValue{}, err
				}
				captureStringSegment(&value, segment[:i+1], captureLimit)
				return value, nil
			}
			if b < 0x20 {
				_ = s.consumeScannedBytes(segment[:i+1], runes)
				return scannedValue{}, s.syntax("unescaped control byte in JSON string")
			}
		}
		if err := s.consumeScannedBytes(segment, runes); err != nil {
			return scannedValue{}, err
		}
		captureStringSegment(&value, segment, captureLimit)
		if readErr != nil && readErr != bufio.ErrBufferFull {
			return scannedValue{}, s.syntax("unterminated JSON string")
		}
	}
}

func captureStringSegment(value *scannedValue, segment []byte, captureLimit int) {
	if value.raw == nil || captureLimit <= 0 {
		return
	}
	if len(segment) > captureLimit-len(value.raw) {
		value.raw = nil
		return
	}
	value.raw = append(value.raw, segment...)
}

func (s *jsonMetaScanner) consumeScannedBytes(p []byte, runes int64) error {
	if len(p) == 0 {
		return nil
	}
	start := s.offset
	s.offset += int64(len(p))
	s.runes += runes
	if s.prefix != nil && start < s.prefixBytes {
		s.flushPrefix()
		n := s.prefixBytes - start
		if n > int64(len(p)) {
			n = int64(len(p))
		}
		_, _ = s.prefix.Write(p[:n])
	}
	return s.ctx.Err()
}

func (s *jsonMetaScanner) observeGoalSignalSegment(segment []byte) bool {
	marker := []byte("goal")
	if bytes.Contains(segment, marker) {
		s.meta.GoalSignalCandidate = true
		return true
	}
	if s.goalTailN > 0 && len(segment) > 0 {
		var boundary [6]byte
		n := copy(boundary[:], s.goalTail[:s.goalTailN])
		prefixN := len(segment)
		if prefixN > len(marker)-1 {
			prefixN = len(marker) - 1
		}
		n += copy(boundary[n:], segment[:prefixN])
		if bytes.Contains(boundary[:n], marker) {
			s.meta.GoalSignalCandidate = true
			return true
		}
	}
	tailN := len(segment)
	if tailN > len(s.goalTail) {
		tailN = len(s.goalTail)
	}
	copy(s.goalTail[:], segment[len(segment)-tailN:])
	s.goalTailN = tailN
	return false
}

func (s *jsonMetaScanner) scanLiteral(literal string, capture bool) (scannedValue, error) {
	value := scannedValue{kind: literal[0]}
	for i := range literal {
		b, err := s.readByte()
		if err != nil || b != literal[i] {
			return scannedValue{}, s.syntax("invalid JSON literal")
		}
	}
	if capture {
		value.raw = []byte(literal)
	}
	return value, nil
}

func (s *jsonMetaScanner) scanNumber(capture bool) (scannedValue, error) {
	value := scannedValue{kind: '0'}
	appendByte := func(b byte) {
		if capture && len(value.raw) < 128 {
			value.raw = append(value.raw, b)
		}
	}
	if ok, err := s.consumeIf('-'); err != nil {
		return scannedValue{}, err
	} else if ok {
		appendByte('-')
	}
	b, err := s.peek()
	if err != nil {
		return scannedValue{}, s.syntax("incomplete JSON number")
	}
	if b == '0' {
		_, _ = s.readByte()
		appendByte(b)
		if next, _ := s.peek(); next >= '0' && next <= '9' {
			return scannedValue{}, s.syntax("leading zero in JSON number")
		}
	} else if b >= '1' && b <= '9' {
		for {
			b, err = s.peek()
			if err != nil || b < '0' || b > '9' {
				break
			}
			_, _ = s.readByte()
			appendByte(b)
		}
	} else {
		return scannedValue{}, s.syntax("invalid JSON number")
	}
	if ok, err := s.consumeIf('.'); err != nil {
		return scannedValue{}, err
	} else if ok {
		appendByte('.')
		if err = s.scanNumberDigits(appendByte); err != nil {
			return scannedValue{}, err
		}
	}
	if b, err = s.peek(); err == nil && (b == 'e' || b == 'E') {
		_, _ = s.readByte()
		appendByte(b)
		if b, err = s.peek(); err == nil && (b == '+' || b == '-') {
			_, _ = s.readByte()
			appendByte(b)
		}
		if err = s.scanNumberDigits(appendByte); err != nil {
			return scannedValue{}, err
		}
	}
	return value, nil
}

func (s *jsonMetaScanner) scanNumberDigits(appendByte func(byte)) error {
	digits := 0
	for {
		b, err := s.peek()
		if err != nil || b < '0' || b > '9' {
			break
		}
		_, _ = s.readByte()
		appendByte(b)
		digits++
	}
	if digits == 0 {
		return s.syntax("incomplete JSON number")
	}
	return nil
}

func (s *jsonMetaScanner) skipSpace() error {
	for {
		b, err := s.peek()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			return nil
		}
		if _, err = s.readByte(); err != nil {
			return err
		}
	}
}

func (s *jsonMetaScanner) expect(want byte) error {
	b, err := s.readByte()
	if err != nil || b != want {
		return s.syntax("expected " + strconv.QuoteRune(rune(want)))
	}
	return nil
}

func (s *jsonMetaScanner) consumeIf(want byte) (bool, error) {
	b, err := s.peek()
	if err != nil {
		return false, err
	}
	if b != want {
		return false, nil
	}
	_, err = s.readByte()
	return true, err
}

func (s *jsonMetaScanner) peek() (byte, error) {
	b, err := s.reader.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (s *jsonMetaScanner) readByte() (byte, error) {
	b, err := s.reader.ReadByte()
	if err != nil {
		return 0, err
	}
	s.offset++
	if b&0xc0 != 0x80 {
		s.runes++
	}
	if s.prefix != nil && s.offset <= s.prefixBytes {
		s.prefixBuf[s.prefixN] = b
		s.prefixN++
		if s.prefixN == len(s.prefixBuf) {
			s.flushPrefix()
		}
	}
	if s.offset&4095 == 0 {
		if err = s.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return b, nil
}

func (s *jsonMetaScanner) flushPrefix() {
	if s.prefix != nil && s.prefixN > 0 {
		_, _ = s.prefix.Write(s.prefixBuf[:s.prefixN])
		s.prefixN = 0
	}
}

func (s *jsonMetaScanner) syntax(message string) error {
	return fmt.Errorf("invalid JSON at byte %d: %s", s.offset, message)
}

func isHex(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
