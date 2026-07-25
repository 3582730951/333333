package upstream

// Antigravity (Google Cloud Code) upstream provider.
//
// Wire protocol:
//   - HTTP/1.1 forced (ALPN: http/1.1 only) — mirrors the real Antigravity Node.js client.
//   - Auth: OAuth 2.0 Bearer token; refresh via https://oauth2.googleapis.com/token.
//   - Stream: POST {base}/v1internal:streamGenerateContent?alt=sse — returns newline-
//     delimited SSE where each "data:" line is a Gemini GenerateContentResponse chunk.
//   - Non-stream: POST {base}/v1internal:generateContent — returns a single JSON object.
//
// Translation layer (Anthropic → Gemini → Antigravity):
//   - messages[{role,content}] → request.contents[{role,parts:[{text}]}]
//   - system string        → request.systemInstruction.parts[{text}]
//   - max_tokens           → request.generationConfig.maxOutputTokens
//   - temperature          → request.generationConfig.temperature
//   - tools[…]             → request.tools[{functionDeclarations:[…]}]
//   - Response chunks are translated back to Anthropic SSE.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/config"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityProdBaseURL  = "https://cloudcode-pa.googleapis.com"
	antigravityStreamPath   = "/v1internal:streamGenerateContent?alt=sse"
	antigravityGeneratePath = "/v1internal:generateContent"
	antigravityDefaultUA    = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	// refreshSkew: token is considered expired 3000s early — mirrors CLIProxyAPI source.
	antigravityRefreshSkew = 3000

	// Vertex AI Anthropic publisher endpoint — used when the requested model is a
	// Claude model routed through the Antigravity OAuth credentials.
	// Claude models are not served by the cloudcode-pa v1internal endpoint; they
	// require Vertex AI's native Anthropic-format path instead.
	antigravityVertexBaseURL    = "https://us-east5-aiplatform.googleapis.com"
	antigravityVertexRegion     = "us-east5"
	antigravityVertexStreamSuffix = ":streamRawPredict" // returns native Anthropic SSE
	antigravityVertexRawSuffix    = ":rawPredict"       // returns native Anthropic JSON
	// anthropicVersion is the value Vertex AI requires in the request body.
	antigravityVertexAnthropicVersion = "vertex-2023-10-16"
)

// antigravityTransport is a cached HTTP/1.1-only transport (one per process).
// Antigravity's backend requires HTTP/1.1; HTTP/2 causes request failures.
var antigravityTransport = &http.Transport{
	ForceAttemptHTTP2: false,
	TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{}, // wipe H2 upgrade
}

// AntigravityTokenResponse is the OAuth refresh token response shape.
type AntigravityTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// RefreshAntigravityToken exchanges a refresh token for a new access token.
// Returns the new access token, updated refresh token, and expiry (Unix seconds).
func RefreshAntigravityToken(ctx context.Context, refreshToken string, cfg *config.Config) (AntigravityTokenResponse, error) {
	form := "client_id=" + cfg.AntigravityOAuthClientID +
		"&client_secret=" + cfg.AntigravityOAuthClientSecret +
		"&grant_type=refresh_token" +
		"&refresh_token=" + refreshToken
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.AntigravityOAuthTokenURL,
		strings.NewReader(form))
	if err != nil {
		return AntigravityTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Use Go's default UA for OAuth token refresh — mirrors real Antigravity behaviour.
	resp, err := antigravityTransport.RoundTrip(req)
	if err != nil {
		return AntigravityTokenResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return AntigravityTokenResponse{}, fmt.Errorf("antigravity token refresh %d: %s", resp.StatusCode, body)
	}
	var tr AntigravityTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return AntigravityTokenResponse{}, err
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	return tr, nil
}

// AntigravityRequest is the pool-side representation of a single Antigravity inference call.
type AntigravityRequest struct {
	AccessToken       string
	ProjectID         string
	Model             string // Gemini or Claude model slug
	BaseURL           string // empty → production
	UserAgent         string // empty → default UA
	Body              []byte // Anthropic-format request JSON
	Stream            bool
	CachedContentName string // optional: Vertex AI resource name for explicit caching
}

