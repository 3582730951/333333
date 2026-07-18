package api

import "testing"

func TestParseCodexRateLimitsEvent(t *testing.T) {
	ev := map[string]interface{}{
		"type":      "codex.rate_limits",
		"plan_type": "pro",
		"rate_limits": map[string]interface{}{
			"primary":   map[string]interface{}{"used_percent": float64(42), "reset_after_seconds": float64(600)},
			"secondary": map[string]interface{}{"used_percent": float64(10), "resets_in_seconds": float64(86400)},
		},
	}
	rl, ok := parseCodexRateLimitsEvent(ev)
	if !ok || !rl.primaryPresent || !rl.secondPresent {
		t.Fatalf("parse failed: %+v ok=%v", rl, ok)
	}
	if rl.primaryUsedPct != 42 || rl.primaryResetSecs != 600 {
		t.Fatalf("primary wrong: %+v", rl)
	}
	if rl.secondUsedPct != 10 || rl.secondResetSecs != 86400 {
		t.Fatalf("secondary wrong: %+v", rl)
	}

	// A window with used_percent but no reset is SKIPPED, so a partial frame never
	// clobbers the complete reset the /wham/usage poll already stored.
	ev2 := map[string]interface{}{"type": "codex.rate_limits", "rate_limits": map[string]interface{}{"primary": map[string]interface{}{"used_percent": float64(99)}}}
	if rl2, ok2 := parseCodexRateLimitsEvent(ev2); ok2 || rl2.primaryPresent {
		t.Fatalf("partial window (no reset) must be skipped: %+v ok=%v", rl2, ok2)
	}

	// Numeric strings are tolerated (Codex sometimes stringifies).
	ev3 := map[string]interface{}{"type": "codex.rate_limits", "rate_limits": map[string]interface{}{"primary": map[string]interface{}{"used_percent": "55", "reset_after_seconds": "120"}}}
	rl3, ok3 := parseCodexRateLimitsEvent(ev3)
	if !ok3 || rl3.primaryUsedPct != 55 || rl3.primaryResetSecs != 120 {
		t.Fatalf("string numerics: %+v ok=%v", rl3, ok3)
	}

	// No rate_limits object → nothing.
	if _, ok4 := parseCodexRateLimitsEvent(map[string]interface{}{"type": "codex.rate_limits"}); ok4 {
		t.Fatal("frame without rate_limits must not parse")
	}
}

func TestCodexRecorderCapturesRateLimitsFrame(t *testing.T) {
	rec := newCodexStreamLedgerRecorder()
	_, _ = rec.Write([]byte("event: codex.rate_limits\n" +
		`data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,"reset_after_seconds":600},"secondary":{"used_percent":7,"reset_after_seconds":86400}}}` + "\n\n"))
	if !rec.rateLimits.any() {
		t.Fatal("recorder did not capture codex.rate_limits frame")
	}
	if rec.rateLimits.primaryUsedPct != 42 || rec.rateLimits.secondUsedPct != 7 {
		t.Fatalf("recorder captured wrong windows: %+v", rec.rateLimits)
	}
	// The frame must not disturb terminal detection (it is not a terminal event).
	if rec.reachedTerminal() {
		t.Fatal("codex.rate_limits must not count as a terminal event")
	}
}
