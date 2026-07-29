package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestForcedReasoningPreservesMaxAndUltra(t *testing.T) {
	for _, effort := range []string{"max", "ultra"} {
		body := applyForcedReasoningResponses([]byte(`{"model":"gpt-5.6-sol","reasoning":{"summary":"auto"},"input":"keep"}`), effort)
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		reasoning, _ := payload["reasoning"].(map[string]interface{})
		if reasoning["effort"] != effort || reasoning["summary"] != "auto" || payload["input"] != "keep" {
			t.Fatalf("effort %q was lowered or sibling context changed: %s", effort, body)
		}
	}
}

func TestForcedReasoningChangesOnlyReasoning(t *testing.T) {
	before := []byte(`{"model":"gpt-5.6-sol","instructions":"keep","reasoning":{"effort":"low","summary":"auto"},"previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"role":"user","content":"keep exact input"}]}`)
	after := applyForcedReasoningResponses(before, "high")
	assertOnlyTopLevelJSONFieldChanged(t, before, after, "reasoning")
	var payload map[string]interface{}
	if err := json.Unmarshal(after, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reasoning"].(map[string]interface{})["effort"] != "high" {
		t.Fatalf("forced effort missing: %s", after)
	}
}

func TestUltraRejectsLunaAndUnknownBeforeScheduling(t *testing.T) {
	var calls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"id":"unexpected"}`))
	})
	for _, model := range []string{"gpt-5.6-luna", "unlisted-codex-model"} {
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"`+model+`","reasoning":{"effort":"ultra"},"input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"reasoning_effort_unsupported"`) {
			t.Fatalf("model=%s status=%d body=%s", model, resp.StatusCode, body)
		}
	}
	if calls != 0 {
		t.Fatalf("unsupported ultra reached upstream %d times", calls)
	}
}

func TestUltraSolRetainsCLISettingAndMapsToMaxOnWire(t *testing.T) {
	var upstreamBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-ultra","object":"response","status":"completed","output":[]}`))
	})
	accountID := h.importAccount(t, "ultra-sol", "up-ultra-sol", "access-ultra-sol")
	setTestCapability(t, h, accountID, "gpt-5.6-sol", 272000)
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","reasoning":{"effort":"ultra","summary":"auto"},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(upstreamBody), `"effort":"max"`) || strings.Contains(string(upstreamBody), `"effort":"ultra"`) || !strings.Contains(string(upstreamBody), `"summary":"auto"`) {
		t.Fatalf("status=%d upstream=%s", resp.StatusCode, upstreamBody)
	}
}

func TestDirectCodexGPT56AliasRoutesAsSol(t *testing.T) {
	var upstreamBody []byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-alias","object":"response","status":"completed","output":[]}`))
	})
	accountID := h.importAccount(t, "alias-sol", "up-alias-sol", "access-alias-sol")
	setTestCapability(t, h, accountID, "gpt-5.6-sol", 372000)

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6","reasoning":{"effort":"ultra"},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(upstreamBody), `"model":"gpt-5.6-sol"`) ||
		!strings.Contains(string(upstreamBody), `"effort":"max"`) {
		t.Fatalf("status=%d body=%s upstream=%s", resp.StatusCode, body, upstreamBody)
	}
}
