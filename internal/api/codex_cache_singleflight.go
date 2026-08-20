package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/supervisor"
	"github.com/tidwall/gjson"
)

type codexCacheFlight struct {
	done          chan struct{}
	releaseReason string
}

type codexCacheFlightAdmission struct {
	Waited        bool
	WaitReason    string
	ReleaseReason string
	PrefixSource  string
	PrefixHash    string
	release       func(string)
	releaseOnce   sync.Once
}

func (a *codexCacheFlightAdmission) Release(reason string) {
	if a == nil || a.release == nil {
		return
	}
	a.releaseOnce.Do(func() { a.release(reason) })
}

func (s *Server) enterCodexCacheSingleflight(ctx context.Context, enabled bool, accountID, model string, body []byte, metadata ...*bodysource.BodyMeta) *codexCacheFlightAdmission {
	admission := &codexCacheFlightAdmission{release: func(string) {}}
	if !enabled {
		return admission
	}
	var meta *bodysource.BodyMeta
	if len(metadata) > 0 {
		meta = metadata[0]
	}
	promptCacheKey := strings.TrimSpace(promptCacheKeyWithMeta(body, meta))
	if promptCacheKey == "" {
		return admission
	}
	prefixHash, prefixSource := codexCacheCoordinationPrefix(body)
	admission.PrefixHash = prefixHash
	admission.PrefixSource = prefixSource
	if prefixHash == "" {
		return admission
	}
	key := strings.Join([]string{strings.TrimSpace(accountID), strings.TrimSpace(model), promptCacheKey, prefixHash}, "\x00")
	s.codexCacheFlightsMu.Lock()
	if s.codexCacheFlights == nil {
		s.codexCacheFlights = make(map[string]*codexCacheFlight)
	}
	if existing := s.codexCacheFlights[key]; existing != nil {
		s.codexCacheFlightsMu.Unlock()
		admission.Waited = true
		admission.WaitReason = "matching_cold_prefix"
		timer := time.NewTimer(cacheSingleflightMaxWait)
		defer timer.Stop()
		select {
		case <-existing.done:
			admission.ReleaseReason = existing.releaseReason
		case <-timer.C:
			admission.ReleaseReason = "follower_max_wait"
		case <-ctx.Done():
			admission.ReleaseReason = "follower_context_done"
		}
		return admission
	}
	if len(s.codexCacheFlights) >= cacheSingleflightMaxFlights {
		s.codexCacheFlightsMu.Unlock()
		admission.WaitReason = "flight_limit"
		return admission
	}
	flight := &codexCacheFlight{done: make(chan struct{})}
	s.codexCacheFlights[key] = flight
	s.codexCacheFlightsMu.Unlock()
	admission.release = func(reason string) {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			reason = "request_complete"
		}
		s.codexCacheFlightsMu.Lock()
		if current := s.codexCacheFlights[key]; current == flight {
			flight.releaseReason = reason
			admission.ReleaseReason = reason
			delete(s.codexCacheFlights, key)
			close(flight.done)
		}
		s.codexCacheFlightsMu.Unlock()
	}
	time.AfterFunc(cacheSingleflightMaxWait, func() {
		defer supervisor.Recover("codex-cache-singleflight-release")
		admission.Release("leader_max_wait")
	})
	return admission
}

// codexCacheCoordinationPrefix fingerprints exactly the request prefix governed by
// the current explicit breakpoint. Without an explicit marker it deliberately uses
// the complete input: singleflight then coordinates only byte-equivalent cold
// requests, never distinct turns or tool results that happen to share a root thread.
func codexCacheCoordinationPrefix(raw []byte) (string, string) {
	var root map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return "", ""
	}
	var material bytes.Buffer
	wrote := false
	for _, field := range []string{"instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "prompt_cache_options"} {
		if value, ok := root[field]; ok {
			material.WriteString(field)
			material.WriteByte(0)
			material.Write(value)
			material.WriteByte(0)
			wrote = true
		}
	}
	input, ok := root["input"]
	if ok {
		prefix, explicit := codexInputPrefixAtBreakpoint(input)
		material.WriteString("input")
		material.WriteByte(0)
		material.Write(prefix)
		wrote = true
		if !wrote {
			return "", ""
		}
		sum := sha256.Sum256(material.Bytes())
		if explicit {
			return hex.EncodeToString(sum[:]), "explicit_breakpoint"
		}
		return hex.EncodeToString(sum[:]), "implicit_exact_prefix"
	}
	if !wrote {
		return "", ""
	}
	sum := sha256.Sum256(material.Bytes())
	return hex.EncodeToString(sum[:]), "implicit_exact_prefix"
}

func codexInputPrefixAtBreakpoint(raw json.RawMessage) ([]byte, bool) {
	input := bytes.TrimSpace(raw)
	array := gjson.ParseBytes(input)
	if array.Type != gjson.JSON || len(input) == 0 || input[0] != '[' {
		return raw, false
	}
	markerEnd := -1
	closeSuffix := ""
	array.ForEach(func(_, item gjson.Result) bool {
		if item.Index <= 0 || item.Raw == "" {
			return true
		}
		if item.Get("prompt_cache_breakpoint").Exists() {
			markerEnd = item.Index + len(item.Raw)
			closeSuffix = "]"
		}
		content := item.Get("content")
		if content.Type != gjson.JSON || len(content.Raw) == 0 || content.Raw[0] != '[' {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			if block.Index > 0 && block.Get("prompt_cache_breakpoint").Exists() {
				markerEnd = block.Index + len(block.Raw)
				// The marker is inside content, so close content, the item, and
				// the enclosing input array after the exact raw block bytes.
				closeSuffix = "]}}]"
			}
			return true
		})
		return true
	})
	if markerEnd < 0 || markerEnd > len(input) {
		return raw, false
	}
	arrayStart := array.Index
	if arrayStart < 0 || arrayStart >= markerEnd || input[arrayStart] != '[' {
		arrayStart = 0
	}
	prefix := make([]byte, 0, markerEnd-arrayStart+len(closeSuffix))
	prefix = append(prefix, input[arrayStart:markerEnd]...)
	prefix = append(prefix, closeSuffix...)
	return prefix, true
}
