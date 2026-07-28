package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/virtual"
	"github.com/tidwall/gjson"
)

func affinityWithMeta(r *http.Request, raw []byte, meta *bodysource.BodyMeta) routing.AffinityKey {
	if meta == nil {
		return routing.ExtractAffinityKey(r, raw)
	}
	header := func(name string) string {
		if r == nil {
			return ""
		}
		return strings.TrimSpace(r.Header.Get(name))
	}
	if value := firstNonEmpty(header("x-codex-parent-thread-id"), header("thread-id")); value != "" {
		return routing.AffinityFromKey(routing.CodexRootThreadAffinitySource+":"+value, routing.CodexRootThreadAffinitySource)
	}
	if value := strings.TrimSpace(meta.ThreadID); value != "" {
		return routing.AffinityFromKey("thread_id:"+value, "thread_id")
	}
	if value := strings.TrimSpace(meta.ConversationID); value != "" {
		return routing.AffinityFromKey("conversation_id:"+value, "conversation_id")
	}
	if value := header("x-codex-window-id"); value != "" {
		return routing.AffinityFromKey("x-codex-window-id:"+value, "x-codex-window-id")
	}
	if value := strings.TrimSpace(meta.PromptCacheKey); value != "" {
		return routing.AffinityFromKey("prompt_cache_key:"+value, "prompt_cache_key")
	}
	if value := strings.TrimSpace(meta.PreviousResponseID); value != "" {
		return routing.ResponseAffinityKey(value)
	}
	if value := header("x-codex-turn-metadata"); value != "" {
		return routing.AffinityFromKey("x-codex-turn-metadata:"+value, "x-codex-turn-metadata")
	}
	return routing.ExtractAffinityKey(r, raw)
}

func bodyMetaForView(meta *bodysource.BodyMeta, original, current []byte) *bodysource.BodyMeta {
	if meta == nil || meta.Size != int64(len(current)) || len(original) != len(current) {
		return nil
	}
	if len(current) > 0 && &original[0] != &current[0] {
		return nil
	}
	return meta
}

func streamRequestWithMeta(raw []byte, meta *bodysource.BodyMeta) bool {
	if meta != nil {
		return meta.StreamPresent && meta.Stream
	}
	return isStreamRequest(raw)
}

func modelWithMeta(raw []byte, meta *bodysource.BodyMeta) string {
	if meta != nil {
		return meta.Model
	}
	return routing.Model(raw)
}

func promptCacheKeyWithMeta(raw []byte, meta *bodysource.BodyMeta) string {
	if meta != nil {
		return meta.PromptCacheKey
	}
	return routing.PromptCacheKey(raw)
}

func topLevelStringWithMeta(raw []byte, meta *bodysource.BodyMeta, key string) string {
	if meta == nil {
		return routing.JSONStringField(raw, key)
	}
	switch key {
	case "model":
		return meta.Model
	case "prompt_cache_key":
		return meta.PromptCacheKey
	case "previous_response_id":
		return meta.PreviousResponseID
	case "conversation_id":
		return meta.ConversationID
	case "session_id":
		return meta.SessionID
	case "thread_id":
		return meta.ThreadID
	}
	var value string
	if scalar := meta.Scalars[key]; len(scalar) > 0 && json.Unmarshal(scalar, &value) == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

func compactionRequestWithMeta(path string, raw []byte, meta *bodysource.BodyMeta) bool {
	if strings.Contains(path, "/responses/compact") {
		return true
	}
	if meta != nil {
		return meta.CompactionTrigger
	}
	return routing.IsCompaction(path, raw)
}

func serverSideStateWithMeta(path string, r *http.Request, raw []byte, meta *bodysource.BodyMeta) bool {
	if r != nil && strings.TrimSpace(r.Header.Get("X-Codex-Turn-State")) != "" {
		return true
	}
	if meta != nil {
		return meta.PreviousResponseID != ""
	}
	return routing.HasServerSideState(path, r, raw)
}

func strictStickyWithMeta(path string, r *http.Request, raw []byte, meta *bodysource.BodyMeta) bool {
	if serverSideStateWithMeta(path, r, raw, meta) || compactionRequestWithMeta(path, raw, meta) {
		return true
	}
	if meta != nil {
		return meta.ClientToolResult
	}
	return routing.IsStrictSticky(path, r, raw)
}

func responsesReasoningEffortWithMeta(raw []byte, meta *bodysource.BodyMeta) string {
	if meta == nil {
		return requestedResponsesReasoningEffort(raw)
	}
	span, present := meta.Fields["reasoning"]
	if !present || span.Offset < 0 || span.Length <= 0 || span.Offset > int64(len(raw)) || span.Length > int64(len(raw))-span.Offset {
		return ""
	}
	value := gjson.GetBytes(raw[span.Offset:span.Offset+span.Length], "effort")
	if value.Type != gjson.String {
		return ""
	}
	return normalizeEffort(value.String())
}

func estimatedTokensWithMeta(raw []byte, meta *bodysource.BodyMeta) int64 {
	if meta != nil && meta.EstimatedTokens > 0 {
		return meta.EstimatedTokens
	}
	return virtual.EstimateTokensJSON(raw)
}

func (s *Server) responseBodyCaptureOptions(ctx context.Context) bodysource.CaptureOptions {
	_ = ctx
	return bodysource.CaptureOptions{
		MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes,
		TempDir: s.cfg.BodySpoolDir, Budget: s.responseBodyBudget, DiskReserver: s.bodyDiskReserver,
		TempFileNamePrefix: "codex-pool-response-*",
	}
}
