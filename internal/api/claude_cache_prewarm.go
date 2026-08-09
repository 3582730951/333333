package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
)

func (s *Server) maybePrewarmClaudeCache(ctx context.Context, mode string, req upstream.Request) bool {
	if mode == "off" || s.upstream == nil {
		return false
	}
	path := strings.TrimSpace(req.DownstreamPath)
	if !strings.Contains(path, "/v1/messages") || strings.Contains(path, "count_tokens") || req.PassThrough {
		return false
	}
	raw, err := req.ReadBody()
	if err != nil {
		return false
	}
	body, ok := claudeCachePrewarmBody(raw)
	if !ok {
		return false
	}
	warmReq := req
	warmReq.SetBodyBytes(body)
	warmReq.Headers = req.Headers.Clone()
	run := func(runCtx context.Context) {
		resp, err := s.upstream.Do(runCtx, warmReq)
		if err != nil {
			log.Printf("[CLAUDE-CACHE] prewarm failed: %v", err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	switch mode {
	case "sync_extreme":
		run(ctx)
	case "async":
		timeout := s.cfg.RequestTimeout()
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		bg, cancel := context.WithTimeout(context.Background(), timeout)
		go func() {
			defer cancel()
			defer supervisor.Recover("claude-cache-prewarm")
			run(bg)
		}()
	default:
		return false
	}
	return true
}

func claudeCachePrewarmBody(body []byte) ([]byte, bool) {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return nil, false
	}
	if !claudeZeroMaxTokensPrewarmSupported(root) {
		return nil, false
	}
	root["max_tokens"] = 0
	root["stream"] = false
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	if bytes.Equal(out, body) {
		return out, true
	}
	return out, true
}

// claudeZeroMaxTokensPrewarmSupported reports whether a max_tokens:0 pre-warm of this body
// is a request Anthropic will accept.
//
// Anthropic documents four shapes that make max_tokens:0 an invalid_request_error, "since
// each implies output that a zero-token budget cannot produce": stream:true (which the
// pre-warm already overrides), extended thinking (thinking.type "enabled"), structured
// outputs (output_config.format), and a tool_choice of {"type":"tool"} or {"type":"any"}.
//
// Claude Code sets thinking.type=enabled on every extended-thinking turn, so without this
// check the pre-warm fired a request that was GUARANTEED to 400 — and Anthropic rejects the
// request before writing anything, so the pre-warm could not even do its job. What reached
// the upstream was a duplicate of the real turn's body that always failed validation: an
// elevated 4xx rate on every pooled account, on a request pattern (same prompt sent twice,
// one copy malformed) that the real client never produces.
//
// The bail-out is deliberately conservative — skip the pre-warm rather than mutate the body
// to make it acceptable. Anthropic's own guidance is that the pre-warm must carry the SAME
// thinking configuration and effort as the follow-up request because those values are
// rendered into the prompt; stripping them here would write a cache entry under a different
// prefix, which the real request would then miss while still paying for the write.
func claudeZeroMaxTokensPrewarmSupported(root map[string]interface{}) bool {
	if thinking, ok := root["thinking"].(map[string]interface{}); ok {
		if strings.EqualFold(strings.TrimSpace(stringValue(thinking["type"])), "enabled") {
			return false
		}
	}
	if outputConfig, ok := root["output_config"].(map[string]interface{}); ok {
		if _, hasFormat := outputConfig["format"]; hasFormat {
			return false
		}
	}
	if toolChoice, ok := root["tool_choice"].(map[string]interface{}); ok {
		switch strings.ToLower(strings.TrimSpace(stringValue(toolChoice["type"]))) {
		case "tool", "any":
			return false
		}
	}
	return true
}
