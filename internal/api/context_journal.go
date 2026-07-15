package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

// journalReplayBody creates the stateless Responses body used when account-local
// previous_response_id state is unavailable. Payloads are encrypted by storage.
func (s *Server) journalReplayBody(ctx context.Context, current []byte) ([]byte, bool) {
	var cur map[string]interface{}
	if json.Unmarshal(current, &cur) != nil {
		return current, false
	}
	prev, _ := cur["previous_response_id"].(string)
	if strings.TrimSpace(prev) == "" {
		return current, false
	}
	j, err := s.store.GetContextJournal(ctx, prev)
	if err != nil {
		return current, false
	}
	var base map[string]interface{}
	if json.Unmarshal([]byte(j.Payload), &base) != nil {
		return current, false
	}
	base["model"] = cur["model"]
	for _, k := range []string{"instructions", "tools", "reasoning", "stream", "include"} {
		if v, ok := cur[k]; ok {
			base[k] = v
		}
	}
	base["input"] = appendItems(base["input"], cur["input"])
	delete(base, "previous_response_id")
	out, e := json.Marshal(base)
	return out, e == nil
}

func appendItems(a, b interface{}) []interface{} {
	out := []interface{}{}
	if x, ok := a.([]interface{}); ok {
		out = append(out, x...)
	} else if a != nil {
		out = append(out, a)
	}
	if x, ok := b.([]interface{}); ok {
		out = append(out, x...)
	} else if b != nil {
		out = append(out, b)
	}
	return out
}

func (s *Server) persistContextJournal(ctx context.Context, requestBody, responseBody []byte, affinityHash, accountID string) error {
	var req, resp map[string]interface{}
	if json.Unmarshal(requestBody, &req) != nil || json.Unmarshal(responseBody, &resp) != nil {
		return errors.New("invalid context journal payload")
	}
	id, _ := resp["id"].(string)
	if id == "" {
		return errors.New("context journal response id missing")
	}
	if prev, _ := req["previous_response_id"].(string); prev != "" {
		if j, e := s.store.GetContextJournal(ctx, prev); e == nil {
			var base map[string]interface{}
			if json.Unmarshal([]byte(j.Payload), &base) == nil {
				base["input"] = appendItems(base["input"], req["input"])
				req = base
			}
		}
	}
	delete(req, "previous_response_id")
	if output, ok := resp["output"]; ok {
		req["input"] = appendItems(req["input"], output)
	}
	payload, e := json.Marshal(req)
	if e != nil {
		return e
	}
	ttl := s.settingInt(ctx, "context_journal_ttl_seconds", s.cfg.ContextJournalTTLSeconds)
	if ttl <= 0 {
		ttl = 3600
	}
	return s.store.PutContextJournal(ctx, storage.ContextJournal{ResponseID: id, AffinityHash: affinityHash, AccountID: accountID, Payload: string(payload), ExpiresAt: time.Now().Add(time.Duration(ttl) * time.Second).Unix()})
}

func ensureEncryptedReasoningInclude(body []byte) []byte {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	items, _ := root["include"].([]interface{})
	for _, v := range items {
		if v == "reasoning.encrypted_content" {
			return body
		}
	}
	root["include"] = append(items, "reasoning.encrypted_content")
	out, e := json.Marshal(root)
	if e != nil {
		return body
	}
	return out
}

func degradedResponsesReplay(body []byte) []byte {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	delete(root, "previous_response_id")
	delete(root, "turn_state")
	out, e := json.Marshal(root)
	if e != nil {
		return body
	}
	return out
}
