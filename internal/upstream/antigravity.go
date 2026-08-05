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
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityDailyBaseURL = "https://daily-cloudcode-pa.googleapis.com"
	antigravityProdBaseURL  = "https://cloudcode-pa.googleapis.com"
	antigravityStreamPath   = "/v1internal:streamGenerateContent?alt=sse"
	antigravityGeneratePath = "/v1internal:generateContent"
	antigravityDefaultUA    = "antigravity/hub/2.2.1 darwin/arm64"
	antigravityOAuthUA      = "Go-http-client/2.0"
	// refreshSkew: token is considered expired 3000s early — mirrors CLIProxyAPI source.
	antigravityRefreshSkew = 3000
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

type AntigravityModel struct {
	ID                  string
	DisplayName         string
	MaxTokens           int64
	MaxCompletionTokens int64
	RawJSON             []byte
}

type antigravityRawRequest func(context.Context, string, http.Header, []byte) (*Response, error)

func directAntigravityRawRequest(timeout time.Duration) antigravityRawRequest {
	client := &http.Client{Transport: antigravityTransport, Timeout: timeout}
	return func(ctx context.Context, target string, headers http.Header, body []byte) (*Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		return &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: resp.Body}, nil
	}
}

// FetchAntigravityModels returns the account-scoped model catalog exposed by
// Cloud Code Assist. Standalone callers use the direct HTTP/1.1 transport.
func FetchAntigravityModels(ctx context.Context, accessToken, projectID, baseURL, userAgent string) ([]AntigravityModel, error) {
	return fetchAntigravityModels(ctx, accessToken, projectID, baseURL, userAgent, directAntigravityRawRequest(30*time.Second))
}

// FetchAntigravityModels performs account discovery through the same outlet used
// for inference, preventing model probes from leaking the host's direct IP.
func (c *Client) FetchAntigravityModels(ctx context.Context, egress storage.EgressProfile, cookieJarKey, accessToken, projectID, baseURL, userAgent string) ([]AntigravityModel, error) {
	return fetchAntigravityModels(ctx, accessToken, projectID, baseURL, userAgent, func(ctx context.Context, target string, headers http.Header, body []byte) (*Response, error) {
		return c.DoRawHTTP1(ctx, egress, http.MethodPost, target, headers, body, cookieJarKey)
	})
}

func fetchAntigravityModels(ctx context.Context, accessToken, projectID, baseURL, userAgent string, do antigravityRawRequest) ([]AntigravityModel, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, errors.New("antigravity model probe requires an access token")
	}
	bases := []string{}
	if custom := strings.TrimRight(strings.TrimSpace(baseURL), "/"); custom != "" {
		bases = append(bases, custom)
	} else {
		bases = append(bases, antigravityDailyBaseURL, antigravityProdBaseURL)
	}
	payload := map[string]string{}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		payload["project"] = projectID
	}
	body, _ := json.Marshal(payload)
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		ua = antigravityDefaultUA
	}
	var lastErr error
	for _, base := range bases {
		headers := make(http.Header)
		headers.Set("Authorization", "Bearer "+accessToken)
		headers.Set("Content-Type", "application/json")
		headers.Set("User-Agent", ua)
		resp, err := do(ctx, base+"/v1internal:fetchAvailableModels", headers, body)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("antigravity model probe returned HTTP %d", resp.StatusCode)
			continue
		}
		modelsNode := gjson.GetBytes(respBody, "models")
		if !modelsNode.IsObject() {
			if gjson.GetBytes(respBody, "webSearchModelIds").IsArray() {
				lastErr = errors.New("antigravity model probe returned capability hints without a model catalog")
			} else {
				lastErr = errors.New("antigravity model probe response is missing models")
			}
			continue
		}
		modelMap := modelsNode.Map()
		if len(modelMap) == 0 {
			lastErr = errors.New("antigravity model probe returned an empty non-authoritative catalog")
			continue
		}
		ids := make([]string, 0, len(modelMap))
		for id := range modelMap {
			id = strings.TrimSpace(id)
			if id != "" && !isAntigravityInternalModel(id) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		models := make([]AntigravityModel, 0, len(ids))
		for _, id := range ids {
			entry := modelMap[id]
			models = append(models, AntigravityModel{
				ID:                  id,
				DisplayName:         firstNonEmptyString(entry.Get("displayName").String(), id),
				MaxTokens:           entry.Get("maxTokens").Int(),
				MaxCompletionTokens: entry.Get("maxOutputTokens").Int(),
				RawJSON:             []byte(entry.Raw),
			})
		}
		return models, nil
	}
	if lastErr == nil {
		lastErr = errors.New("antigravity model probe failed")
	}
	return nil, lastErr
}

func isAntigravityInternalModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview", "gemini-2.5-flash-thinking", "gemini-2.5-pro":
		return true
	default:
		return false
	}
}

// RefreshAntigravityToken exchanges a refresh token for a new access token.
// Returns the new access token, updated refresh token, and expiry (Unix seconds).
func RefreshAntigravityToken(ctx context.Context, refreshToken string, cfg *config.Config) (AntigravityTokenResponse, error) {
	return refreshAntigravityToken(ctx, refreshToken, cfg, directAntigravityRawRequest(30*time.Second))
}

// RefreshAntigravityToken refreshes account credentials through the selected
// account outlet. OAuth and inference therefore share the same source IP.
func (c *Client) RefreshAntigravityToken(ctx context.Context, egress storage.EgressProfile, cookieJarKey, refreshToken string, cfg *config.Config) (AntigravityTokenResponse, error) {
	return refreshAntigravityToken(ctx, refreshToken, cfg, func(ctx context.Context, target string, headers http.Header, body []byte) (*Response, error) {
		return c.DoRawHTTP1(ctx, egress, http.MethodPost, target, headers, body, cookieJarKey)
	})
}

func refreshAntigravityToken(ctx context.Context, refreshToken string, cfg *config.Config, do antigravityRawRequest) (AntigravityTokenResponse, error) {
	if cfg == nil {
		return AntigravityTokenResponse{}, errors.New("antigravity OAuth config is required")
	}
	form := url.Values{
		"client_id":     []string{cfg.AntigravityOAuthClientID},
		"client_secret": []string{cfg.AntigravityOAuthClientSecret},
		"grant_type":    []string{"refresh_token"},
		"refresh_token": []string{refreshToken},
	}.Encode()
	headers := make(http.Header)
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	headers.Set("User-Agent", antigravityOAuthUA)
	resp, err := do(ctx, cfg.AntigravityOAuthTokenURL, headers, []byte(form))
	if err != nil {
		return AntigravityTokenResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return AntigravityTokenResponse{}, fmt.Errorf("antigravity token refresh failed with status %d", resp.StatusCode)
	}
	var tr AntigravityTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return AntigravityTokenResponse{}, err
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return AntigravityTokenResponse{}, errors.New("antigravity token refresh response missing access_token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 3600
	}
	return tr, nil
}

