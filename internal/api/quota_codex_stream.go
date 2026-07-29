package api

import (
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// Real-time Codex quota (workstream B): chatgpt.com/backend-api returns NO x-ratelimit-*
// headers on normal 200s (see quota.go), so the header piggyback that keeps Claude quota
// fresh does nothing for Codex — its quota only updated on the 300s /wham/usage poll,
// which is why "ChatGPT quota is not timely". But Codex streams its live quota in a
// `codex.rate_limits` SSE frame on every response; leakfilter drops that frame from the
// downstream, so we capture it server-side (before it is stripped) and refresh the SAME
// rows the quota view reads (5h_polled / 7d_polled). This is the passive "piggyback"
// track of the dual-track (piggyback + cached poll) design, now working for Codex too.

// codexStreamRateLimits holds the primary (5h) and secondary (7d) windows parsed from a
// codex.rate_limits frame. Only windows that carry both a used-percent and a reset are
// kept, so a partial frame never clobbers the complete data the poll already stored.
type codexStreamRateLimits struct {
	planType         string
	primaryPresent   bool
	primaryUsedPct   float64
	primaryResetSecs int64
	secondPresent    bool
	secondUsedPct    float64
	secondResetSecs  int64
}

func (r codexStreamRateLimits) any() bool { return r.primaryPresent || r.secondPresent }

// parseCodexRateLimitsEvent extracts the windows from a decoded codex.rate_limits event.
// The frame nests windows under "rate_limits":{"primary":{...},"secondary":{...}} where a
// window carries used_percent and one of reset_after_seconds / resets_in_seconds. Values
// may arrive as numbers or numeric strings, so extraction is tolerant.
func parseCodexRateLimitsEvent(ev map[string]interface{}) (codexStreamRateLimits, bool) {
	var out codexStreamRateLimits
	out.planType = streamString(ev["plan_type"])
	rl, ok := ev["rate_limits"].(map[string]interface{})
	if !ok {
		return out, false
	}
	if pw, ok := rl["primary"].(map[string]interface{}); ok {
		if used, hasUsed := codexWindowUsedPercent(pw); hasUsed {
			if reset, hasReset := codexWindowResetSeconds(pw); hasReset {
				out.primaryPresent = true
				out.primaryUsedPct = used
				out.primaryResetSecs = reset
			}
		}
	}
	if sw, ok := rl["secondary"].(map[string]interface{}); ok {
		if used, hasUsed := codexWindowUsedPercent(sw); hasUsed {
			if reset, hasReset := codexWindowResetSeconds(sw); hasReset {
				out.secondPresent = true
				out.secondUsedPct = used
				out.secondResetSecs = reset
			}
		}
	}
	return out, out.any()
}

func codexWindowUsedPercent(w map[string]interface{}) (float64, bool) {
	for _, k := range []string{"used_percent", "usedPercent"} {
		if v, ok := jsonFloatValue(w[k]); ok {
			return v, true
		}
	}
	return 0, false
}

func codexWindowResetSeconds(w map[string]interface{}) (int64, bool) {
	for _, k := range []string{"reset_after_seconds", "resetAfterSeconds", "resets_in_seconds", "resetsInSeconds"} {
		if v, ok := jsonFloatValue(w[k]); ok && v > 0 {
			return int64(v), true
		}
	}
	// window_minutes is a duration, not a reset offset, but on a fresh window it is a safe
	// upper bound so the reset is never left unknown when the frame omits an explicit reset.
	for _, k := range []string{"window_minutes", "windowMinutes"} {
		if v, ok := jsonFloatValue(w[k]); ok && v > 0 {
			return int64(v * 60), true
		}
	}
	return 0, false
}

func jsonFloatValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		end := 0
		if end < len(s) && (s[end] == '-' || s[end] == '+') {
			end++
		}
		seenDot := false
		for end < len(s) {
			c := s[end]
			if c >= '0' && c <= '9' {
				end++
				continue
			}
			if c == '.' && !seenDot {
				seenDot = true
				end++
				continue
			}
			break
		}
		if f, err := strconv.ParseFloat(s[:end], 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// captureCodexStreamRateLimits refreshes the 5h/7d Codex quota rows from a live
// codex.rate_limits frame, mirroring pollOneCodexQuota's mapping so the quota view (which
// selects 5h_polled/7d_polled) sees real-time numbers between polls. Source is tagged
// codex_stream so the origin stays visible. Best-effort and off the request hot path.
func (s *Server) captureCodexStreamRateLimits(accountID string, rl codexStreamRateLimits) {
	if accountID == "" || !rl.any() {
		return
	}
	now := storage.Now()
	var snaps []storage.AccountRateLimit
	if rl.primaryPresent {
		snaps = append(snaps, storage.AccountRateLimit{
			AccountID: accountID, Provider: "codex", LimiterType: "5h_polled", Source: "codex_stream",
			UsedPercent: rl.primaryUsedPct, RemainingTokens: -1, LimitTokens: -1, LimitRequests: -1, RemainingRequests: -1,
			ResetAt: now + rl.primaryResetSecs, UpdatedAt: now,
		})
	}
	if rl.secondPresent {
		snaps = append(snaps, storage.AccountRateLimit{
			AccountID: accountID, Provider: "codex", LimiterType: "7d_polled", Source: "codex_stream",
			UsedPercent: rl.secondUsedPct, RemainingTokens: -1, LimitTokens: -1, LimitRequests: -1, RemainingRequests: -1,
			ResetAt: now + rl.secondResetSecs, UpdatedAt: now,
		})
	}
	for _, snap := range snaps {
		s.scheduler.ApplyRateLimitSnapshot(snap)
		snapCopy := snap
		s.enqueueWrite(func() {
			writeCtx, cancel := s.bgWriteContext()
			defer cancel()
			_ = s.store.UpsertAccountRateLimit(writeCtx, snapCopy)
		})
	}
}