// AntigravityIsClaudeModel reports whether the given model slug is an Anthropic
// Claude model routed through the Antigravity provider. Claude models require a
// different upstream endpoint (Vertex AI Anthropic publisher) and a different wire
// format (native Anthropic JSON) than Gemini models (cloudcode-pa v1internal).
func AntigravityIsClaudeModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude")
}

// DoAntigravity sends one Antigravity request and returns the raw HTTP response.
// The caller is responsible for closing the response body.
//
// Routing:
//   - Claude models → Vertex AI Anthropic publisher endpoint (native Anthropic format).
//   - Gemini models → cloudcode-pa.googleapis.com/v1internal (Gemini format, existing path).
func DoAntigravity(ctx context.Context, req AntigravityRequest) (*http.Response, error) {
	if AntigravityIsClaudeModel(req.Model) {
		return doAntigravityClaudeViaVertex(ctx, req)
	}

	base := strings.TrimRight(antigravityProdBaseURL, "/")
	if u := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/"); u != "" {
		base = u
	}

	// Convert Anthropic request body → Antigravity wire format.
	agBody, err := anthropicToAntigravity(req.Body, req.Model, req.ProjectID, req.CachedContentName)
	if err != nil {
		return nil, fmt.Errorf("antigravity body conversion: %w", err)
	}

	var target string
	if req.Stream {
		target = base + antigravityStreamPath
	} else {
		target = base + antigravityGeneratePath
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target,
		bytes.NewReader(agBody))
	if err != nil {
		return nil, err
	}
	ua := antigravityDefaultUA
	if u := strings.TrimSpace(req.UserAgent); u != "" {
		ua = u
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.AccessToken)
	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Host", httpReq.URL.Host)
	httpReq.Close = true // Connection: close — matches Node.js https default

	client := &http.Client{Transport: antigravityTransport, Timeout: 5 * time.Minute}
	return client.Do(httpReq)
}

// doAntigravityClaudeViaVertex routes a Claude model request through the Vertex AI
// Anthropic publisher endpoint. The request body is passed in native Anthropic format
// (no Gemini conversion); only anthropic_version is injected and the top-level model
// field is removed (it lives in the URL instead).
//
// Vertex AI supports HTTP/2, so we use a plain http.Client (not the HTTP/1.1-only
// antigravityTransport which is required only for the cloudcode-pa Gemini endpoint).
func doAntigravityClaudeViaVertex(ctx context.Context, req AntigravityRequest) (*http.Response, error) {
	if req.ProjectID == "" {
		return nil, fmt.Errorf("antigravity: project_id required for Claude-via-Vertex routing")
	}

	body, err := prepareAntigravityClaudeVertexBody(req.Body)
	if err != nil {
		return nil, fmt.Errorf("antigravity: claude vertex body prep: %w", err)
	}

	suffix := antigravityVertexRawSuffix
	if req.Stream {
		suffix = antigravityVertexStreamSuffix
	}
	target := fmt.Sprintf(
		"%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s%s",
		antigravityVertexBaseURL, req.ProjectID, antigravityVertexRegion, req.Model, suffix,
	)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.AccessToken)

	client := &http.Client{Timeout: 5 * time.Minute}
	return client.Do(httpReq)
}