// AntigravityRequest is the pool-side representation of a single Antigravity inference call.
type AntigravityRequest struct {
	AccountID   string // account-scoped salt for stable session isolation
	AccessToken string
	ProjectID   string
	Model       string // Gemini or Claude model slug
	BaseURL     string // empty → production
	UserAgent   string // empty → default UA
	Body        []byte // Anthropic-format request JSON
	Stream      bool
	// MaxOutputTokens is the account catalog's exact-model output ceiling. Zero
	// means the catalog did not publish a limit.
	MaxOutputTokens int64
}

// AntigravityConversionError identifies a downstream request that cannot be
// represented by the Antigravity wire protocol without losing semantics. API
// handlers must return this as a local 422 and must not retry another account.
type AntigravityConversionError struct {
	Reason string
}

func (e *AntigravityConversionError) Error() string {
	return e.Reason
}

func antigravityConversionError(format string, args ...interface{}) error {
	return &AntigravityConversionError{Reason: fmt.Sprintf(format, args...)}
}

func IsAntigravityConversionError(err error) bool {
	var target *AntigravityConversionError
	return errors.As(err, &target)
}

type AntigravityFailureClass string

const (
	AntigravityFailureNone      AntigravityFailureClass = "none"
	AntigravityFailureAuth      AntigravityFailureClass = "auth"
	AntigravityFailureRateLimit AntigravityFailureClass = "rate_limit"
	AntigravityFailureCapacity  AntigravityFailureClass = "capacity"
	AntigravityFailureTransient AntigravityFailureClass = "transient"
	AntigravityFailurePermanent AntigravityFailureClass = "permanent_4xx"
)

// AntigravityFailure describes the provider decision without mutating account
// capability state. Retryable is a bounded same-account wire retry; Failover is
// an account-level retry after those attempts are exhausted.
type AntigravityFailure struct {
	Class     AntigravityFailureClass
	Status    int
	Retryable bool
	Failover  bool
}

// AntigravityUpstreamError preserves an error embedded in an HTTP-200 SSE event
// so it enters the same classifier as ordinary HTTP failures.
type AntigravityUpstreamError struct {
	StatusCode int
	Body       []byte
}

func (e *AntigravityUpstreamError) Error() string {
	if e == nil {
		return "antigravity upstream error"
	}
	return fmt.Sprintf("antigravity upstream error (status %d)", e.StatusCode)
}

func AsAntigravityUpstreamError(err error) (*AntigravityUpstreamError, bool) {
	var target *AntigravityUpstreamError
	if !errors.As(err, &target) {
		return nil, false
	}
	return target, true
}

// ClassifyAntigravityFailure is shared by HTTP and SSE failures. Permanent
// request/model 4xx responses are not retried or converted into capability loss.
func ClassifyAntigravityFailure(status int, header http.Header, body []byte, requestErr error) AntigravityFailure {
	if embedded, ok := AsAntigravityUpstreamError(requestErr); ok {
		status = embedded.StatusCode
		body = embedded.Body
		requestErr = nil
	}
	if requestErr != nil || status == 0 {
		return AntigravityFailure{Class: AntigravityFailureTransient, Status: status, Retryable: true, Failover: true}
	}
	if status >= 200 && status < 300 {
		return AntigravityFailure{Class: AntigravityFailureNone, Status: status}
	}
	switch status {
	case http.StatusUnauthorized:
		return AntigravityFailure{Class: AntigravityFailureAuth, Status: status, Failover: true}
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly:
		return AntigravityFailure{Class: AntigravityFailureTransient, Status: status, Retryable: true, Failover: true}
	case http.StatusTooManyRequests:
		lower := strings.ToLower(string(body))
		soft := header.Get("Retry-After") != "" || strings.Contains(lower, "resource_exhausted") ||
			strings.Contains(lower, "resource exhausted") || strings.Contains(lower, "no capacity") ||
			strings.Contains(lower, "no_capacity") || strings.Contains(lower, "temporar") ||
			strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests")
		return AntigravityFailure{Class: AntigravityFailureRateLimit, Status: status, Retryable: soft, Failover: true}
	}
	if status >= 500 && status <= 599 {
		class := AntigravityFailureTransient
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "resource exhausted") ||
			strings.Contains(lower, "no capacity") || strings.Contains(lower, "no_capacity") {
			class = AntigravityFailureCapacity
		}
		return AntigravityFailure{Class: class, Status: status, Retryable: true, Failover: true}
	}
	if status >= 400 && status <= 499 {
		return AntigravityFailure{Class: AntigravityFailurePermanent, Status: status}
	}
	return AntigravityFailure{Class: AntigravityFailureTransient, Status: status, Retryable: true, Failover: true}
}

// AntigravityIsClaudeModel reports whether the model uses Claude semantics. Both
// Claude and Gemini are sent through Antigravity's v1internal endpoint; the flag is
// used only for model-family policy and cache behavior.
func AntigravityIsClaudeModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude")
}

// AntigravityEndpointBases returns the native endpoint order. Account-specific
// custom endpoints are deliberately never combined with the public endpoints.
func AntigravityEndpointBases(baseURL string) []string {
	if custom := strings.TrimRight(strings.TrimSpace(baseURL), "/"); custom != "" {
		return []string{custom}
	}
	return []string{antigravityDailyBaseURL, antigravityProdBaseURL}
}

func buildAntigravityRequest(req AntigravityRequest) (string, http.Header, []byte, error) {
	base := AntigravityEndpointBases(req.BaseURL)[0]

	// Convert Anthropic request body → Antigravity wire format.
	agBody, err := anthropicToAntigravityForAccount(req.Body, req.Model, req.ProjectID, req.AccountID, req.MaxOutputTokens)
	if err != nil {
		return "", nil, nil, err
	}

	var target string
	if req.Stream {
		target = base + antigravityStreamPath
	} else {
		target = base + antigravityGeneratePath
	}

	ua := antigravityDefaultUA
	if u := strings.TrimSpace(req.UserAgent); u != "" {
		ua = u
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+req.AccessToken)
	headers.Set("User-Agent", ua)
	return target, headers, agBody, nil
}

