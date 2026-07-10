// Command live-probe runs a low-output live Codex matrix through pool_server.
// It never reads account credentials: import auth.json into the isolated pool
// first, then point this command at that pool's downstream URL.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type variant struct {
	Model  string
	Effort string
}

type tokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	Total      int64 `json:"total_tokens"`
}

type result struct {
	Kind          string     `json:"kind"`
	Transport     string     `json:"transport"`
	Model         string     `json:"model"`
	Effort        string     `json:"effort"`
	Length        string     `json:"length,omitempty"`
	Round         string     `json:"round,omitempty"`
	CorpusBytes   int        `json:"corpus_bytes,omitempty"`
	Expected      string     `json:"expected"`
	Actual        string     `json:"actual"`
	Pass          bool       `json:"pass"`
	ReturnedModel string     `json:"returned_model,omitempty"`
	Status        int        `json:"status"`
	LatencyMS     int64      `json:"latency_ms"`
	Usage         tokenUsage `json:"usage"`
	Events        []string   `json:"events,omitempty"`
	LastEvent     string     `json:"last_event,omitempty"`
	Error         string     `json:"error,omitempty"`
}

type responseCapture struct {
	Status        int
	Text          strings.Builder
	ReturnedModel string
	Usage         tokenUsage
	Events        []string
	LastEvent     string
	Err           error
}

func main() {
	base := flag.String("base", "http://127.0.0.1:18788", "pool_server downstream base URL")
	kind := flag.String("kind", "quality", "quality or cache")
	transport := flag.String("transport", "sse", "sse or websocket")
	models := flag.String("models", "gpt-5.6-sol:low,gpt-5.6-terra:low,gpt-5.6-luna:low,gpt-5.5:low,gpt-5.5:medium,gpt-5.5:high,gpt-5.5:xhigh,gpt-5.4:low,gpt-5.4-mini:low", "comma-separated model:effort variants")
	lengths := flag.String("lengths", "short:1200,medium:16000,long:80000", "cache corpus name:bytes list")
	rounds := flag.Int("rounds", 3, "cache rounds: warm, exact replay, then appended turn")
	cacheSettle := flag.Duration("cache-settle", 30*time.Second, "one propagation wait after all cache warm writes")
	flag.Parse()

	variants, err := parseVariants(*models)
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()
	switch strings.ToLower(strings.TrimSpace(*kind)) {
	case "quality":
		for _, v := range variants {
			emit(runQuality(ctx, *base, *transport, v))
		}
	case "cache":
		sizes, err := parseLengths(*lengths)
		if err != nil {
			fatal(err)
		}
		if *rounds < 2 || *rounds > 3 {
			fatal(fmt.Errorf("rounds must be 2 or 3"))
		}
		if *cacheSettle < 0 {
			fatal(fmt.Errorf("cache-settle must not be negative"))
		}
		// Warm every model/length combination first, wait once for upstream cache
		// propagation, then replay the matrix. This keeps the measurement accurate
		// without paying one 30-second delay per combination or consuming model tokens
		// during the wait.
		for round := 1; round <= *rounds; round++ {
			if round == 2 && *cacheSettle > 0 {
				time.Sleep(*cacheSettle)
			}
			for _, v := range variants {
				for _, size := range sizes {
					emit(runCache(ctx, *base, *transport, v, size.Name, size.Bytes, round))
				}
			}
		}
	default:
		fatal(fmt.Errorf("unsupported kind %q", *kind))
	}
}

func runQuality(ctx context.Context, base, transport string, v variant) result {
	const prompt = "Solve both checks. (1) f(1)=2 and f(n)=2*f(n-1)+n; find f(5). (2) Order W,X,Y,Z given Z is before W, W is immediately before Y, and X is after Y. Reply only as number|four-letter-order, with no spaces."
	const expected = "73|ZWYX"
	body := responsesBody(v, []interface{}{
		message("developer", "Deterministic integrity check. Return only the requested compact answer."),
		message("user", prompt),
	})
	started := time.Now()
	capture := send(ctx, base, transport, body)
	return resultFromCapture("quality", transport, v, "", "", 0, expected, capture, started)
}

