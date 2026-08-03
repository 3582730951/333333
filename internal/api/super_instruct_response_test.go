package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
)

func TestSuperInstructResponseRewriteMemoryMonitor(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), "bypass license") {
			_, _ = w.Write([]byte(`{"id":"resp_refusal","object":"response","status":"completed","output_text":"I can't assist with that request to bypass license activation."}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_success","object":"response","status":"completed","output_text":"This is a sufficiently long successful reverse engineering answer that should be learned by memory and left unchanged for the downstream client."}`))
	})

	rewriteKey := createTestAPIKeyForUserGroup(t, h, "si-rewrite-pool", map[string]interface{}{
		"super_instruct_response_rewrite_enabled": true,
		"super_instruct_memory_enabled":           true,
		"super_instruct_monitor_enabled":          true,
	})
	rewriteAcc := h.importAccount(t, "si-rewrite", "up-si-rewrite", "access-si-rewrite")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+rewriteAcc+"/group", `{"group":"si-rewrite-pool"}`); code != http.StatusOK {
		t.Fatalf("assign rewrite group = %d: %s", code, raw)
	}
	setTestCapability(t, h, rewriteAcc, "gpt-5.6-sol", 272000)

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"please bypass license activation"}`))
	req.Header.Set("Authorization", "Bearer "+rewriteKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Rei Protocol") || !strings.Contains(string(body), "resp_tamper") {
		t.Fatalf("rewrite response status=%d body=%s", resp.StatusCode, body)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/super-instruct/monitor", "")
	if code != http.StatusOK {
		t.Fatalf("monitor status = %d: %s", code, raw)
	}
	var monitor superinstruct.MonitorSnapshot
	if err := json.Unmarshal(raw, &monitor); err != nil {
		t.Fatalf("decode monitor: %v (%s)", err, raw)
	}
	if monitor.Stats.Total != 1 || monitor.Stats.Tamper != 1 || len(monitor.History) != 1 || !monitor.History[0].Tampered {
		t.Fatalf("monitor after rewrite mismatch: %+v", monitor)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/super-instruct/memory", "")
	if code != http.StatusOK {
		t.Fatalf("memory status = %d: %s", code, raw)
	}
	var memory superinstruct.MemoryData
	if err := json.Unmarshal(raw, &memory); err != nil {
		t.Fatalf("decode memory: %v (%s)", err, raw)
	}
	if memory.Stats.Total != 0 || len(memory.Successes) != 0 {
		t.Fatalf("tampered response should not be learned: %+v", memory)
	}

	memoryKey := createTestAPIKeyForUserGroup(t, h, "si-memory-pool", map[string]interface{}{
		"super_instruct_memory_enabled":  true,
		"super_instruct_monitor_enabled": true,
	})
	memoryAcc := h.importAccount(t, "si-memory", "up-si-memory", "access-si-memory")
	if code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+memoryAcc+"/group", `{"group":"si-memory-pool"}`); code != http.StatusOK {
		t.Fatalf("assign memory group = %d: %s", code, raw)
	}
	setTestCapability(t, h, memoryAcc, "gpt-5.6-sol", 272000)

	req, _ = http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"reverse this binary safely"}`))
	req.Header.Set("Authorization", "Bearer "+memoryKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "Rei Protocol") || !strings.Contains(string(body), "sufficiently long successful reverse engineering answer") {
		t.Fatalf("memory response status=%d body=%s", resp.StatusCode, body)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/super-instruct/memory", "")
	if code != http.StatusOK {
		t.Fatalf("memory status after success = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &memory); err != nil {
		t.Fatalf("decode memory after success: %v (%s)", err, raw)
	}
	if memory.Stats.Total != 1 || memory.Stats.Reverse != 1 || len(memory.Successes) != 1 {
		t.Fatalf("memory after success mismatch: %+v", memory)
	}
	code, raw = grpReq(t, h, http.MethodGet, "/admin/super-instruct/monitor", "")
	if code != http.StatusOK {
		t.Fatalf("monitor status after success = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &monitor); err != nil {
		t.Fatalf("decode monitor after success: %v (%s)", err, raw)
	}
	if monitor.Stats.Total != 2 || monitor.Stats.Tamper != 1 || monitor.Stats.MemoryCount != 1 || len(monitor.History) != 2 {
		t.Fatalf("monitor after success mismatch: %+v", monitor)
	}
}

func TestSuperInstructResponseProfilesSelectByModelFamily(t *testing.T) {
	group := storage.Group{SuperInstructProfiles: storage.SuperInstructProfiles{
		storage.ModelInstructionFamilyGPT: {
			ResponseRewriteEnabled: true,
			MonitorEnabled:         true,
		},
		storage.ModelInstructionFamilyClaude: {
			MonitorEnabled: true,
		},
	}}
	gpt := superInstructResponseFeatures(group, "chatgpt-5")
	if !gpt.ResponseRewriteEnabled || !gpt.MonitorEnabled || gpt.MemoryEnabled {
		t.Fatalf("gpt features mismatch: %+v", gpt)
	}
	claude := superInstructResponseFeatures(group, "claude-sonnet-4.6")
	if claude.ResponseRewriteEnabled || !claude.MonitorEnabled || claude.MemoryEnabled {
		t.Fatalf("claude features mismatch: %+v", claude)
	}
	gemini := superInstructResponseFeatures(group, "gemini-3.2-pro")
	if gemini.Enabled() {
		t.Fatalf("gemini should be disabled without a profile: %+v", gemini)
	}
}