// DoAntigravity sends a request through the scheduler-selected egress while
// retaining Antigravity's HTTP/1.1-only wire profile.
func (c *Client) DoAntigravity(ctx context.Context, egress storage.EgressProfile, cookieJarKey string, req AntigravityRequest) (*Response, error) {
	bases := AntigravityEndpointBases(req.BaseURL)
	var lastResp *Response
	var lastBody []byte
	var lastErr error
	for index, base := range bases {
		wireReq := req
		wireReq.BaseURL = base
		target, headers, body, err := buildAntigravityRequest(wireReq)
		if err != nil {
			return nil, err
		}
		resp, requestErr := c.DoRawHTTP1(ctx, egress, http.MethodPost, target, headers, body, cookieJarKey)
		if requestErr != nil {
			lastErr = requestErr
			if index+1 < len(bases) {
				continue
			}
			return nil, requestErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		responseBody, readErr := DrainAndClose(resp.Body)
		if readErr != nil {
			lastErr = readErr
			if index+1 < len(bases) {
				continue
			}
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))
		lastResp, lastBody = resp, responseBody
		failure := ClassifyAntigravityFailure(resp.StatusCode, resp.Header, responseBody, nil)
		if !failure.Retryable || index+1 == len(bases) {
			return resp, nil
		}
	}
	if lastResp != nil {
		lastResp.Body = io.NopCloser(bytes.NewReader(lastBody))
		return lastResp, nil
	}
	return nil, lastErr
}

// DoAntigravity remains available to standalone adapter callers and focused
// conversion tests. Production API traffic uses Client.DoAntigravity above.
func DoAntigravity(ctx context.Context, req AntigravityRequest) (*http.Response, error) {
	client := &http.Client{Transport: antigravityTransport, Timeout: 5 * time.Minute}
	bases := AntigravityEndpointBases(req.BaseURL)
	var lastErr error
	for index, base := range bases {
		wireReq := req
		wireReq.BaseURL = base
		target, headers, body, err := buildAntigravityRequest(wireReq)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header = headers
		resp, requestErr := client.Do(httpReq)
		if requestErr != nil {
			lastErr = requestErr
			if index+1 < len(bases) {
				continue
			}
			return nil, requestErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		resp.Body = io.NopCloser(bytes.NewReader(responseBody))
		failure := ClassifyAntigravityFailure(resp.StatusCode, resp.Header, responseBody, nil)
		if !failure.Retryable || index+1 == len(bases) {
			return resp, nil
		}
	}
	return nil, lastErr
}

// anthropicToAntigravity converts an Anthropic /v1/messages JSON body to the
// Antigravity wire format: outer Antigravity envelope wrapping a Gemini inner request.
func anthropicToAntigravity(body []byte, model, projectID string) ([]byte, error) {
	return anthropicToAntigravityForAccount(body, model, projectID, "", 0)
}

func anthropicToAntigravityForAccount(body []byte, model, projectID, accountID string, maxOutputTokens int64) ([]byte, error) {
	if !json.Valid(body) {
		return nil, antigravityConversionError("request body is not valid JSON")
	}
	if strings.TrimSpace(model) == "" {
		return nil, antigravityConversionError("request model is required")
	}
	isClaudeModel := AntigravityIsClaudeModel(model)
	contents, err := anthropicMessagesToGeminiContents(body, model)
	if err != nil {
		return nil, err
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
	out, _ = sjson.SetBytes(out, "requestId", uuid.NewString())
	out, _ = sjson.SetRawBytes(out, "request.contents", contentsJSON)
	out, _ = sjson.SetBytes(out, "request.sessionId", antigravityStableSessionID(body, accountID))

	// 3. Forward generationConfig fields.
	if mt := gjson.GetBytes(body, "max_tokens"); mt.Exists() {
		requested := mt.Int()
		if maxOutputTokens > 0 && (requested <= 0 || requested > maxOutputTokens) {
			requested = maxOutputTokens
		}
		if requested > 0 {
			out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", requested)
		}
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
	if stops := gjson.GetBytes(body, "stop_sequences"); stops.Exists() {
		if !stops.IsArray() {
			return nil, antigravityConversionError("stop_sequences must be an array")
		}
		values := make([]string, 0, len(stops.Array()))
		for _, stop := range stops.Array() {
			if stop.Type != gjson.String {
				return nil, antigravityConversionError("stop_sequences entries must be strings")
			}
			values = append(values, stop.String())
		}
		out, _ = sjson.SetBytes(out, "request.generationConfig.stopSequences", values)
	}
	if thinking := gjson.GetBytes(body, "thinking"); thinking.Exists() {
		if !thinking.IsObject() {
			return nil, antigravityConversionError("thinking must be an object")
		}
		switch thinking.Get("type").String() {
		case "enabled":
			budget := thinking.Get("budget_tokens")
			if !budget.Exists() || budget.Type != gjson.Number || budget.Int() <= 0 {
				return nil, antigravityConversionError("enabled thinking requires a positive budget_tokens")
			}
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget", budget.Int())
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts", true)
		case "adaptive", "auto":
			effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String()))
			if effort == "" {
				effort = "high"
			}
			switch effort {
			case "low", "medium", "high":
			default:
				return nil, antigravityConversionError("unsupported adaptive thinking effort %q", effort)
			}
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel", effort)
			out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts", true)
		case "disabled":
		default:
			return nil, antigravityConversionError("unsupported thinking type %q", thinking.Get("type").String())
		}
	}

	// 4. System prompt → systemInstruction.
	parts, err := anthropicSystemToGeminiParts(body)
	if err != nil {
		return nil, err
	}
	if len(parts) > 0 {
		partsJSON, _ := json.Marshal(parts)
		out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
		out, _ = sjson.SetRawBytes(out, "request.systemInstruction.parts", partsJSON)
	}

	// 5. Tools → functionDeclarations.
	hasFunctionTools := false
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		funcDecls := make([]json.RawMessage, 0, len(tools.Array()))
		for index, t := range tools.Array() {
			if typ := strings.TrimSpace(t.Get("type").String()); typ != "" && typ != "custom" {
				return nil, antigravityConversionError("tools[%d] type %q is not supported by Antigravity", index, typ)
			}
			name := strings.TrimSpace(t.Get("name").String())
			if name == "" {
				return nil, antigravityConversionError("tools[%d] is missing name", index)
			}
			inputSchema := t.Get("input_schema")
			if !inputSchema.IsObject() {
				return nil, antigravityConversionError("tools[%d].input_schema must be an object", index)
			}
			cleanSchema, schemaErr := sanitizeAntigravityToolSchema(inputSchema.Raw, isClaudeModel)
			if schemaErr != nil {
				return nil, antigravityConversionError("tools[%d].input_schema cannot be converted: %v", index, schemaErr)
			}
			declaration := map[string]interface{}{"name": name, "parameters": cleanSchema}
			if description := t.Get("description"); description.Type == gjson.String && description.String() != "" {
				declaration["description"] = description.String()
			}
			fdBytes, marshalErr := json.Marshal(declaration)
			if marshalErr != nil {
				return nil, antigravityConversionError("tools[%d] could not be serialized: %v", index, marshalErr)
			}
			funcDecls = append(funcDecls, json.RawMessage(fdBytes))
		}
		fdJSON, err := json.Marshal(funcDecls)
		if err != nil {
			return nil, antigravityConversionError("tools could not be serialized: %v", err)
		}
		out, _ = sjson.SetRawBytes(out, "request.tools.0.functionDeclarations", fdJSON)
		hasFunctionTools = len(funcDecls) > 0
	}
	if toolChoice := gjson.GetBytes(body, "tool_choice"); toolChoice.Exists() {
		if !toolChoice.IsObject() {
			return nil, antigravityConversionError("tool_choice must be an object")
		}
		switch typ := toolChoice.Get("type").String(); typ {
		case "auto":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "AUTO")
		case "none":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "NONE")
		case "any":
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "ANY")
		case "tool":
			name := strings.TrimSpace(toolChoice.Get("name").String())
			if name == "" {
				return nil, antigravityConversionError("tool_choice type tool requires name")
			}
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "ANY")
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.allowedFunctionNames", []string{name})
		default:
			return nil, antigravityConversionError("unsupported tool_choice type %q", typ)
		}
	}
	if isClaudeModel && hasFunctionTools {
		// Antigravity Claude validates tool arguments against the cleaned schema.
		// The real client overrides AUTO/ANY/NONE at the final transport boundary.
		out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
	}
	// context_management is an Anthropic request-side history policy. The Antigravity
	// envelope is rebuilt field-by-field above, so it is intentionally consumed only by
	// the local context/compaction pipeline and omitted from the final provider wire.
	// Rejecting it here caused Claude Code's automatic context management to fail with a
	// local 422 even though no unsupported field would have reached Antigravity.
	// Note: context_management is already omitted because we rebuild the envelope from scratch.

	// Check for unsupported fields that would reach the wire.
	if gjson.GetBytes(body, "mcp_servers").Exists() {
		return nil, antigravityConversionError("mcp_servers are not supported by the Antigravity protocol")
	}

	return out, nil
}