// prepareAntigravityClaudeVertexBody adjusts an Anthropic-format request body for
// the Vertex AI Anthropic publisher:
//   - Sets anthropic_version to the Vertex-required value (overrides any client value).
//   - Removes the top-level "model" field (the model is encoded in the request URL).
func prepareAntigravityClaudeVertexBody(body []byte) ([]byte, error) {
	out, err := sjson.SetBytes(body, "anthropic_version", antigravityVertexAnthropicVersion)
	if err != nil {
		return nil, err
	}
	out, err = sjson.DeleteBytes(out, "model")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// anthropicToAntigravity converts an Anthropic /v1/messages JSON body to the
// Antigravity wire format: outer Antigravity envelope wrapping a Gemini inner request.
// When cacheRef is non-empty the cache is referenced via request.cachedContent and
// only the final (new) user turn is sent in request.contents; the prefix is served
// from the cache so we must NOT repeat it here.
func anthropicToAntigravity(body []byte, model, projectID, cacheRef string) ([]byte, error) {
	var (
		contents    []geminiContent
		contentsErr error
	)
	if cacheRef != "" {
		// Cache mode: only the last user turn goes in request.contents.
		_, lastTurns, _, _ := ExtractAntigravityPrefixForCache(body)
		contents = lastTurns
	} else {
		// Normal mode: all turns.
		contents, contentsErr = anthropicMessagesToGeminiContents(body)
		if contentsErr != nil {
			return nil, contentsErr
		}
	}
	contentsJSON, err := json.Marshal(contents)
	if err != nil {
		return nil, err
	}

	// 2. Build the outer Antigravity envelope using sjson.
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "model", model)
	out, _ = sjson.SetBytes(out, "userAgent", "antigravity")
	out, _ = sjson.SetBytes(out, "requestType", "agent")
	if projectID != "" {
		out, _ = sjson.SetBytes(out, "project", projectID)
	}
	out, _ = sjson.SetBytes(out, "requestId", "agent-"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	out, _ = sjson.SetRawBytes(out, "request.contents", contentsJSON)
	out, _ = sjson.SetBytes(out, "request.sessionId", antigravityStableSessionID(body))
	if cacheRef != "" {
		// Inject explicit cache reference; systemInstruction is already in the cache.
		out, _ = sjson.SetBytes(out, "request.cachedContent", cacheRef)
	}

	// 3. Forward generationConfig fields.
	if mt := gjson.GetBytes(body, "max_tokens"); mt.Exists() {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", mt.Int())
	}
	if temp := gjson.GetBytes(body, "temperature"); temp.Exists() {
		out, _ = sjson.SetBytes(out, "request.generationConfig.temperature", temp.Float())
	}
	if topP := gjson.GetBytes(body, "top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topP", topP.Float())
	}
	if topK := gjson.GetBytes(body, "top_k"); topK.Exists() {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topK", topK.Int())
	}

	// 4. System prompt → systemInstruction (omitted in cache mode; it lives in the cache).
	if cacheRef == "" {
		if sys := gjson.GetBytes(body, "system"); sys.Exists() {
			sysText := ""
			switch sys.Type {
			case gjson.String:
				sysText = sys.String()
			case gjson.JSON:
				// Array of content blocks — concatenate text parts.
				for _, b := range sys.Array() {
					if b.Get("type").String() == "text" {
						sysText += b.Get("text").String()
					}
				}
			}
			if sysText != "" {
				out, _ = sjson.SetBytes(out, "request.systemInstruction.parts.0.text", sysText)
			}
		}
	}

	// 5. Tools → functionDeclarations.
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		funcDecls := make([]json.RawMessage, 0, len(tools.Array()))
		for _, t := range tools.Array() {
			fdBytes := []byte(t.Raw)
			// Remap: Anthropic uses "input_schema", Gemini uses "parameters".
			if is := gjson.GetBytes(fdBytes, "input_schema"); is.Exists() {
				if updated, e := sjson.SetRawBytes(fdBytes, "parameters", []byte(is.Raw)); e == nil {
					fdBytes = updated
				}
				if updated, e := sjson.DeleteBytes(fdBytes, "input_schema"); e == nil {
					fdBytes = updated
				}
			}
			funcDecls = append(funcDecls, json.RawMessage(fdBytes))
		}
		fdJSON, err := json.Marshal(funcDecls)
		if err == nil {
			out, _ = sjson.SetRawBytes(out, "request.tools.0.functionDeclarations", fdJSON)
		}
	}

	return out, nil
}

// geminiContent is a single Gemini content object.
type geminiContent struct {
	Role  string          `json:"role"`
	Parts []geminiPart    `json:"parts"`
}

type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	FunctionCall     json.RawMessage `json:"functionCall,omitempty"`
	FunctionResponse json.RawMessage `json:"functionResponse,omitempty"`
}