func runCache(ctx context.Context, base, transport string, v variant, length string, corpusBytes, round int) result {
	marker := stableMarker(v, length)
	corpus := makeCorpus(corpusBytes, marker)
	expected := "CACHE_OK|" + marker
	question := "Use the complete reference above. Reply only " + expected
	roundName := "warm"
	if round == 2 {
		roundName = "exact_replay"
	}
	input := []interface{}{
		message("developer", "Preserve the complete conversation context. Return only the requested compact answer."),
		message("user", corpus),
		assistantMessage("REFERENCE_ACCEPTED"),
	}
	if round == 3 {
		roundName = "append_turn"
		// Grow the conversation exactly as a real client does: retain the prior
		// user/assistant exchange and append a new user turn. This verifies both
		// context preservation and a conversation-stable automatic cache key.
		input = append(input,
			message("user", question),
			assistantMessage(expected),
			message("user", "Follow-up: re-check the complete original reference and reply only "+expected),
		)
	} else {
		input = append(input, message("user", question))
	}
	body := responsesBody(v, input)
	started := time.Now()
	capture := send(ctx, base, transport, body)
	return resultFromCapture("cache", transport, v, length, roundName, len(corpus), expected, capture, started)
}

func responsesBody(v variant, input []interface{}) []byte {
	payload := map[string]interface{}{
		"model":               v.Model,
		"store":               false,
		"stream":              true,
		"parallel_tool_calls": false,
		"tools":               []interface{}{},
		"input":               input,
		"reasoning":           map[string]interface{}{"effort": v.Effort},
		"text":                map[string]interface{}{"verbosity": "low"},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func message(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role": role,
		"content": []interface{}{
			map[string]interface{}{"type": "input_text", "text": text},
		},
	}
}

func assistantMessage(text string) map[string]interface{} {
	return map[string]interface{}{
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{"type": "output_text", "text": text},
		},
	}
}

func send(ctx context.Context, base, transport string, body []byte) responseCapture {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "sse":
		return sendSSE(ctx, base, body)
	case "websocket", "ws":
		return sendWebSocket(ctx, base, body)
	default:
		return responseCapture{Err: fmt.Errorf("unsupported transport %q", transport)}
	}
}

func sendSSE(ctx context.Context, base string, body []byte) responseCapture {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return responseCapture{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return responseCapture{Err: err}
	}
	defer resp.Body.Close()
	capture := responseCapture{Status: resp.StatusCode}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		capture.Err = fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		return capture
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		consumeEvent(data, &capture)
	}
	if err := scanner.Err(); err != nil {
		capture.Err = err
	}
	return capture
}

func sendWebSocket(ctx context.Context, base string, body []byte) responseCapture {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	u, err := url.Parse(strings.TrimRight(base, "/") + "/v1/responses")
	if err != nil {
		return responseCapture{Err: err}
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		capture := responseCapture{Err: err}
		if resp != nil {
			capture.Status = resp.StatusCode
		}
		return capture
	}
	defer conn.Close()
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return responseCapture{Err: err}
	}
	payload["type"] = "response.create"
	if err := conn.WriteJSON(payload); err != nil {
		return responseCapture{Err: err}
	}
	capture := responseCapture{Status: http.StatusOK}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			capture.Err = err
			return capture
		}
		consumeEvent(raw, &capture)
		var event map[string]interface{}
		if json.Unmarshal(raw, &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.completed":
			return capture
		case "error":
			capture.Err = fmt.Errorf("websocket error: %s", compactJSON(raw))
			return capture
		}
	}
}

