package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"codex-account-pool/internal/anthropicwire"
	"codex-account-pool/internal/storage"
)

// moderation.go is the compliance content-moderation layer. When enabled, each
// incoming request's conversation HISTORY (prior assistant turns) is keyword-scanned;
// on a hit the admin-configured pool model is asked to rewrite the offending text
// while preserving code verbatim, BEFORE the request is forwarded upstream. The live
// streamed reply is never touched (the operator's requirement); dangerous content is
// instead sanitized out of the history it carries on the next turn.

const moderationSafeSentinel = "__SAFE__"

// moderationSystemPrompt instructs the configured model to act as a safety rewriter
// that NEVER alters code. It is given the operator's word/topic list as the detection
// target.
func moderationSystemPrompt(words []string) string {
	return "You are a strict content-safety filter for a CODING ASSISTANT's conversation history. " +
		"You will receive one prior assistant message. Detect whether it contains unsafe, dangerous, " +
		"or disallowed content related to any of these terms/topics: [" + strings.Join(words, ", ") + "]. " +
		"RULES:\n" +
		"1. CRITICAL — preserve ALL code EXACTLY, byte-for-byte: fenced code blocks (```...```), inline code, " +
		"diffs/patches, file paths, file contents, shell commands, JSON and tool arguments must be returned UNCHANGED.\n" +
		"2. Only rewrite/neutralize the UNSAFE natural-language prose; keep the original language, tone and formatting.\n" +
		"3. If the message contains nothing unsafe, reply with EXACTLY: " + moderationSafeSentinel + "\n" +
		"4. Otherwise reply with ONLY the cleaned message text — no preamble, no explanation, no markdown fences around the whole reply."
}

// containsAnyWord reports whether b contains any of words (case-insensitive substring).
func containsAnyWord(b []byte, words []string) bool {
	if len(words) == 0 {
		return false
	}
	hay := bytes.ToLower(b)
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if bytes.Contains(hay, bytes.ToLower([]byte(w))) {
			return true
		}
	}
	return false
}

// internalComplete runs a one-shot, non-streaming Chat-Completions request for the
// given model THROUGH the relay's own gateway (in-memory request + recorder) and
// returns the assistant text. Reusing the gateway means any pool model — custom
// (DeepSeek/SiliconFlow…), Claude, or Codex — routes and converts correctly. The
// request is marked internal so it skips history moderation (no recursion) and the
// require-downstream-key gate.
func (s *Server) internalComplete(ctx context.Context, model, system, user string) (string, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
		"stream":     false,
		"max_tokens": 8192,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	// Mark internal so it skips history moderation (no recursion) + the require-key gate.
	req = req.WithContext(withInternal(ctx))
	rec := httptest.NewRecorder()
	s.handleGatewayPost(rec, req)
	if rec.Code >= 400 {
		return "", fmt.Errorf("moderation model %q failed: HTTP %d %s", model, rec.Code, bodySnippet(rec.Body.Bytes(), 200))
	}
	var root map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		return "", fmt.Errorf("moderation model %q: bad response: %w", model, err)
	}
	if choices, ok := root["choices"].([]interface{}); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := ch["message"].(map[string]interface{}); ok {
				if c, ok := msg["content"].(string); ok {
					return c, nil
				}
			}
		}
	}
	return "", errors.New("moderation model returned no content")
}

// moderateHistory sanitizes the conversation history in an incoming request body. It
// is a zero-cost pass-through unless moderation is enabled, configured with words +
// model, and a configured word actually appears in the body. shape is "chat",
// "responses" or "anthropic". Best-effort: any model/parse error fails OPEN (returns
// the original body) so a moderation hiccup never blocks live traffic.
func (s *Server) moderateHistory(ctx context.Context, raw []byte, shape string) []byte {
	if isInternalCall(ctx) {
		return raw
	}
	cfg, err := s.store.GetModerationConfig(ctx)
	if err != nil || !cfg.Enabled || len(cfg.Words) == 0 || strings.TrimSpace(cfg.Model) == "" {
		return raw
	}
	if !containsAnyWord(raw, cfg.Words) {
		return raw
	}
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	changed := false
	sys := moderationSystemPrompt(cfg.Words)
	rewrite := func(text string) string {
		if strings.TrimSpace(text) == "" || !containsAnyWord([]byte(text), cfg.Words) {
			return text
		}
		out, err := s.internalComplete(ctx, cfg.Model, sys, text)
		if err != nil {
			log.Printf("moderation: model %q call failed (history left unchanged): %v", cfg.Model, err)
			return text
		}
		out = strings.TrimSpace(out)
		if out == "" || out == moderationSafeSentinel {
			return text
		}
		changed = true
		return out
	}
	switch shape {
	case "responses":
		moderateResponsesInput(root["input"], rewrite)
	default: // chat / anthropic both use messages[]
		moderateMessages(root["messages"], rewrite)
	}
	if !changed {
		return raw
	}
	var out []byte
	if shape == "anthropic" {
		out, err = anthropicwire.MarshalPreservingOrder(raw, root)
	} else {
		out, err = json.Marshal(root)
	}
	if err != nil {
		return raw
	}
	return out
}

