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
	body, ok := claudeCachePrewarmBody(req.Body)
	if !ok {
		return false
	}
	warmReq := req
	warmReq.Body = body
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