func consumeEvent(raw []byte, capture *responseCapture) {
	var event map[string]interface{}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	kind, _ := event["type"].(string)
	if kind != "" && (len(capture.Events) == 0 || capture.Events[len(capture.Events)-1] != kind) {
		capture.Events = append(capture.Events, kind)
	}
	last := compactJSON(raw)
	if len(last) > 512 {
		last = last[:512]
	}
	capture.LastEvent = last
	switch kind {
	case "error", "response.failed", "response.incomplete":
		capture.Err = fmt.Errorf("terminal event: %s", compactJSON(raw))
	}
	if kind == "response.output_text.delta" {
		if delta, _ := event["delta"].(string); delta != "" {
			capture.Text.WriteString(delta)
		}
	}
	response, _ := event["response"].(map[string]interface{})
	if response == nil {
		return
	}
	if model, _ := response["model"].(string); model != "" {
		capture.ReturnedModel = model
	}
	usageMap, _ := response["usage"].(map[string]interface{})
	if usageMap == nil {
		return
	}
	capture.Usage.Input = int64Value(usageMap["input_tokens"])
	capture.Usage.Output = int64Value(usageMap["output_tokens"])
	capture.Usage.Total = int64Value(usageMap["total_tokens"])
	if details, _ := usageMap["input_tokens_details"].(map[string]interface{}); details != nil {
		capture.Usage.Cached = int64Value(details["cached_tokens"])
		capture.Usage.CacheWrite = int64Value(details["cache_write_tokens"])
	}
	if details, _ := usageMap["output_tokens_details"].(map[string]interface{}); details != nil {
		capture.Usage.Reasoning = int64Value(details["reasoning_tokens"])
	}
}

func resultFromCapture(kind, transport string, v variant, length, round string, corpusBytes int, expected string, capture responseCapture, started time.Time) result {
	actual := strings.TrimSpace(capture.Text.String())
	r := result{
		Kind: kind, Transport: transport, Model: v.Model, Effort: v.Effort,
		Length: length, Round: round, CorpusBytes: corpusBytes,
		Expected: expected, Actual: actual, Pass: canonical(actual) == canonical(expected),
		ReturnedModel: capture.ReturnedModel, Status: capture.Status,
		LatencyMS: time.Since(started).Milliseconds(), Usage: capture.Usage,
	}
	if !r.Pass || capture.Err != nil {
		r.Events = capture.Events
		r.LastEvent = capture.LastEvent
	}
	if capture.Err != nil {
		r.Error = capture.Err.Error()
	}
	return r
}

func makeCorpus(target int, marker string) string {
	if target < 256 {
		target = 256
	}
	const line = "Stable repository reference: module alpha calls module beta; invariant gamma remains unchanged; preserve every earlier fact.\n"
	var out strings.Builder
	out.Grow(target + 128)
	out.WriteString("REFERENCE_BEGIN\n")
	for out.Len()+len(line)+64 < target {
		out.WriteString(line)
	}
	out.WriteString("The verification key at the end of the complete reference is ")
	out.WriteString(marker)
	out.WriteString(".\nREFERENCE_END")
	return out.String()
}

func stableMarker(v variant, length string) string {
	sum := sha256.Sum256([]byte(v.Model + "\x00" + v.Effort + "\x00" + length))
	return "K" + strings.ToUpper(hex.EncodeToString(sum[:4]))
}

func canonical(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.Trim(value, "`* \t\r\n.!,;:\"'")
	return strings.Join(strings.Fields(value), "")
}

func int64Value(value interface{}) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func parseVariants(raw string) ([]variant, error) {
	var out []variant
	for _, item := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid model variant %q", item)
		}
		out = append(out, variant{Model: parts[0], Effort: parts[1]})
	}
	return out, nil
}

type namedLength struct {
	Name  string
	Bytes int
}

func parseLengths(raw string) ([]namedLength, error) {
	var out []namedLength
	for _, item := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid length %q", item)
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid length bytes %q", item)
		}
		out = append(out, namedLength{Name: parts[0], Bytes: n})
	}
	return out, nil
}

func compactJSON(raw []byte) string {
	var out bytes.Buffer
	if json.Compact(&out, raw) == nil {
		return out.String()
	}
	return strings.TrimSpace(string(raw))
}

func emit(value result) {
	raw, _ := json.Marshal(value)
	fmt.Println(string(raw))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}