// moderateMessages rewrites the text of assistant-role messages in a chat/anthropic
// messages[] array, in place. User and system turns are left untouched.
func moderateMessages(v interface{}, rewrite func(string) string) {
	arr, ok := v.([]interface{})
	if !ok {
		return
	}
	for _, mi := range arr {
		m, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		if r, _ := m["role"].(string); r != "assistant" {
			continue
		}
		m["content"] = rewriteContentField(m["content"], rewrite)
	}
}

// moderateResponsesInput rewrites assistant message items in a Responses input[]
// array, in place. function_call / function_call_output / reasoning items (which
// carry tool data and code) are never touched.
func moderateResponsesInput(v interface{}, rewrite func(string) string) {
	arr, ok := v.([]interface{})
	if !ok {
		return
	}
	for _, it := range arr {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		role, _ := m["role"].(string)
		if (typ == "message" || typ == "") && role == "assistant" {
			m["content"] = rewriteContentField(m["content"], rewrite)
		}
	}
}

// rewriteContentField applies rewrite to a message's content, preserving structure: a
// plain string is rewritten directly; an array of parts has only its text-type parts'
// text rewritten (tool_use / image / non-text parts are left intact). The rewrite fn
// itself no-ops on text that contains no configured word.
func rewriteContentField(content interface{}, rewrite func(string) string) interface{} {
	switch c := content.(type) {
	case string:
		return rewrite(c)
	case []interface{}:
		for _, pi := range c {
			p, ok := pi.(map[string]interface{})
			if !ok {
				continue
			}
			typ, _ := p["type"].(string)
			switch typ {
			case "text", "output_text", "input_text":
				if txt, ok := p["text"].(string); ok {
					p["text"] = rewrite(txt)
				}
			}
		}
		return c
	}
	return content
}

// hasCJK reports whether s contains any CJK ideograph (used to decide auto-translation).
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// ── admin endpoints ──

// adminModeration is GET/POST /admin/moderation — read or replace the moderation config.
func (s *Server) adminModeration(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.store.GetModerationConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if cfg.Words == nil {
			cfg.Words = []string{}
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost, http.MethodPatch:
		var req storage.ModerationConfig
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Model = strings.TrimSpace(req.Model)
		if err := s.store.SetModerationConfig(r.Context(), req); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out, err := s.store.GetModerationConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if out.Words == nil {
			out.Words = []string{}
		}
		writeJSON(w, http.StatusOK, out)
	default:
		methodNotAllowed(w)
	}
}

// adminModerationTranslate is POST /admin/moderation/translate {word} — translate a
// (typically Chinese) detection word to English via the configured model, so the UI
// can auto-append the English term. Returns {translations:[...]}.
func (s *Server) adminModerationTranslate(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Word  string `json:"word"`
		Model string `json:"model"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	word := strings.TrimSpace(req.Word)
	if word == "" || !hasCJK(word) {
		writeJSON(w, http.StatusOK, map[string]interface{}{"translations": []string{}})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		cfg, err := s.store.GetModerationConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if cfg.Model != "" {
			model = cfg.Model
		}
	}
	if model == "" {
		writeError(w, http.StatusBadRequest, errors.New("no moderation model configured"))
		return
	}
	out, err := s.internalComplete(r.Context(), model,
		"You translate a content-moderation keyword to English. Reply with ONLY the English word or short phrase, no explanation, no quotes.",
		word)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	en := strings.TrimSpace(strings.Trim(strings.TrimSpace(out), "\"'`"))
	res := []string{}
	if en != "" && !strings.EqualFold(en, word) && len(en) < 80 {
		res = append(res, en)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"translations": res})
}
