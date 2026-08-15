package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestRandomizedWebSocketContextLossRebuildsToolHistoryWhileRescueExportRuns
// reproduces the downstream audit incident: a long-lived downstream WebSocket
// supplies only previous_response_id plus an otherwise orphaned tool output while
// the account-local model session may have forgotten every earlier token. The
// durable Goal checkpoint must turn every such append into a self-contained root,
// and an emergency diagnostic ZIP must remain exportable during those recoveries.
func TestRandomizedWebSocketContextLossRebuildsToolHistoryWhileRescueExportRuns(t *testing.T) {
	const (
		turns          = 18
		originalMarker = "incident-original-context-marker"
	)

	lossRNG := rand.New(rand.NewSource(0x5eedc0de))
	lostAt := make(map[int]bool, turns)
	for turn := 1; turn < turns; turn++ {
		lostAt[turn] = lossRNG.Intn(3) != 0
	}
	// Pin both edges so the regression cannot accidentally cover only early or
	// late turns after a future replay optimization.
	lostAt[1] = true
	lostAt[turns-1] = true

	var upstreamTurns atomic.Int32
	var capturedMu sync.Mutex
	captured := make([][]byte, 0, turns)
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("random context-loss turn unexpectedly left WebSocket transport: %s", r.Method)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if bytes.Contains(raw, []byte(`"type":"response.processed"`)) {
				continue
			}
			turn := int(upstreamTurns.Add(1)) - 1
			if turn >= turns {
				t.Errorf("unexpected upstream turn %d: %s", turn, raw)
				return
			}
			capturedMu.Lock()
			captured = append(captured, append([]byte(nil), raw...))
			capturedMu.Unlock()

			if turn > 0 && lostAt[turn] {
				callID := fmt.Sprintf("incident-call-%02d", turn-1)
				callIndex := bytes.Index(raw, []byte(`"type":"custom_tool_call"`))
				outputIndex := bytes.Index(raw, []byte(`"type":"custom_tool_call_output"`))
				if !bytes.Contains(raw, []byte(originalMarker)) ||
					!bytes.Contains(raw, []byte(callID)) || callIndex < 0 || outputIndex <= callIndex ||
					bytes.Contains(raw, []byte(`"previous_response_id"`)) {
					t.Errorf("turn %d could not survive total upstream context loss: %s", turn, raw)
				}
			}

			// Match a real upstream quirk reported by downstreams: an informational
			// quota snapshot can say limit_reached before the actual completed event.
			// It is metadata, not a terminal for this inference.
			if turn%5 == 3 {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"limit_reached":true,"used_percent":100}}}`))
			}
			responseID := fmt.Sprintf("incident-response-%02d", turn)
			output := `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"incident complete"}]}]`
			if turn < turns-1 {
				callID := fmt.Sprintf("incident-call-%02d", turn)
				output = fmt.Sprintf(`[{"type":"custom_tool_call","id":"item-%[1]s","call_id":"%[1]s","name":"exec","input":"{}"}]`, callID)
			}
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"object":"response","model":"gpt-5.5","status":"completed","output":%s,"usage":{"input_tokens":128000,"output_tokens":1,"total_tokens":128001}}}`, responseID, output)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
				return
			}
			if turn == turns-1 {
				return
			}
		}
	})
	h.app.cfg.CodexSessionMappingEnabled = true
	h.app.cfg.CodexCPAStrict = true
	h.app.cfg.CodexStatelessPassthrough = true
	h.app.cfg.GoalContinuityEnabled = true
	h.importAccount(t, "context-loss", "upstream-context-loss", "access-context-loss")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	header := http.Header{}
	header.Set("Thread-Id", "incident-thread")
	header.Set("Session-Id", "incident-session")
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("downstream dial: %v status=%d body=%s", err, response.StatusCode, body)
		}
		t.Fatal(err)
	}
	defer conn.Close()

	readCompleted := func(wantID string) {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, event, readErr := conn.ReadMessage()
			if readErr != nil {
				t.Fatalf("read %s: %v", wantID, readErr)
			}
			if bytes.Contains(event, []byte(`"type":"response.completed"`)) {
				if !bytes.Contains(event, []byte(wantID)) {
					t.Fatalf("completed event=%s, want %s", event, wantID)
				}
				return
			}
			if bytes.Contains(event, []byte(`"type":"response.failed"`)) || bytes.Contains(event, []byte(`"type":"error"`)) {
				t.Fatalf("context recovery failed before %s: %s", wantID, event)
			}
		}
	}

	initial := `{"type":"response.create","model":"gpt-5.5","input":[{"role":"developer","content":"Use web search, tools, and repository Skills when needed."},{"role":"user","content":"` + originalMarker + `"}],"tools":[{"type":"custom","name":"exec","description":"Run a command","format":{"type":"text"}},{"type":"web_search_preview"}],"stream":true}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(initial)); err != nil {
		t.Fatal(err)
	}
	readCompleted("incident-response-00")
	wipeCPAAliases := func(turn int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var aliases int
			if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM codex_session_alias`).Scan(&aliases); err != nil {
				t.Fatal(err)
			}
			if aliases > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("turn %d CPA terminal aliases were never committed", turn)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err := h.store.DB().ExecContext(context.Background(), `DELETE FROM codex_session_alias`); err != nil {
			t.Fatalf("turn %d simulate lost CPA alias commit: %v", turn, err)
		}
	}

	type exportResult struct {
		raw    []byte
		header http.Header
		status int
		err    error
	}
	exportDone := make(chan exportResult, 1)
	exportStarted := false
	for turn := 1; turn < turns; turn++ {
		previousID := fmt.Sprintf("incident-response-%02d", turn-1)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.processed","response_id":%q}`, previousID))); err != nil {
			t.Fatal(err)
		}
		if lostAt[turn] {
			// Model the exact downstream incident window: the Goal terminal is
			// durable, but its independently committed CPA aliases disappear before
			// the next append. Recovery must consult Goal instead of rejecting the
			// otherwise orphaned tool output as unidentified.
			wipeCPAAliases(turn)
		}
		if turn == 7 {
			exportStarted = true
			go func() {
				request, requestErr := http.NewRequest(http.MethodGet, h.pool.URL+"/admin/export/logs?mode=rescue", nil)
				if requestErr != nil {
					exportDone <- exportResult{err: requestErr}
					return
				}
				result, requestErr := http.DefaultClient.Do(request)
				if requestErr != nil {
					exportDone <- exportResult{err: requestErr}
					return
				}
				raw, readErr := io.ReadAll(result.Body)
				result.Body.Close()
				exportDone <- exportResult{raw: raw, header: result.Header.Clone(), status: result.StatusCode, err: readErr}
			}()
		}
		callID := fmt.Sprintf("incident-call-%02d", turn-1)
		appendRequest := fmt.Sprintf(`{"type":"response.append","model":"gpt-5.5","input":[{"type":"custom_tool_call_output","call_id":%q,"output":"result-%02d"}],"stream":true}`, callID, turn)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(appendRequest)); err != nil {
			t.Fatal(err)
		}
		readCompleted(fmt.Sprintf("incident-response-%02d", turn))
	}
	if !exportStarted {
		t.Fatal("diagnostic export was not started")
	}

	var exported exportResult
	select {
	case exported = <-exportDone:
	case <-time.After(15 * time.Second):
		t.Fatal("rescue diagnostic export stalled during context recovery")
	}
	if exported.err != nil || exported.status != http.StatusOK ||
		!strings.Contains(exported.header.Get("Content-Type"), "application/zip") ||
		exported.header.Get("X-Codex-Diagnostic-Mode") != "rescue" {
		t.Fatalf("rescue export status=%d headers=%v bytes=%d err=%v", exported.status, exported.header, len(exported.raw), exported.err)
	}
	files := readZipFiles(t, exported.raw)
	for _, name := range []string{"manifest.json", "diagnostic_summary.json", "goal_continuity.csv", "audit_log.csv"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("incident bundle missing %s: %v", name, zipFileNames(files))
		}
	}
	if !strings.Contains(files["audit_log.csv"], codexRecoveryReasonUnidentifiedMapping) ||
		!strings.Contains(files["goal_continuity.csv"], "GOAL-") {
		t.Fatalf("incident bundle lost recovery evidence: audit=%s goals=%s", files["audit_log.csv"], files["goal_continuity.csv"])
	}
	for _, secret := range []string{originalMarker, "incident-thread", "incident-session", "incident-call-"} {
		if strings.Contains(files["goal_continuity.csv"], secret) {
			t.Fatalf("goal continuity diagnostics leaked %q: %s", secret, files["goal_continuity.csv"])
		}
	}

	if got := upstreamTurns.Load(); got != turns {
		t.Fatalf("upstream turns=%d want=%d", got, turns)
	}
	capturedMu.Lock()
	defer capturedMu.Unlock()
	if len(captured) != turns {
		t.Fatalf("captured upstream turns=%d want=%d", len(captured), turns)
	}
	for turn := 1; turn < turns; turn++ {
		if lostAt[turn] {
			if bytes.Contains(captured[turn], []byte(`"previous_response_id"`)) ||
				!bytes.Contains(captured[turn], []byte(originalMarker)) {
				t.Fatalf("lost turn %d was not a self-contained recovery root: %s", turn, captured[turn])
			}
		} else if !bytes.Contains(captured[turn], []byte(`"previous_response_id"`)) {
			t.Fatalf("healthy turn %d discarded native upstream context: %s", turn, captured[turn])
		}
	}
}