func anthropicSystemToGeminiParts(body []byte) ([]geminiPart, error) {
	parts, err := anthropicSystemContentToGeminiParts(gjson.GetBytes(body, "system"), "system")
	if err != nil {
		return nil, err
	}
	// Some gateway/client instruction layers use Chat-style system-role
	// messages. Antigravity has only systemInstruction, so hoist those blocks
	// instead of rejecting an otherwise losslessly representable request.
	for index, message := range gjson.GetBytes(body, "messages").Array() {
		if strings.TrimSpace(message.Get("role").String()) != "system" {
			continue
		}
		messageParts, partErr := anthropicSystemContentToGeminiParts(message.Get("content"), fmt.Sprintf("messages[%d].content", index))
		if partErr != nil {
			return nil, partErr
		}
		parts = append(parts, messageParts...)
	}
	return parts, nil
}

func anthropicSystemContentToGeminiParts(content gjson.Result, field string) ([]geminiPart, error) {
	if !content.Exists() {
		return nil, nil
	}
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil, nil
		}
		return []geminiPart{{Text: content.String()}}, nil
	}
	if !content.IsArray() {
		return nil, antigravityConversionError("%s must be a string or an array of text blocks", field)
	}
	parts := make([]geminiPart, 0, len(content.Array()))
	for index, block := range content.Array() {
		if block.Get("type").String() != "text" {
			return nil, antigravityConversionError("%s[%d] type %q cannot be converted", field, index, block.Get("type").String())
		}
		if text := block.Get("text").String(); text != "" {
			parts = append(parts, geminiPart{Text: text})
		}
	}
	return parts, nil
}

// geminiContent is a single Gemini content object.
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string          `json:"text,omitempty"`
	InlineData       json.RawMessage `json:"inlineData,omitempty"`
	FunctionCall     json.RawMessage `json:"functionCall,omitempty"`
	FunctionResponse json.RawMessage `json:"functionResponse,omitempty"`
	Thought          bool            `json:"thought,omitempty"`
	ThoughtSignature string          `json:"thoughtSignature,omitempty"`
}

