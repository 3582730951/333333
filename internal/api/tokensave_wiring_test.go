package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// tokensave_wiring_test.go proves Phase ③ end-to-end: with token_save_enabled ON, a
// /v1/messages request's large tool_result is compressed BEFORE it reaches the upstream
// (observed on the captured upstream body); with it OFF the upstream receives the full
// content. The compression engine itself is unit-tested in internal/tokensave.

func TestTokenSaveWiringOnMessagesPath(t *testing.T) {
	var hb strings.Builder
	for i := 0; i < 800; i++ {
		hb.WriteString("compile unit ")
		hb.WriteString(strconv.Itoa(i))
		hb.WriteString(" ok\n")
	}
	reqRaw, _ := json.Marshal(map[string]interface{}{
		"model": "claude-x",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": hb.String()},
			}},
		},
	})
	reqBody := string(reqRaw)

	run := func(enabled bool) string {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		})
		h.importAccount(t, "claude-a", "", "sk-ant-oat-test")
		if enabled {
			patchConfig(t, h, `{"token_save_enabled": true}`)
		}
		resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, rq := range h.requests() {
			if strings.Contains(rq.Body, "compile unit") || strings.Contains(rq.Body, "elided") {
				return rq.Body
			}
		}
		return ""
	}

	on := run(true)
	if on == "" || !strings.Contains(on, "lines elided by token-saver") {
		t.Fatalf("token-save ON: upstream tool_result not compressed:\n%.300s", on)
	}
	off := run(false)
	if off == "" || strings.Contains(off, "lines elided by token-saver") {
		t.Fatalf("token-save OFF: upstream body should be uncompressed:\n%.300s", off)
	}
	if len(off) <= len(on) {
		t.Fatalf("OFF body (%d bytes) should be larger than ON (%d bytes)", len(off), len(on))
	}
}
