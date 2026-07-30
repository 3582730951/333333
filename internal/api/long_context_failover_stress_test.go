package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

const (
	longContextStressDownstreams = 8
	longContextBigInteger        = "900719925474099312345678901"
)

type longContextHTTPResult struct {
	index   int
	status  int
	headers http.Header
	body    []byte
	err     error
}

func runLongContextRequests(concurrency int, build func(int) (*http.Request, error)) []longContextHTTPResult {
	start := make(chan struct{})
	results := make(chan longContextHTTPResult, concurrency)
	var workers sync.WaitGroup
	client := &http.Client{Timeout: 3 * time.Minute}
	for index := 0; index < concurrency; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			request, err := build(index)
			if err != nil {
				results <- longContextHTTPResult{index: index, err: err}
				return
			}
			response, err := client.Do(request)
			if err != nil {
				results <- longContextHTTPResult{index: index, err: err}
				return
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr == nil {
				readErr = closeErr
			}
			results <- longContextHTTPResult{
				index: index, status: response.StatusCode,
				headers: response.Header.Clone(), body: body, err: readErr,
			}
		}(index)
	}
	close(start)
	workers.Wait()
	close(results)
	out := make([]longContextHTTPResult, concurrency)
	for result := range results {
		out[result.index] = result
	}
	return out
}

func requireLongContextHTTPResults(t *testing.T, phase string, results []longContextHTTPResult, contextStatus string) {
	t.Helper()
	for _, result := range results {
		if result.err != nil || result.status != http.StatusOK {
			t.Fatalf("%s downstream=%d status=%d err=%v body=%s",
				phase, result.index, result.status, result.err, result.body)
		}
		if contextStatus != "" && result.headers.Get("X-MiCliProxy-Context-Status") != contextStatus {
			t.Fatalf("%s downstream=%d context_status=%q want=%q headers=%v",
				phase, result.index, result.headers.Get("X-MiCliProxy-Context-Status"), contextStatus, result.headers)
		}
	}
}

func longContextMarker(family string, index int, edge string) string {
	return fmt.Sprintf("%s_1M_%s_%02d_7D9E", family, edge, index)
}

func longContextDigest(payload string) [sha256.Size]byte {
	return sha256.Sum256([]byte(payload))
}

func contextFromWire(raw []byte, begin, end string) ([]byte, bool) {
	start := bytes.Index(raw, []byte(begin))
	if start < 0 {
		return nil, false
	}
	finishRelative := bytes.Index(raw[start+len(begin):], []byte(end))
	if finishRelative < 0 {
		return nil, false
	}
	finish := start + len(begin) + finishRelative + len(end)
	return raw[start:finish], true
}

func identifyLongContext(raw []byte, family string, count int) (int, bool) {
	for index := 0; index < count; index++ {
		if bytes.Contains(raw, []byte(longContextMarker(family, index, "BEGIN"))) {
			return index, true
		}
	}
	return 0, false
}

func assertCompressedGoalStorage(t *testing.T, store *storage.Store, wantSessions int) {
	t.Helper()
	var sessions, chunks, logicalBytes, storedBytes int64
	if err := store.DB().QueryRowContext(context.Background(), `SELECT
(SELECT COUNT(*) FROM goal_session),
(SELECT COUNT(*) FROM goal_payload_chunk),
(SELECT COALESCE(SUM(payload_bytes),0) FROM goal_payload_chunk),
(SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM goal_payload_chunk)`).Scan(
		&sessions, &chunks, &logicalBytes, &storedBytes); err != nil {
		t.Fatal(err)
	}
	if sessions != int64(wantSessions) || chunks == 0 || logicalBytes == 0 || storedBytes == 0 {
		t.Fatalf("goal compression accounting sessions=%d chunks=%d logical=%d stored=%d",
			sessions, chunks, logicalBytes, storedBytes)
	}
	// Both stress fixtures are intentionally repetitive. A loose 50% threshold
	// catches accidental plaintext chunk persistence without coupling the test to
	// one gzip implementation or encryption envelope.
	if storedBytes >= logicalBytes/2 {
		t.Fatalf("goal chunks were not compressed at rest: logical=%d stored=%d", logicalBytes, storedBytes)
	}
}

// TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs exercises a
// one-mebibyte model-visible prompt. That is approximately 250-300K tokens and
// deliberately stays inside the product's fixed 372K GPT-5.6 window. Eight
// independent downstream threads first bind to account A, then concurrently
// resume after account A fails. The alternate account must receive a byte-exact,
// self-contained rebuild with the tool call/output pair intact.
func TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs(t *testing.T) {
	const (
		family        = "CODEX_GPT"
		accountAToken = "access-long-context-codex-a"
		accountBToken = "access-long-context-codex-b"
	)
	core := strings.Repeat(" g", (1<<20)/2)
	contexts := make([]string, longContextStressDownstreams)
	digests := make([][sha256.Size]byte, longContextStressDownstreams)
	callIDs := make([]string, longContextStressDownstreams)
	responseIDs := make([]string, longContextStressDownstreams)
	for index := range contexts {
		begin := longContextMarker(family, index, "BEGIN")
		end := longContextMarker(family, index, "END")
		contexts[index] = begin + core + end
		digests[index] = longContextDigest(contexts[index])
		callIDs[index] = fmt.Sprintf("call_codex_1m_%02d", index)
		responseIDs[index] = fmt.Sprintf("resp_codex_1m_%02d", index)
	}

	var recovered atomic.Int32
	var violationsMu sync.Mutex
	var violations []string
	recordViolation := func(format string, args ...interface{}) {
		violationsMu.Lock()
		violations = append(violations, fmt.Sprintf(format, args...))
		violationsMu.Unlock()
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")

		if index, ok := identifyLongContext(raw, family, len(contexts)); ok &&
			!bytes.Contains(raw, []byte(`"type":"custom_tool_call_output"`)) {
			if auth != accountAToken {
				recordViolation("root downstream=%d used auth=%q", index, auth)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"root account drift"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":%q,"call_id":%q,"name":"apply_patch","input":"{\"offset\":%s}"}]}`,
				responseIDs[index], "ctc_"+callIDs[index], callIDs[index], longContextBigInteger)
			return
		}

		if auth == accountAToken {
			// The first resume remains native and references account A's response.
			// Force the mapped-session failover path before any bytes are committed.
			if !bytes.Contains(raw, []byte(`"previous_response_id"`)) {
				recordViolation("account A resume was not native: bytes=%d", len(raw))
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"session_risk","message":"account session unavailable"}}`)
			return
		}
		if auth != accountBToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"unexpected account"}`)
			return
		}

		index, ok := identifyLongContext(raw, family, len(contexts))
		if !ok {
			recordViolation("rebuilt Codex request has no downstream marker")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"missing rebuilt context"}`)
			return
		}
		begin := longContextMarker(family, index, "BEGIN")
		end := longContextMarker(family, index, "END")
		extracted, complete := contextFromWire(raw, begin, end)
		if !complete || longContextDigest(string(extracted)) != digests[index] {
			recordViolation("Codex downstream=%d context digest mismatch complete=%v bytes=%d", index, complete, len(extracted))
		}
		for other := range contexts {
			if other != index && bytes.Contains(raw, []byte(longContextMarker(family, other, "BEGIN"))) {
				recordViolation("Codex downstream=%d contains downstream=%d context", index, other)
			}
		}
		callIndex := bytes.Index(raw, []byte(`"type":"custom_tool_call"`))
		outputIndex := bytes.Index(raw, []byte(`"type":"custom_tool_call_output"`))
		callIDFields := bytes.Count(raw, []byte(fmt.Sprintf(`"call_id":%q`, callIDs[index])))
		toolItemIDs := bytes.Count(raw, []byte(fmt.Sprintf(`"id":%q`, "ctc_"+callIDs[index])))
		if bytes.Contains(raw, []byte(`"previous_response_id"`)) ||
			callIndex < 0 || outputIndex <= callIndex ||
			callIDFields != 2 || toolItemIDs != 1 ||
			!bytes.Contains(raw, []byte(longContextBigInteger)) ||
			!bytes.Contains(raw, []byte(fmt.Sprintf("codex-tool-result-%02d", index))) {
			recordViolation("Codex downstream=%d tool pair invalid call=%d output=%d call_id_fields=%d tool_item_ids=%d big_integer=%v",
				index, callIndex, outputIndex, callIDFields, toolItemIDs,
				bytes.Contains(raw, []byte(longContextBigInteger)))
		}
		recovered.Add(1)
		_, _ = fmt.Fprintf(w, `{"id":"resp_codex_1m_recovered_%02d","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`, index)
	})
	h.app.cfg.GoalLegacyJournalDualWrite = false
	enableCodexSessionMappingForTest(h)
	accountA := h.importAccount(t, "long-context-codex-a", "upstream-long-context-codex-a", accountAToken)
	setTestCapability(t, h, accountA, "gpt-5.6-sol", 372000)

	roots := runLongContextRequests(len(contexts), func(index int) (*http.Request, error) {
		body := fmt.Sprintf(`{"model":"gpt-5.6-sol","store":false,"parallel_tool_calls":false,"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"input":[{"role":"user","content":[{"type":"input_text","text":%q}]}]}`, contexts[index])
		request, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Session-Id", fmt.Sprintf("codex-1m-session-%02d", index))
		request.Header.Set("Thread-Id", fmt.Sprintf("codex-1m-session-%02d", index))
		return request, nil
	})
	requireLongContextHTTPResults(t, "Codex roots", roots, "")

	accountB := h.importAccount(t, "long-context-codex-b", "upstream-long-context-codex-b", accountBToken)
	setTestCapability(t, h, accountB, "gpt-5.6-sol", 372000)
	resumes := runLongContextRequests(len(contexts), func(index int) (*http.Request, error) {
		body := fmt.Sprintf(`{"model":"gpt-5.6-sol","previous_response_id":%q,"input":[{"type":"custom_tool_call_output","call_id":%q,"output":%q}]}`,
			responseIDs[index], callIDs[index], fmt.Sprintf("codex-tool-result-%02d", index))
		request, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Session-Id", fmt.Sprintf("codex-1m-session-%02d", index))
		request.Header.Set("Thread-Id", fmt.Sprintf("codex-1m-session-%02d", index))
		return request, nil
	})
	requireLongContextHTTPResults(t, "Codex resumes", resumes, "rebuilt")
	if recovered.Load() != int32(len(contexts)) {
		t.Fatalf("Codex recovered upstream requests=%d want=%d", recovered.Load(), len(contexts))
	}
	violationsMu.Lock()
	gotViolations := append([]string(nil), violations...)
	violationsMu.Unlock()
	if len(gotViolations) > 0 {
		t.Fatalf("Codex 1MiB account-switch violations:\n%s", strings.Join(gotViolations, "\n"))
	}
	assertCompressedGoalStorage(t, h.store, len(contexts))
}

// TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults uses
// one million token-like " a" units per Claude Code session. The second turn
// intentionally contains only the tool_result delta; goal continuity must rebuild
// the original 1M request and tool_use before movable Messages failover reaches
// account B. Eight sessions run together to guard against cross-downstream aliasing.
func TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults(t *testing.T) {
	const (
		family = "CLAUDE_CODE"
	)
	core := strings.Repeat(" a", 1_000_000)
	contexts := make([]string, longContextStressDownstreams)
	digests := make([][sha256.Size]byte, longContextStressDownstreams)
	toolIDs := make([]string, longContextStressDownstreams)
	for index := range contexts {
		begin := longContextMarker(family, index, "BEGIN")
		end := longContextMarker(family, index, "END")
		contexts[index] = begin + core + end
		digests[index] = longContextDigest(contexts[index])
		toolIDs[index] = fmt.Sprintf("toolu_claude_1m_%02d", index)
	}

	const (
		accountAID = "claude-long-context-a"
		accountBID = "claude-long-context-b"
	)
	accountAAuth := "credential-" + accountAID
	accountBAuth := "credential-" + accountBID
	var recovered atomic.Int32
	var violationsMu sync.Mutex
	var violations []string
	recordViolation := func(format string, args ...interface{}) {
		violationsMu.Lock()
		violations = append(violations, fmt.Sprintf(format, args...))
		violationsMu.Unlock()
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")

		if index, ok := identifyLongContext(raw, family, len(contexts)); ok &&
			!bytes.Contains(raw, []byte(`"type":"tool_result"`)) {
			if auth != accountAAuth {
				recordViolation("Claude root downstream=%d used auth=%q", index, auth)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"error","error":{"message":"root account drift"}}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"msg_claude_1m_%02d","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"tool_use","id":%q,"name":"Read","input":{"path":"/tmp/%02d","offset":%s}}],"stop_reason":"tool_use","usage":{"input_tokens":1000000,"output_tokens":16}}`,
				index, toolIDs[index], index, longContextBigInteger)
			return
		}

		if auth == accountAAuth {
			// goalReplayBody has already made Messages self-contained. A recoverable
			// pre-commit response now exercises transparent account failover.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"account unavailable"}}`)
			return
		}
		if auth != accountBAuth {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"message":"unexpected account"}}`)
			return
		}

		index, ok := identifyLongContext(raw, family, len(contexts))
		if !ok {
			recordViolation("rebuilt Claude request has no downstream marker")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"message":"missing rebuilt context"}}`)
			return
		}
		begin := longContextMarker(family, index, "BEGIN")
		end := longContextMarker(family, index, "END")
		extracted, complete := contextFromWire(raw, begin, end)
		if !complete || longContextDigest(string(extracted)) != digests[index] {
			recordViolation("Claude downstream=%d context digest mismatch complete=%v bytes=%d", index, complete, len(extracted))
		}
		for other := range contexts {
			if other != index && bytes.Contains(raw, []byte(longContextMarker(family, other, "BEGIN"))) {
				recordViolation("Claude downstream=%d contains downstream=%d context", index, other)
			}
		}
		toolUseIndex := bytes.Index(raw, []byte(`"type":"tool_use"`))
		toolResultIndex := bytes.Index(raw, []byte(`"type":"tool_result"`))
		if bytes.Contains(raw, []byte(`"type":"custom_tool_call"`)) ||
			toolUseIndex < 0 || toolResultIndex <= toolUseIndex ||
			bytes.Count(raw, []byte(toolIDs[index])) != 2 ||
			!bytes.Contains(raw, []byte(longContextBigInteger)) ||
			!bytes.Contains(raw, []byte(fmt.Sprintf("claude-tool-result-%02d", index))) {
			recordViolation("Claude downstream=%d tool pair invalid use=%d result=%d occurrences=%d big_integer=%v",
				index, toolUseIndex, toolResultIndex, bytes.Count(raw, []byte(toolIDs[index])),
				bytes.Contains(raw, []byte(longContextBigInteger)))
		}
		recovered.Add(1)
		_, _ = fmt.Fprintf(w, `{"id":"msg_claude_1m_recovered_%02d","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"recovered"}],"stop_reason":"end_turn","usage":{"input_tokens":1000001,"output_tokens":2}}`, index)
	})
	h.app.cfg.GoalLegacyJournalDualWrite = false
	seedClaudeContextAccount(t, h, accountAID, "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)

	roots := runLongContextRequests(len(contexts), func(index int) (*http.Request, error) {
		body := fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":64,"tools":[{"name":"Read","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":%q}]}]}`, contexts[index])
		request, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Pool-Provider", "claude")
		request.Header.Set("X-Claude-Code-Session-Id", fmt.Sprintf("claude-1m-session-%02d", index))
		request.Header.Set("Anthropic-Beta", anthropicContext1MBeta)
		return request, nil
	})
	requireLongContextHTTPResults(t, "Claude roots", roots, "")

	seedClaudeContextAccount(t, h, accountBID, "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	resumes := runLongContextRequests(len(contexts), func(index int) (*http.Request, error) {
		body := fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}]}`,
			toolIDs[index], fmt.Sprintf("claude-tool-result-%02d", index))
		request, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Pool-Provider", "claude")
		request.Header.Set("X-Claude-Code-Session-Id", fmt.Sprintf("claude-1m-session-%02d", index))
		request.Header.Set("Anthropic-Beta", anthropicContext1MBeta)
		return request, nil
	})
	requireLongContextHTTPResults(t, "Claude resumes", resumes, "rebuilt")
	if recovered.Load() != int32(len(contexts)) {
		t.Fatalf("Claude recovered upstream requests=%d want=%d", recovered.Load(), len(contexts))
	}
	violationsMu.Lock()
	gotViolations := append([]string(nil), violations...)
	violationsMu.Unlock()
	if len(gotViolations) > 0 {
		t.Fatalf("Claude 1M account-switch violations:\n%s", strings.Join(gotViolations, "\n"))
	}
	assertCompressedGoalStorage(t, h.store, len(contexts))
}