// anthropicMessagesToGeminiContents converts an Anthropic messages array to
// the Gemini contents format.
func anthropicMessagesToGeminiContents(body []byte, model string) ([]geminiContent, error) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return nil, antigravityConversionError("anthropic body is missing messages array")
	}
	out := make([]geminiContent, 0, len(msgs.Array()))
	toolNameByID := make(map[string]string)
	for messageIndex, m := range msgs.Array() {
		role := strings.TrimSpace(m.Get("role").String())
		if role == "system" {
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, antigravityConversionError("messages[%d] has unsupported role %q", messageIndex, role)
		}
		// Anthropic "assistant" → Gemini "model"
		if role == "assistant" {
			role = "model"
		}
		gc := geminiContent{Role: role}
		content := m.Get("content")
		switch content.Type {
		case gjson.String:
			if content.String() != "" {
				gc.Parts = []geminiPart{{Text: content.String()}}
			}
		case gjson.JSON:
			if !content.IsArray() {
				return nil, antigravityConversionError("messages[%d].content must be a string or array", messageIndex)
			}
			for blockIndex, block := range content.Array() {
				typ := block.Get("type").String()
				switch typ {
				case "text":
					if text := block.Get("text").String(); text != "" {
						gc.Parts = append(gc.Parts, geminiPart{Text: text})
					}
				case "image":
					source := block.Get("source")
					if source.Get("type").String() != "base64" {
						return nil, antigravityConversionError("messages[%d].content[%d] image source type %q is not supported", messageIndex, blockIndex, source.Get("type").String())
					}
					mediaType := strings.TrimSpace(source.Get("media_type").String())
					data := strings.TrimSpace(source.Get("data").String())
					if mediaType == "" || data == "" {
						return nil, antigravityConversionError("messages[%d].content[%d] base64 image requires media_type and data", messageIndex, blockIndex)
					}
					inlineData, _ := json.Marshal(map[string]string{"mimeType": mediaType, "data": data})
					gc.Parts = append(gc.Parts, geminiPart{InlineData: inlineData})
				case "thinking":
					if role != "model" {
						return nil, antigravityConversionError("messages[%d].content[%d] thinking is only valid for assistant messages", messageIndex, blockIndex)
					}
					thinkingText := block.Get("thinking").String()
					signature, compatible := antigravityReplayThinkingSignature(model, block.Get("signature").String())
					if thinkingText == "" || !compatible {
						// Signed reasoning is provider-specific. Dropping an incompatible
						// historical block is safer than replaying it and failing the turn.
						continue
					}
					gc.Parts = append(gc.Parts, geminiPart{Text: thinkingText, Thought: true, ThoughtSignature: signature})
				case "redacted_thinking":
					return nil, antigravityConversionError("messages[%d].content[%d] redacted_thinking cannot be safely converted", messageIndex, blockIndex)
				case "tool_use":
					// Anthropic tool_use → Gemini functionCall
					if role != "model" {
						return nil, antigravityConversionError("messages[%d].content[%d] tool_use is only valid for assistant messages", messageIndex, blockIndex)
					}
					toolID := strings.TrimSpace(block.Get("id").String())
					toolName := strings.TrimSpace(block.Get("name").String())
					input := block.Get("input")
					inputRaw := input.Raw
					if input.Type == gjson.String {
						parsed := gjson.Parse(input.String())
						if parsed.IsObject() {
							inputRaw = parsed.Raw
						}
					}
					if toolID == "" || toolName == "" || !gjson.Parse(inputRaw).IsObject() {
						return nil, antigravityConversionError("messages[%d].content[%d] tool_use requires id, name, and object input", messageIndex, blockIndex)
					}
					if prior := toolNameByID[toolID]; prior != "" && prior != toolName {
						return nil, antigravityConversionError("tool_use id %q is associated with conflicting names", toolID)
					}
					toolNameByID[toolID] = toolName
					fc := map[string]interface{}{
						"name": toolName,
						"args": json.RawMessage(inputRaw),
						"id":   toolID,
					}
					for _, signaturePath := range []string{"signature", "thought_signature", "extra_content.google.thought_signature"} {
						if signature, ok := antigravityReplayToolSignature(model, block.Get(signaturePath).String()); ok {
							fcJSON, _ := json.Marshal(fc)
							gc.Parts = append(gc.Parts, geminiPart{FunctionCall: fcJSON, ThoughtSignature: signature})
							fc = nil
							break
						}
					}
					if fc != nil {
						fcJSON, _ := json.Marshal(fc)
						part := geminiPart{FunctionCall: fcJSON}
						if !AntigravityIsClaudeModel(model) {
							part.ThoughtSignature = "skip_thought_signature_validator"
						}
						gc.Parts = append(gc.Parts, part)
					}
				case "tool_result":
					// Anthropic tool_result → Gemini functionResponse
					if role != "user" {
						return nil, antigravityConversionError("messages[%d].content[%d] tool_result is only valid for user messages", messageIndex, blockIndex)
					}
					toolID := strings.TrimSpace(block.Get("tool_use_id").String())
					if toolID == "" {
						return nil, antigravityConversionError("messages[%d].content[%d] tool_result requires tool_use_id", messageIndex, blockIndex)
					}
					toolName := toolNameByID[toolID]
					if toolName == "" {
						toolName = antigravityToolNameFromID(toolID)
					}
					response, parts, responseErr := antigravityToolResult(block.Get("content"), messageIndex, blockIndex)
					if responseErr != nil {
						return nil, responseErr
					}
					fr := map[string]interface{}{
						"id":       toolID,
						"name":     toolName,
						"response": response,
					}
					if len(parts) > 0 {
						fr["parts"] = parts
					}
					if block.Get("is_error").Bool() {
						fr["isError"] = true
					}
					frJSON, _ := json.Marshal(fr)
					gc.Parts = append(gc.Parts, geminiPart{FunctionResponse: frJSON})
				default:
					return nil, antigravityConversionError("messages[%d].content[%d] type %q cannot be safely converted", messageIndex, blockIndex, typ)
				}
			}
		default:
			return nil, antigravityConversionError("messages[%d].content must be a string or array", messageIndex)
		}
		if len(gc.Parts) == 0 && role == "model" {
			continue
		}
		if len(gc.Parts) == 0 {
			return nil, antigravityConversionError("messages[%d] has no convertible content", messageIndex)
		}
		out = append(out, gc)
	}
	if len(out) == 0 {
		return nil, antigravityConversionError("request has no messages")
	}
	return out, nil
}

func antigravityToolResult(content gjson.Result, messageIndex, blockIndex int) (map[string]interface{}, []map[string]interface{}, error) {
	response := map[string]interface{}{}
	if content.Type == gjson.String {
		response["result"] = content.String()
		return response, nil, nil
	}
	if !content.IsArray() {
		if !content.Exists() {
			response["result"] = ""
			return response, nil, nil
		}
		if content.IsObject() {
			var object interface{}
			if err := json.Unmarshal([]byte(content.Raw), &object); err != nil {
				return nil, nil, antigravityConversionError("messages[%d].content[%d] tool_result object is invalid: %v", messageIndex, blockIndex, err)
			}
			response["result"] = object
			return response, nil, nil
		}
		return nil, nil, antigravityConversionError("messages[%d].content[%d] tool_result content must be a string or array", messageIndex, blockIndex)
	}
	results := make([]interface{}, 0, len(content.Array()))
	inlineParts := make([]map[string]interface{}, 0)
	for resultIndex, item := range content.Array() {
		switch item.Get("type").String() {
		case "text":
			results = append(results, map[string]interface{}{"type": "text", "text": item.Get("text").String()})
		case "image":
			source := item.Get("source")
			if source.Get("type").String() != "base64" || strings.TrimSpace(source.Get("media_type").String()) == "" || strings.TrimSpace(source.Get("data").String()) == "" {
				return nil, nil, antigravityConversionError("messages[%d].content[%d] tool_result item %d has an unsupported image source", messageIndex, blockIndex, resultIndex)
			}
			inlineParts = append(inlineParts, map[string]interface{}{"inlineData": map[string]string{
				"mimeType": source.Get("media_type").String(), "data": source.Get("data").String(),
			}})
		default:
			return nil, nil, antigravityConversionError("messages[%d].content[%d] tool_result item %d type %q cannot be safely converted", messageIndex, blockIndex, resultIndex, item.Get("type").String())
		}
	}
	switch len(results) {
	case 0:
		response["result"] = ""
	case 1:
		response["result"] = results[0]
	default:
		response["result"] = results
	}
	return response, inlineParts, nil
}

func antigravityToolNameFromID(toolID string) string {
	toolID = strings.TrimSpace(toolID)
	parts := strings.Split(toolID, "-")
	if len(parts) > 2 {
		if derived := strings.TrimSpace(strings.Join(parts[:len(parts)-2], "-")); derived != "" {
			return derived
		}
	}
	if toolID != "" {
		return toolID
	}
	return "tool"
}

func antigravityReplayThinkingSignature(model, raw string) (string, bool) {
	if !AntigravityIsClaudeModel(model) {
		return "", false
	}
	return normalizeAntigravityClaudeSignature(raw)
}

func antigravityReplayToolSignature(model, raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if AntigravityIsClaudeModel(model) {
		return normalizeAntigravityClaudeSignature(raw)
	}
	if _, incompatibleClaude := normalizeAntigravityClaudeSignature(raw); incompatibleClaude || strings.HasPrefix(strings.ToLower(raw), "claude#") || strings.HasPrefix(strings.ToLower(raw), "anthropic#") {
		return "skip_thought_signature_validator", true
	}
	if prefix, payload, ok := strings.Cut(raw, "#"); ok && (strings.EqualFold(prefix, "gemini") || strings.EqualFold(prefix, "google")) {
		raw = strings.TrimSpace(payload)
	}
	return raw, raw != ""
}