// anthropicMessagesToGeminiContents converts an Anthropic messages array to
// the Gemini contents format.
func anthropicMessagesToGeminiContents(body []byte) ([]geminiContent, error) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil, fmt.Errorf("anthropic body missing messages array")
	}
	out := make([]geminiContent, 0, len(msgs.Array()))
	for _, m := range msgs.Array() {
		role := m.Get("role").String()
		// Anthropic "assistant" → Gemini "model"
		if role == "assistant" {
			role = "model"
		}
		gc := geminiContent{Role: role}
		content := m.Get("content")
		switch content.Type {
		case gjson.String:
			gc.Parts = []geminiPart{{Text: content.String()}}
		case gjson.JSON:
			for _, block := range content.Array() {
				typ := block.Get("type").String()
				switch typ {
				case "text":
					gc.Parts = append(gc.Parts, geminiPart{Text: block.Get("text").String()})
				case "tool_use":
					// Anthropic tool_use → Gemini functionCall
					fc := map[string]interface{}{
						"name": block.Get("name").String(),
						"args": json.RawMessage(block.Get("input").Raw),
					}
					fcJSON, _ := json.Marshal(fc)
					gc.Parts = append(gc.Parts, geminiPart{FunctionCall: fcJSON})
				case "tool_result":
					// Anthropic tool_result → Gemini functionResponse
					fr := map[string]interface{}{
						"name": block.Get("tool_use_id").String(),
						"response": map[string]interface{}{
							"content": block.Get("content").Value(),
						},
					}
					frJSON, _ := json.Marshal(fr)
					gc.Parts = append(gc.Parts, geminiPart{FunctionResponse: frJSON})
				}
			}
		}
		if len(gc.Parts) > 0 {
			out = append(out, gc)
		}
	}
	return out, nil
}

// antigravityStableSessionID derives a stable session ID from the first user message
// text — matching the CLIProxyAPI generateStableSessionID implementation.
func antigravityStableSessionID(body []byte) string {
	msgs := gjson.GetBytes(body, "messages")
	if msgs.IsArray() {
		for _, m := range msgs.Array() {
			if m.Get("role").String() == "user" {
				text := ""
				content := m.Get("content")
				switch content.Type {
				case gjson.String:
					text = content.String()
				case gjson.JSON:
					for _, b := range content.Array() {
						if b.Get("type").String() == "text" {
							text = b.Get("text").String()
							break
						}
					}
				}
				if text != "" {
					h := sha256.Sum256([]byte(text))
					n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
					return fmt.Sprintf("-%d", n)
				}
			}
		}
	}
	return fmt.Sprintf("-%d", time.Now().UnixNano()&0x7FFFFFFFFFFFFFFF)
}

// AntigravityChunk is a parsed Gemini streaming response chunk translated to
// a minimal Anthropic-compatible form for downstream writing.
type AntigravityChunk struct {
	Text         string
	FunctionCall *AntigravityFunctionCall
	StopReason   string   // "end_turn", "tool_use", "max_tokens", ""
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64 // usageMetadata.cachedContentTokenCount; >0 means cache hit
	IsLast       bool
}

type AntigravityFunctionCall struct {
	Name  string
	Args  json.RawMessage
	Index int
}

// ParseAntigravitySSELine parses one "data:" SSE line from an Antigravity stream
// into an AntigravityChunk. Returns (nil, nil) for non-data lines.
func ParseAntigravitySSELine(line string) (*AntigravityChunk, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return nil, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" || payload == "" {
		return &AntigravityChunk{IsLast: true}, nil
	}
	return parseAntigravityChunk([]byte(payload))
}

func parseAntigravityChunk(payload []byte) (*AntigravityChunk, error) {
	r := gjson.ParseBytes(payload)
	chunk := &AntigravityChunk{}

	// usageMetadata at top level
	if usage := r.Get("usageMetadata"); usage.Exists() {
		chunk.InputTokens = usage.Get("promptTokenCount").Int()
		chunk.OutputTokens = usage.Get("candidatesTokenCount").Int()
		chunk.CachedTokens = usage.Get("cachedContentTokenCount").Int()
		chunk.IsLast = true // usage chunk is terminal
	}

	candidates := r.Get("candidates")
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		return chunk, nil
	}
	cand := candidates.Array()[0]

	// finishReason
	switch strings.ToUpper(cand.Get("finishReason").String()) {
	case "STOP", "":
		if chunk.IsLast {
			chunk.StopReason = "end_turn"
		}
	case "MAX_TOKENS":
		chunk.StopReason = "max_tokens"
		chunk.IsLast = true
	case "SAFETY", "OTHER", "FINISH_REASON_UNSPECIFIED":
		chunk.StopReason = "end_turn"
		chunk.IsLast = true
	default:
		if cand.Get("finishReason").String() != "" {
			chunk.StopReason = "end_turn"
			chunk.IsLast = true
		}
	}

	content := cand.Get("content")
	parts := content.Get("parts")
	if !parts.IsArray() {
		return chunk, nil
	}
	for i, part := range parts.Array() {
		if text := part.Get("text").String(); text != "" {
			chunk.Text += text
		}
		if fc := part.Get("functionCall"); fc.Exists() {
			chunk.FunctionCall = &AntigravityFunctionCall{
				Name:  fc.Get("name").String(),
				Args:  []byte(fc.Get("args").Raw),
				Index: i,
			}
			if chunk.StopReason == "" {
				chunk.StopReason = "tool_use"
			}
		}
	}
	return chunk, nil
}