// Antigravity Claude expects its provider-native E-form signature wrapped in a
// second base64 layer (R-form). Shallow validation is deliberate here: malformed
// or cross-provider history is dropped before the request, while valid signatures
// are normalized without interpreting their opaque payload.
func normalizeAntigravityClaudeSignature(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if prefix, payload, ok := strings.Cut(raw, "#"); ok {
		if !strings.EqualFold(strings.TrimSpace(prefix), "claude") && !strings.EqualFold(strings.TrimSpace(prefix), "anthropic") {
			return "", false
		}
		raw = strings.TrimSpace(payload)
	}
	if raw == "" || len(raw) > 32*1024*1024 {
		return "", false
	}
	switch raw[0] {
	case 'E':
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(decoded) == 0 || decoded[0] != 0x12 {
			return "", false
		}
		return base64.StdEncoding.EncodeToString([]byte(raw)), true
	case 'R':
		inner, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(inner) == 0 || inner[0] != 'E' {
			return "", false
		}
		decoded, err := base64.StdEncoding.DecodeString(string(inner))
		if err != nil || len(decoded) == 0 || decoded[0] != 0x12 {
			return "", false
		}
		return raw, true
	default:
		return "", false
	}
}

// antigravityStableSessionID derives a stable session ID from the first user message
// text. The account salt prevents two physical accounts from sharing provider
// state while preserving stability for retries and later turns on one account.
func antigravityStableSessionID(body []byte, accountID string) string {
	seed := strings.TrimSpace(accountID)
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
					h := sha256.Sum256([]byte(seed + "\x00" + text))
					n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
					return fmt.Sprintf("-%d", n)
				}
			}
		}
	}
	h := sha256.Sum256(append([]byte(seed+"\x00"), body...))
	n := int64(binary.BigEndian.Uint64(h[:8])) & 0x7FFFFFFFFFFFFFFF
	return fmt.Sprintf("-%d", n)
}

// AntigravityChunk is a parsed Gemini streaming response chunk translated to
// a minimal Anthropic-compatible form for downstream writing.
type AntigravityChunk struct {
	Text         string
	FunctionCall *AntigravityFunctionCall
	Parts        []AntigravityResponsePart
	StopReason   string // "end_turn", "tool_use", "max_tokens", ""
	InputTokens  int64
	OutputTokens int64
	CachedTokens int64 // usageMetadata.cachedContentTokenCount; >0 means cache hit
	IsLast       bool
	Blocked      bool
}

type AntigravityFunctionCall struct {
	ID    string
	Name  string
	Args  json.RawMessage
	Index int
}

type AntigravityResponsePart struct {
	Text             string
	Thought          bool
	ThoughtSignature string
	FunctionCall     *AntigravityFunctionCall
}

// ParseAntigravitySSELine remains as a compatibility helper for a single-line
// event. Streaming paths use antigravitySSEDecoder so CRLF, multi-line data
// fields and arbitrary transport chunking are handled correctly.
func ParseAntigravitySSELine(line string) (*AntigravityChunk, error) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if !strings.HasPrefix(line, "data:") {
		return nil, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "[DONE]" {
		return &AntigravityChunk{IsLast: true}, nil
	}
	if payload == "" {
		return nil, nil
	}
	return parseAntigravityChunk([]byte(payload))
}

func parseAntigravityChunk(payload []byte) (*AntigravityChunk, error) {
	if !json.Valid(payload) {
		return nil, fmt.Errorf("antigravity response chunk is not valid JSON")
	}
	r := gjson.ParseBytes(payload)
	if embedded := r.Get("error"); embedded.Exists() {
		return nil, newAntigravityEmbeddedError(embedded)
	}
	if wrapped := r.Get("response"); wrapped.Exists() {
		r = wrapped
	}
	if embedded := r.Get("error"); embedded.Exists() {
		return nil, newAntigravityEmbeddedError(embedded)
	}
	chunk := &AntigravityChunk{}

	// usageMetadata at top level
	if usage := r.Get("usageMetadata"); usage.Exists() {
		chunk.InputTokens = usage.Get("promptTokenCount").Int()
		chunk.OutputTokens = usage.Get("candidatesTokenCount").Int() + usage.Get("thoughtsTokenCount").Int()
		chunk.CachedTokens = usage.Get("cachedContentTokenCount").Int()
	}
	if usage := r.Get("cpaUsageMetadata"); usage.Exists() {
		if chunk.InputTokens == 0 {
			chunk.InputTokens = usage.Get("promptTokenCount").Int()
		}
		if chunk.OutputTokens == 0 {
			chunk.OutputTokens = usage.Get("candidatesTokenCount").Int() + usage.Get("thoughtsTokenCount").Int()
		}
	}
	if r.Get("promptFeedback.blockReason").String() != "" {
		chunk.Blocked = true
		chunk.IsLast = true
		chunk.StopReason = "end_turn"
	}

	candidates := r.Get("candidates")
	if !candidates.IsArray() || len(candidates.Array()) == 0 {
		return chunk, nil
	}
	cand := candidates.Array()[0]

	// finishReason
	switch strings.ToUpper(cand.Get("finishReason").String()) {
	case "STOP":
		chunk.StopReason = "end_turn"
		chunk.IsLast = true
	case "":
		// Google explicitly uses an empty finishReason while generation is still
		// in progress. Usage metadata in the same frame does not change that.
	case "MAX_TOKENS":
		chunk.StopReason = "max_tokens"
		chunk.IsLast = true
	case "FINISH_REASON_UNSPECIFIED":
		// Unspecified is not evidence that generation has stopped.
	case "SAFETY", "OTHER":
		chunk.StopReason = "end_turn"
		chunk.IsLast = true
		chunk.Blocked = strings.EqualFold(cand.Get("finishReason").String(), "SAFETY")
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
		responsePart := AntigravityResponsePart{
			Text:             part.Get("text").String(),
			Thought:          part.Get("thought").Bool(),
			ThoughtSignature: firstNonEmptyString(part.Get("thoughtSignature").String(), part.Get("thought_signature").String()),
		}
		if responsePart.Text != "" && !responsePart.Thought {
			chunk.Text += responsePart.Text
		}
		if fc := part.Get("functionCall"); fc.Exists() {
			responsePart.FunctionCall = &AntigravityFunctionCall{
				ID:    fc.Get("id").String(),
				Name:  fc.Get("name").String(),
				Args:  []byte(fc.Get("args").Raw),
				Index: i,
			}
			if chunk.FunctionCall == nil {
				chunk.FunctionCall = responsePart.FunctionCall
			}
			if chunk.StopReason == "" {
				chunk.StopReason = "tool_use"
			}
		}
		if responsePart.Text != "" || responsePart.ThoughtSignature != "" || responsePart.FunctionCall != nil {
			chunk.Parts = append(chunk.Parts, responsePart)
		}
	}
	return chunk, nil
}