// AntigravityStreamToAnthropic reads an Antigravity SSE stream and writes Anthropic-
// compatible SSE events to w. Returns the final usage (input/output/cached tokens).
// The msgID is used as the Anthropic message id (msg_*) for the downstream response.
func AntigravityStreamToAnthropic(ctx context.Context, body io.Reader, w io.Writer,
	model, msgID string) (inputTok, outputTok, cachedTok int64, stopReason string, err error) {

	stopReason = "end_turn"
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	// message_start
	msgStart := fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[],"model":%q,"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		msgID, model)
	if _, werr := fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", msgStart); werr != nil {
		return 0, 0, 0, "", werr
	}
	// content_block_start for index 0 (text block)
	if _, werr := fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"); werr != nil {
		return 0, 0, 0, "", werr
	}
	if _, werr := fmt.Fprintf(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); werr != nil {
		return 0, 0, 0, "", werr
	}

	toolIndex := -1 // tracks if we've opened a tool-use block
	for scanner.Scan() {
		if ctx.Err() != nil {
			return inputTok, outputTok, cachedTok, stopReason, ctx.Err()
		}
		line := scanner.Text()
		chunk, parseErr := ParseAntigravitySSELine(line)
		if parseErr != nil || chunk == nil {
			continue
		}
		if chunk.InputTokens > 0 {
			inputTok = chunk.InputTokens
		}
		if chunk.OutputTokens > 0 {
			outputTok = chunk.OutputTokens
		}
		if chunk.CachedTokens > 0 {
			cachedTok = chunk.CachedTokens
		}
		if chunk.StopReason != "" {
			stopReason = chunk.StopReason
		}

		// Text delta
		if chunk.Text != "" {
			textJSON, _ := json.Marshal(chunk.Text)
			if _, werr := fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", textJSON); werr != nil {
				return inputTok, outputTok, cachedTok, stopReason, werr
			}
		}

		// Function call
		if fc := chunk.FunctionCall; fc != nil && toolIndex < 0 {
			toolIndex = 1 // tool block at index 1
			fcNameJSON, _ := json.Marshal(fc.Name)
			if _, werr := fmt.Fprintf(w,
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":%q,\"name\":%s,\"input\":{}}}\n\n",
				"toolu_"+strings.ReplaceAll(uuid.NewString(), "-", "")[:16], fcNameJSON,
			); werr != nil {
				return inputTok, outputTok, cachedTok, stopReason, werr
			}
			argsJSON, _ := json.Marshal(string(fc.Args))
			if _, werr := fmt.Fprintf(w,
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n",
				argsJSON,
			); werr != nil {
				return inputTok, outputTok, cachedTok, stopReason, werr
			}
		}

		if chunk.IsLast {
			break
		}
	}
	if serr := scanner.Err(); serr != nil {
		err = serr
	}

	// Close open blocks.
	if _, werr := fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"); werr != nil {
		return inputTok, outputTok, cachedTok, stopReason, werr
	}
	if toolIndex >= 0 {
		if _, werr := fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"); werr != nil {
			return inputTok, outputTok, cachedTok, stopReason, werr
		}
	}

	// message_delta with final usage
	if _, werr := fmt.Fprintf(w,
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n",
		stopReason, outputTok,
	); werr != nil {
		return inputTok, outputTok, cachedTok, stopReason, werr
	}
	// message_stop
	if _, werr := fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"); werr != nil {
		return inputTok, outputTok, cachedTok, stopReason, werr
	}
	return inputTok, outputTok, cachedTok, stopReason, err
}