const antigravityMaxSSEFrameBytes = 2 << 20

type antigravitySSEDecoder struct {
	reader *bufio.Reader
}

func newAntigravitySSEDecoder(reader io.Reader) *antigravitySSEDecoder {
	if buffered, ok := reader.(*bufio.Reader); ok {
		return &antigravitySSEDecoder{reader: buffered}
	}
	return &antigravitySSEDecoder{reader: bufio.NewReaderSize(reader, 64*1024)}
}

// Next returns one complete SSE event. data fields are joined with a newline as
// required by the SSE standard. raw is a normalized replayable frame.
func (d *antigravitySSEDecoder) Next() (data string, raw []byte, ok bool, err error) {
	var dataLines []string
	var frame bytes.Buffer
	sawData := false
	for {
		line, readErr := readBoundedSSELine(d.reader, antigravityMaxSSEFrameBytes-frame.Len())
		if len(line) > 0 || readErr == nil {
			frame.Write(line)
			frame.WriteByte('\n')
		}
		if frame.Len() > antigravityMaxSSEFrameBytes {
			return "", nil, false, errors.New("antigravity SSE frame exceeds limit")
		}
		if len(line) == 0 {
			if sawData {
				return strings.Join(dataLines, "\n"), append([]byte(nil), frame.Bytes()...), true, nil
			}
			frame.Reset()
		} else if line[0] != ':' {
			field, value, hasColon := strings.Cut(string(line), ":")
			if hasColon && strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			if field == "data" {
				sawData = true
				dataLines = append(dataLines, value)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", nil, false, readErr
			}
			if sawData {
				return strings.Join(dataLines, "\n"), append([]byte(nil), frame.Bytes()...), true, nil
			}
			return "", nil, false, io.EOF
		}
	}
}

func readBoundedSSELine(reader *bufio.Reader, remaining int) ([]byte, error) {
	if remaining <= 0 {
		return nil, errors.New("antigravity SSE frame exceeds limit")
	}
	line := make([]byte, 0, min(remaining, 4096))
	for {
		part, prefix, err := reader.ReadLine()
		if len(line)+len(part) > remaining {
			return nil, errors.New("antigravity SSE frame exceeds limit")
		}
		line = append(line, part...)
		if err != nil {
			return line, err
		}
		if !prefix {
			return line, nil
		}
	}
}

func newAntigravityEmbeddedError(node gjson.Result) error {
	status := int(node.Get("code").Int())
	if status < 100 || status > 599 {
		switch strings.ToUpper(strings.TrimSpace(node.Get("status").String())) {
		case "UNAUTHENTICATED":
			status = http.StatusUnauthorized
		case "RESOURCE_EXHAUSTED":
			status = http.StatusTooManyRequests
		case "INVALID_ARGUMENT":
			status = http.StatusBadRequest
		case "PERMISSION_DENIED":
			status = http.StatusForbidden
		case "UNAVAILABLE":
			status = http.StatusServiceUnavailable
		default:
			status = http.StatusBadGateway
		}
	}
	body := []byte(node.Raw)
	if len(body) == 0 {
		body = []byte(`{"error":{"message":"upstream error"}}`)
	}
	return &AntigravityUpstreamError{StatusCode: status, Body: body}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func antigravitySignatureForClaude(model, signature string) string {
	if signature == "" || !AntigravityIsClaudeModel(model) || !strings.HasPrefix(signature, "R") {
		return signature
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(decoded) == 0 {
		return signature
	}
	return string(decoded)
}

// ProbeAntigravitySSE reads through the first valid Antigravity data event. The
// returned prefix must be replayed into AntigravityStreamToAnthropic. This lets
// the API fail over while no downstream bytes have been committed when a 200
// response actually contains an error or closes before producing a response.
func ProbeAntigravitySSE(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, io.ErrUnexpectedEOF
	}
	var prefix bytes.Buffer
	decoder := newAntigravitySSEDecoder(reader)
	for prefix.Len() <= 512*1024 {
		payload, raw, ok, err := decoder.Next()
		if len(raw) > 0 {
			prefix.Write(raw)
		}
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if !ok {
			continue
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			return nil, io.ErrUnexpectedEOF
		}
		chunk, parseErr := parseAntigravityChunk([]byte(payload))
		if parseErr != nil {
			return nil, parseErr
		}
		if chunk == nil {
			continue
		}
		// Usage-only frames are valid but do not commit downstream bytes. Keep
		// probing so a pre-first-byte failure can still fail over accounts.
		if len(chunk.Parts) == 0 && !chunk.IsLast && !chunk.Blocked {
			continue
		}
		if len(chunk.Parts) == 0 && chunk.IsLast && !chunk.Blocked && chunk.InputTokens == 0 && chunk.OutputTokens == 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return append([]byte(nil), prefix.Bytes()...), nil
	}
	return nil, fmt.Errorf("antigravity stream did not produce a valid event within the probe limit")
}

// AntigravityStreamToAnthropic reads an Antigravity SSE stream and writes Anthropic-
// compatible SSE events to w. Returns the final usage (input/output/cached tokens).
// The msgID is used as the Anthropic message id (msg_*) for the downstream response.
func AntigravityStreamToAnthropic(ctx context.Context, body io.Reader, w io.Writer,
	model, msgID string) (inputTok, outputTok, cachedTok int64, stopReason string, err error) {
	stopReason = ""
	decoder := newAntigravitySSEDecoder(body)
	writeEvent := func(event string, payload []byte) error {
		_, writeErr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		return writeErr
	}
	msgStart, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant", "content": []interface{}{},
			"model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int64{"input_tokens": 0, "output_tokens": 0},
		},
	})
	if err = writeEvent("message_start", msgStart); err != nil {
		return 0, 0, 0, "", err
	}
	if err = writeEvent("ping", []byte(`{"type":"ping"}`)); err != nil {
		return 0, 0, 0, "", err
	}

	blockIndex := 0
	blockType := ""
	hasContent := false
	sawChunk := false
	sawTerminal := false
	closeBlock := func() error {
		if blockType == "" {
			return nil
		}
		payload, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
		if closeErr := writeEvent("content_block_stop", payload); closeErr != nil {
			return closeErr
		}
		blockIndex++
		blockType = ""
		return nil
	}
	startBlock := func(kind string, content map[string]interface{}) error {
		if blockType == kind && kind != "tool_use" {
			return nil
		}
		if closeErr := closeBlock(); closeErr != nil {
			return closeErr
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"type": "content_block_start", "index": blockIndex, "content_block": content,
		})
		if startErr := writeEvent("content_block_start", payload); startErr != nil {
			return startErr
		}
		blockType = kind
		hasContent = true
		return nil
	}
	for {
		if ctx.Err() != nil {
			return inputTok, outputTok, cachedTok, stopReason, ctx.Err()
		}
		payload, _, ok, readErr := decoder.Next()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				err = io.ErrUnexpectedEOF
			} else {
				err = readErr
			}
			break
		}
		if !ok {
			continue
		}
		var chunk *AntigravityChunk
		var parseErr error
		if strings.TrimSpace(payload) == "[DONE]" {
			chunk = &AntigravityChunk{IsLast: true}
		} else {
			chunk, parseErr = parseAntigravityChunk([]byte(payload))
		}
		if parseErr != nil {
			err = parseErr
			break
		}
		if chunk == nil {
			continue
		}
		sawChunk = true
		if chunk.InputTokens > inputTok {
			inputTok = chunk.InputTokens
		}
		if chunk.OutputTokens > outputTok {
			outputTok = chunk.OutputTokens
		}
		if chunk.CachedTokens > cachedTok {
			cachedTok = chunk.CachedTokens
		}
		if chunk.StopReason != "" {
			stopReason = chunk.StopReason
		}

		for _, part := range chunk.Parts {
			if part.FunctionCall != nil {
				fc := part.FunctionCall
				toolID := strings.TrimSpace(fc.ID)
				if toolID == "" {
					toolID = "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
				}
				if err = startBlock("tool_use", map[string]interface{}{"type": "tool_use", "id": toolID, "name": fc.Name, "input": map[string]interface{}{}}); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
				args := string(fc.Args)
				if args == "" {
					args = "{}"
				}
				delta, _ := json.Marshal(map[string]interface{}{
					"type": "content_block_delta", "index": blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
				})
				if err = writeEvent("content_block_delta", delta); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
				if err = closeBlock(); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
				continue
			}
			if part.Thought {
				if err = startBlock("thinking", map[string]interface{}{"type": "thinking", "thinking": ""}); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
				if part.Text != "" {
					delta, _ := json.Marshal(map[string]interface{}{
						"type": "content_block_delta", "index": blockIndex,
						"delta": map[string]interface{}{"type": "thinking_delta", "thinking": part.Text},
					})
					if err = writeEvent("content_block_delta", delta); err != nil {
						return inputTok, outputTok, cachedTok, stopReason, err
					}
				}
				if signature := antigravitySignatureForClaude(model, part.ThoughtSignature); signature != "" {
					delta, _ := json.Marshal(map[string]interface{}{
						"type": "content_block_delta", "index": blockIndex,
						"delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
					})
					if err = writeEvent("content_block_delta", delta); err != nil {
						return inputTok, outputTok, cachedTok, stopReason, err
					}
				}
				continue
			}
			if part.ThoughtSignature != "" {
				if err = startBlock("thinking", map[string]interface{}{"type": "thinking", "thinking": ""}); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
				delta, _ := json.Marshal(map[string]interface{}{
					"type": "content_block_delta", "index": blockIndex,
					"delta": map[string]interface{}{"type": "signature_delta", "signature": antigravitySignatureForClaude(model, part.ThoughtSignature)},
				})
				if err = writeEvent("content_block_delta", delta); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
			}
			if part.Text != "" {
				if err = startBlock("text", map[string]interface{}{"type": "text", "text": ""}); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
				delta, _ := json.Marshal(map[string]interface{}{
					"type": "content_block_delta", "index": blockIndex,
					"delta": map[string]interface{}{"type": "text_delta", "text": part.Text},
				})
				if err = writeEvent("content_block_delta", delta); err != nil {
					return inputTok, outputTok, cachedTok, stopReason, err
				}
			}
		}

		if chunk.IsLast {
			sawTerminal = true
			break
		}
	}
	if err == nil && (!sawChunk || !sawTerminal) {
		err = io.ErrUnexpectedEOF
	}

	if err == nil && !hasContent {
		if startErr := startBlock("text", map[string]interface{}{"type": "text", "text": ""}); startErr != nil {
			return inputTok, outputTok, cachedTok, stopReason, startErr
		}
	}
	if closeErr := closeBlock(); closeErr != nil {
		return inputTok, outputTok, cachedTok, stopReason, closeErr
	}
	if err != nil {
		failure, _ := json.Marshal(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":       "server_error",
				"code":       "service_unavailable",
				"message":    "The relay service is temporarily unavailable. Please retry.",
				"request_id": "REQ-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")[:16]),
			},
		})
		if writeErr := writeEvent("error", failure); writeErr != nil {
			return inputTok, outputTok, cachedTok, stopReason, writeErr
		}
		return inputTok, outputTok, cachedTok, stopReason, err
	}
	if stopReason == "" {
		stopReason = "end_turn"
	}

	usage := map[string]interface{}{"output_tokens": outputTok}
	if cachedTok > 0 {
		usage["cache_read_input_tokens"] = cachedTok
	}
	messageDelta, _ := json.Marshal(map[string]interface{}{
		"type": "message_delta", "delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil}, "usage": usage,
	})
	if writeErr := writeEvent("message_delta", messageDelta); writeErr != nil {
		return inputTok, outputTok, cachedTok, stopReason, writeErr
	}
	if writeErr := writeEvent("message_stop", []byte(`{"type":"message_stop"}`)); writeErr != nil {
		return inputTok, outputTok, cachedTok, stopReason, writeErr
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

	content := make([]map[string]interface{}, 0, len(chunk.Parts))
	hasToolUse := false
	for _, part := range chunk.Parts {
		if part.FunctionCall != nil {
			toolID := strings.TrimSpace(part.FunctionCall.ID)
			if toolID == "" {
				toolID = "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
			}
			input := map[string]interface{}{}
			if len(part.FunctionCall.Args) > 0 {
				_ = json.Unmarshal(part.FunctionCall.Args, &input)
			}
			content = append(content, map[string]interface{}{
				"type": "tool_use", "id": toolID, "name": part.FunctionCall.Name, "input": input,
			})
			hasToolUse = true
			continue
		}
		if part.Thought || part.ThoughtSignature != "" {
			block := map[string]interface{}{"type": "thinking", "thinking": part.Text}
			if signature := antigravitySignatureForClaude(model, part.ThoughtSignature); signature != "" {
				block["signature"] = signature
			}
			content = append(content, block)
			continue
		}
		if part.Text != "" {
			content = append(content, map[string]interface{}{"type": "text", "text": part.Text})
		}
	}
	if len(content) == 0 {
		content = []map[string]interface{}{{"type": "text", "text": ""}}
	}
	if hasToolUse {
		stopReason = "tool_use"
	}

	type usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	type msg struct {
		ID           string                   `json:"id"`
		Type         string                   `json:"type"`
		Role         string                   `json:"role"`
		Content      []map[string]interface{} `json:"content"`
		Model        string                   `json:"model"`
		StopReason   string                   `json:"stop_reason"`
		StopSequence interface{}              `json:"stop_sequence"`
		Usage        usage                    `json:"usage"`
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