// ParseAntigravityNonStream reads a complete (non-streaming) Antigravity JSON response
// and returns it as an AntigravityChunk.
func ParseAntigravityNonStream(body io.Reader) (*AntigravityChunk, error) {
	data, err := io.ReadAll(io.LimitReader(body, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	// Non-stream responses may be wrapped in {"response":{...},"traceId":"..."}.
	payload := data
	if inner := gjson.GetBytes(data, "response"); inner.Exists() {
		payload = []byte(inner.Raw)
	}
	return parseAntigravityChunk(payload)
}

// AntigravityChunkToAnthropicJSON serialises a single AntigravityChunk as an
// Anthropic /v1/messages non-stream JSON response.
func AntigravityChunkToAnthropicJSON(chunk *AntigravityChunk, model, msgID string) []byte {
	if chunk == nil {
		chunk = &AntigravityChunk{StopReason: "end_turn"}
	}
	stopReason := chunk.StopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	var content []contentBlock
	if chunk.Text != "" {
		content = append(content, contentBlock{Type: "text", Text: chunk.Text})
	}
	if len(content) == 0 {
		content = []contentBlock{{Type: "text", Text: ""}}
	}

	type usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	type msg struct {
		ID           string         `json:"id"`
		Type         string         `json:"type"`
		Role         string         `json:"role"`
		Content      []contentBlock `json:"content"`
		Model        string         `json:"model"`
		StopReason   string         `json:"stop_reason"`
		StopSequence interface{}    `json:"stop_sequence"`
		Usage        usage          `json:"usage"`
	}
	out, _ := json.Marshal(msg{
		ID:           msgID,
		Type:         "message",
		Role:         "assistant",
		Content:      content,
		Model:        model,
		StopReason:   stopReason,
		StopSequence: nil,
		Usage:        usage{InputTokens: chunk.InputTokens, OutputTokens: chunk.OutputTokens},
	})
	return out
}

// ── Explicit cache helpers ────────────────────────────────────────────────────

// antigravityCacheCreateEndpoint is the Vertex AI endpoint used for CachedContent CRUD.
// The Antigravity OAuth token has Vertex AI scopes, so we create caches there and
// reference them via the top-level request.cachedContent field during inference.
const antigravityCacheCreateEndpoint = "https://us-central1-aiplatform.googleapis.com"

// AntigravityCacheCreateRequest is the input to CreateAntigravityCachedContent.
type AntigravityCacheCreateRequest struct {
	AccessToken  string
	ProjectID    string
	Model        string // Gemini model slug, e.g. "gemini-2.5-pro"
	SystemText   string
	PrefixTurns  []geminiContent // history turns to cache (all but the final user turn)
	TTLSeconds   int64           // 0 → default 3600
}

// AntigravityCacheCreateResponse is the minimal subset of the Vertex AI
// CachedContent response that we need.
type AntigravityCacheCreateResponse struct {
	Name        string `json:"name"`       // "projects/.../cachedContents/xxx"
	ExpiresAt   int64  // Unix seconds parsed from expireTime
	TotalTokens int64  // usageMetadata.totalTokenCount
}

// CreateAntigravityCachedContent calls the Vertex AI cachedContents API to create
// an explicit cache resource for the given prefix. Returns the resource name and
// expiry so the caller can persist an AntigravityCacheEntry.
func CreateAntigravityCachedContent(ctx context.Context, req AntigravityCacheCreateRequest) (AntigravityCacheCreateResponse, error) {
	if req.ProjectID == "" {
		return AntigravityCacheCreateResponse{}, fmt.Errorf("project_id required for cache creation")
	}
	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = 3600
	}

	// Vertex AI requires the full publisher model path.
	model := req.Model
	if !strings.Contains(model, "/") {
		model = fmt.Sprintf("projects/%s/locations/us-central1/publishers/google/models/%s", req.ProjectID, req.Model)
	}

	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	type createReq struct {
		Model             string    `json:"model"`
		DisplayName       string    `json:"displayName,omitempty"`
		SystemInstruction *content  `json:"systemInstruction,omitempty"`
		Contents          []content `json:"contents,omitempty"`
		TTL               string    `json:"ttl"`
	}
	body := createReq{
		Model: model,
		TTL:   fmt.Sprintf("%ds", ttl),
	}
	if req.SystemText != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: req.SystemText}}}
	}
	for _, turn := range req.PrefixTurns {
		var parts []part
		for _, p := range turn.Parts {
			if p.Text != "" {
				parts = append(parts, part{Text: p.Text})
			}
		}
		if len(parts) > 0 {
			body.Contents = append(body.Contents, content{Role: turn.Role, Parts: parts})
		}
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return AntigravityCacheCreateResponse{}, err
	}

	url := fmt.Sprintf("%s/v1/projects/%s/locations/us-central1/cachedContents",
		antigravityCacheCreateEndpoint, req.ProjectID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return AntigravityCacheCreateResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.AccessToken)

	client := &http.Client{Transport: antigravityTransport, Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return AntigravityCacheCreateResponse{}, fmt.Errorf("cache create request: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AntigravityCacheCreateResponse{}, fmt.Errorf("cache create HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	name := gjson.GetBytes(respBytes, "name").String()
	if name == "" {
		return AntigravityCacheCreateResponse{}, fmt.Errorf("cache create: empty name in response: %s", string(respBytes))
	}
	totalTok := gjson.GetBytes(respBytes, "usageMetadata.totalTokenCount").Int()

	// Parse expireTime RFC3339 → Unix seconds.
	var expiresAt int64
	if et := gjson.GetBytes(respBytes, "expireTime").String(); et != "" {
		if t, tErr := time.Parse(time.RFC3339, et); tErr == nil {
			expiresAt = t.Unix()
		}
	}
	if expiresAt == 0 {
		expiresAt = time.Now().Unix() + ttl
	}
	return AntigravityCacheCreateResponse{Name: name, ExpiresAt: expiresAt, TotalTokens: totalTok}, nil
}

// ExtractAntigravityPrefixForCache splits an Anthropic request body into the stable
// prefix (system + history turns) suitable for a CachedContent resource, and the
// single new user turn to include in the actual inference request.
//
// Returns:
//   - prefixTurns: all converted Gemini turns except the last user turn
//   - lastTurns:   just the final user turn as a slice (for request.contents)
//   - systemText:  extracted system prompt text
//   - convKeyHash: FNV-64a hex fingerprint over systemText+prefix turns (stable across
//     multiple requests in the same conversation)
//
// approxChars is the total character count of the prefix — used by callers to
// decide whether the prefix is large enough to be worth caching.
func ExtractAntigravityPrefixForCache(body []byte) (prefixTurns []geminiContent, lastTurns []geminiContent, systemText string, convKeyHash string) {
	// Extract system prompt.
	if sys := gjson.GetBytes(body, "system"); sys.Exists() {
		switch sys.Type {
		case gjson.String:
			systemText = sys.String()
		case gjson.JSON:
			for _, b := range sys.Array() {
				if b.Get("type").String() == "text" {
					systemText += b.Get("text").String()
				}
			}
		}
	}

	all, err := anthropicMessagesToGeminiContents(body)
	if err != nil || len(all) == 0 {
		return nil, all, systemText, antigravityConvHash(systemText, nil)
	}

	// Prefix = everything except the last turn.
	// Last turn = the final element (should be the new user message).
	last := all[len(all)-1:]
	prefix := all[:len(all)-1]
	return prefix, last, systemText, antigravityConvHash(systemText, prefix)
}

// ApproxAntigravityPrefixChars returns the approximate character count of the stable
// prefix (system + history). Used to gate cache creation.
func ApproxAntigravityPrefixChars(body []byte) int {
	prefix, _, sys, _ := ExtractAntigravityPrefixForCache(body)
	n := len(sys)
	for _, c := range prefix {
		for _, p := range c.Parts {
			n += len(p.Text)
		}
	}
	return n
}

// antigravityConvHash computes a stable FNV-64a fingerprint over the system text and
// the prefix turns. Two requests with the same system + same history produce the same
// hash, allowing cache reuse across turns.
func antigravityConvHash(systemText string, prefixTurns []geminiContent) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(systemText))
	_, _ = h.Write([]byte{0x00})
	for _, turn := range prefixTurns {
		_, _ = h.Write([]byte(turn.Role))
		_, _ = h.Write([]byte{0x01})
		for _, p := range turn.Parts {
			_, _ = h.Write([]byte(p.Text))
			_, _ = h.Write([]byte{0x02})
		}
		_, _ = h.Write([]byte{0x03})
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// ── Claude-via-Vertex stream / non-stream parsers ─────────────────────────────
//
// Vertex AI's :streamRawPredict and :rawPredict endpoints return native Anthropic
// wire format (identical to the direct Anthropic API). We pass the bytes through
// unchanged and extract usage counters from the well-known event fields.

// AntigravityClaudeVertexStreamToAnthropic reads native Anthropic SSE from a Vertex
// AI :streamRawPredict response, writes each line to w unchanged, and returns the
// token counts extracted from message_start/message_delta usage events.
//
// Signature is identical to AntigravityStreamToAnthropic so the API layer can call
// either function through the same code path.
func AntigravityClaudeVertexStreamToAnthropic(ctx context.Context, body io.Reader, w io.Writer,
	_, _ string) (inputTok, outputTok, cachedTok int64, stopReason string, err error) {

	stopReason = "end_turn"
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 512*1024), 512*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return inputTok, outputTok, cachedTok, stopReason, ctx.Err()
		}
		line := scanner.Bytes()
		if _, werr := w.Write(line); werr != nil {
			return inputTok, outputTok, cachedTok, stopReason, werr
		}
		if _, werr := w.Write([]byte{'\n'}); werr != nil {
			return inputTok, outputTok, cachedTok, stopReason, werr
		}

		// Extract usage from native Anthropic SSE events.
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := line[6:]
		switch gjson.GetBytes(data, "type").String() {
		case "message_start":
			// message_start carries input_tokens and cache counters.
			if v := gjson.GetBytes(data, "message.usage.input_tokens"); v.Exists() {
				inputTok = v.Int()
			}
			if v := gjson.GetBytes(data, "message.usage.cache_read_input_tokens"); v.Exists() {
				cachedTok = v.Int()
			}
		case "message_delta":
			// message_delta carries output_tokens and the final stop_reason.
			if v := gjson.GetBytes(data, "usage.output_tokens"); v.Exists() {
				outputTok = v.Int()
			}
			if v := gjson.GetBytes(data, "delta.stop_reason"); v.Exists() && v.String() != "" {
				stopReason = v.String()
			}
		}
	}
	return inputTok, outputTok, cachedTok, stopReason, scanner.Err()
}

// AntigravityClaudeVertexNonStreamResult holds the parsed result from a Vertex AI
// :rawPredict Claude response. RawBody is the original Anthropic JSON; usage fields
// are extracted separately for billing.
type AntigravityClaudeVertexNonStreamResult struct {
	RawBody      []byte
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64
	StopReason   string
}

// ParseAntigravityClaudeVertexNonStream reads a Vertex AI :rawPredict response body
// (native Anthropic JSON) and returns the raw bytes together with the extracted usage
// counters. The RawBody is forwarded as-is to the downstream client, preserving all
// fields (thinking blocks, tool calls, etc.) that AntigravityChunkToAnthropicJSON
// would otherwise lose.
func ParseAntigravityClaudeVertexNonStream(body io.Reader) (AntigravityClaudeVertexNonStreamResult, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 4*1024*1024))
	if err != nil {
		return AntigravityClaudeVertexNonStreamResult{}, err
	}
	var res AntigravityClaudeVertexNonStreamResult
	res.RawBody = raw
	res.InputTokens = gjson.GetBytes(raw, "usage.input_tokens").Int()
	res.OutputTokens = gjson.GetBytes(raw, "usage.output_tokens").Int()
	res.CachedTokens = gjson.GetBytes(raw, "usage.cache_read_input_tokens").Int()
	res.StopReason = gjson.GetBytes(raw, "stop_reason").String()
	if res.StopReason == "" {
		res.StopReason = "end_turn"
	}
	return res, nil
}

// AntigravityClaudeVertexIsCacheRejected reports whether an HTTP 400/422 response
// body indicates that the upstream Vertex AI endpoint rejected a cache_control field.
// When true, the caller should strip cache_control from the request and retry once.
func AntigravityClaudeVertexIsCacheRejected(statusCode int, respBody []byte) bool {
	if statusCode != http.StatusBadRequest && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	low := strings.ToLower(string(respBody))
	return strings.Contains(low, "cache_control") || strings.Contains(low, "caching")
}
