package virtual

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

type Planner struct {
	store *storage.Store
	cfg   config.Config
}

type MaterializeResult struct {
	Body               []byte
	Changed            bool
	OriginalTokens     int64
	MaterializedTokens int64
	IncludedLedger     int
}

type RebuildResult struct {
	Body           []byte
	IncludedLedger int
}

func NewPlanner(store *storage.Store, cfg config.Config) *Planner {
	return &Planner{store: store, cfg: cfg}
}

func EstimateTokensText(s string) int64 {
	if s == "" {
		return 0
	}
	// Conservative no-tokenizer estimate; sufficient for planning gates and tests.
	// Count runes without materializing a []rune copy of s (identical to len([]rune(s))).
	return int64(utf8.RuneCountInString(s)/4 + 1)
}

// EstimateTokensJSON estimates tokens for a raw JSON body. It counts runes directly
// over the bytes (utf8.RuneCount), avoiding the full string + []rune copies of the
// whole body that string(raw)/[]rune(...) would allocate — this runs twice per request
// (route estimate + billing hold) on bodies up to MaxBodyBytes, so the saved allocation
// is real GC pressure. The returned count is identical to len([]rune(string(raw))).
func EstimateTokensJSON(raw []byte) int64 {
	return int64(utf8.RuneCount(raw)/4 + 1)
}

func (p *Planner) RecordRequest(ctx context.Context, routeKeyHash, accountID string, body []byte) {
	if routeKeyHash == "" || accountID == "" || len(body) == 0 {
		return
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return
	}
	model, _ := root["model"].(string)
	promptCacheKey, _ := root["prompt_cache_key"].(string)
	input := root["input"]
	if input == nil {
		input = root["messages"]
	}
	for _, item := range flattenInput(input) {
		raw, _ := json.Marshal(item.Raw)
		_ = p.store.InsertVirtualLedger(ctx, storage.VirtualLedgerItem{
			RouteKeyHash:   routeKeyHash,
			AccountID:      accountID,
			Model:          model,
			PromptCacheKey: promptCacheKey,
			Role:           item.Role,
			Content:        item.Content,
			TokenEstimate:  EstimateTokensText(item.Content),
			RawJSON:        string(raw),
		})
	}
}

func (p *Planner) RecordRawRequest(ctx context.Context, routeKeyHash, accountID, model, promptCacheKey string, body []byte) {
	if routeKeyHash == "" || accountID == "" || len(body) == 0 {
		return
	}
	_ = p.store.InsertVirtualLedger(ctx, storage.VirtualLedgerItem{
		RouteKeyHash:   routeKeyHash,
		AccountID:      accountID,
		Model:          model,
		PromptCacheKey: promptCacheKey,
		Role:           "raw_request",
		Content:        "",
		TokenEstimate:  EstimateTokensJSON(body),
		RawJSON:        string(body),
	})
}

func (p *Planner) RecordRawResponse(ctx context.Context, routeKeyHash, accountID, model, promptCacheKey string, body []byte) {
	if routeKeyHash == "" || accountID == "" || len(body) == 0 {
		return
	}
	_ = p.store.InsertVirtualLedger(ctx, storage.VirtualLedgerItem{
		RouteKeyHash:   routeKeyHash,
		AccountID:      accountID,
		Model:          model,
		PromptCacheKey: promptCacheKey,
		Role:           "raw_response",
		Content:        "",
		TokenEstimate:  EstimateTokensJSON(body),
		RawJSON:        string(body),
	})
}

func (p *Planner) RebuildStatefulRequest(ctx context.Context, routeKeyHash string, current []byte) (RebuildResult, bool) {
	if routeKeyHash == "" || len(current) == 0 {
		return RebuildResult{}, false
	}
	var currentRoot map[string]interface{}
	if json.Unmarshal(current, &currentRoot) != nil {
		return RebuildResult{}, false
	}
	currentItems, ok := responsesInputItems(currentRoot["input"])
	if !ok || len(currentItems) == 0 {
		return RebuildResult{}, false
	}
	ledger, err := p.store.ListVirtualLedger(ctx, routeKeyHash, 200)
	if err != nil || len(ledger) == 0 {
		return RebuildResult{}, false
	}

	var input []interface{}
	haveBase := false
	havePriorResponse := false
	currentSeen := false
	included := 0
	for _, item := range ledger {
		switch item.Role {
		case "raw_request":
			var root map[string]interface{}
			if json.Unmarshal([]byte(item.RawJSON), &root) != nil {
				continue
			}
			items, ok := responsesInputItems(root["input"])
			if !ok || len(items) == 0 {
				continue
			}
			if !requestUsesServerState(root) {
				input = append([]interface{}{}, items...)
				haveBase = true
				included = 1
			} else if haveBase {
				input = append(input, items...)
				included++
			}
			if strings.TrimSpace(item.RawJSON) == strings.TrimSpace(string(current)) {
				currentSeen = true
			}
		case "raw_response":
			if !haveBase {
				continue
			}
			items := responseOutputItems([]byte(item.RawJSON))
			if len(items) == 0 {
				continue
			}
			input = append(input, items...)
			havePriorResponse = true
			included++
		}
	}
	if !currentSeen && haveBase {
		input = append(input, currentItems...)
		included++
	}
	if !haveBase || !havePriorResponse || len(input) == 0 {
		return RebuildResult{}, false
	}
	delete(currentRoot, "previous_response_id")
	currentRoot["input"] = input
	out, err := json.Marshal(currentRoot)
	if err != nil {
		return RebuildResult{}, false
	}
	return RebuildResult{Body: out, IncludedLedger: included}, true
}

func (p *Planner) MaterializeIfNeeded(ctx context.Context, routeKeyHash, accountID string, nativeWindow int64, body []byte) (MaterializeResult, error) {
	originalTokens := EstimateTokensJSON(body)
	if !p.cfg.Virtual2MEnabled || nativeWindow <= 0 || originalTokens <= nativeWindow {
		return MaterializeResult{Body: body, OriginalTokens: originalTokens, MaterializedTokens: originalTokens}, nil
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return MaterializeResult{}, err
	}
	input, ok := root["input"].([]interface{})
	if !ok {
		return MaterializeResult{Body: body, OriginalTokens: originalTokens, MaterializedTokens: originalTokens}, nil
	}
	if requiresVerbatimResponsesInput(input) {
		return MaterializeResult{Body: body, OriginalTokens: originalTokens, MaterializedTokens: originalTokens}, nil
	}
	budget := nativeWindow
	if budget > 4096 {
		budget -= 2048
	}
	selected := make([]interface{}, 0, len(input))
	var used int64

	if instructions, ok := root["instructions"].(string); ok {
		used += EstimateTokensText(instructions)
	}

	current := lastUserLike(input)
	if current != nil {
		selected = append(selected, current)
		used += EstimateTokensText(contentOf(current))
	}

	for i := len(input) - 1; i >= 0; i-- {
		if current != nil && samePointerish(input[i], current) {
			continue
		}
		itemTokens := EstimateTokensText(contentOf(input[i]))
		if used+itemTokens > budget {
			continue
		}
		selected = append([]interface{}{input[i]}, selected...)
		used += itemTokens
	}

	ledger := []storage.VirtualLedgerItem(nil)
	if routeKeyHash != "" {
		// Only pool prior turns when this request carries a true per-conversation
		// correlator (a non-empty ledger key). For weak/heuristic affinity the
		// caller passes "" so sibling agents that merely share a coarse key are
		// never merged into one reconstructed context (multi-agent contamination
		// guard).
		ledger, _ = p.store.ListVirtualLedger(ctx, routeKeyHash, 200)
	}
	includedLedger := 0
	for i := len(ledger) - 1; i >= 0; i-- {
		item := ledger[i]
		if used+item.TokenEstimate > budget {
			continue
		}
		var raw interface{}
		if item.RawJSON != "" && json.Unmarshal([]byte(item.RawJSON), &raw) == nil {
			for _, candidate := range ledgerCandidates(raw) {
				candidateTokens := EstimateTokensText(contentOf(candidate))
				if candidateTokens == 0 {
					candidateTokens = EstimateTokensJSON(mustJSON(candidate))
				}
				if used+candidateTokens > budget {
					continue
				}
				selected = append([]interface{}{candidate}, selected...)
				used += candidateTokens
				includedLedger++
			}
		}
	}

	if len(selected) == 0 {
		selected = input[len(input)-1:]
	}
	root["input"] = selected
	// The materialization summary is returned in MaterializeResult for telemetry
	// but deliberately NOT written into the upstream body: /v1/responses rejects
	// unknown top-level fields, and any extra field perturbs the cached prompt
	// prefix. Keeping the body to known fields preserves prompt-cache hits.
	out, err := json.Marshal(root)
	if err != nil {
		return MaterializeResult{}, err
	}
	return MaterializeResult{
		Body:               out,
		Changed:            true,
		OriginalTokens:     originalTokens,
		MaterializedTokens: used,
		IncludedLedger:     includedLedger,
	}, nil
}

func ledgerCandidates(raw interface{}) []interface{} {
	if m, ok := raw.(map[string]interface{}); ok {
		if arr, ok := m["input"].([]interface{}); ok {
			return arr
		}
		if arr, ok := m["messages"].([]interface{}); ok {
			return arr
		}
	}
	return []interface{}{raw}
}

func requestUsesServerState(root map[string]interface{}) bool {
	v, ok := root["previous_response_id"].(string)
	return ok && strings.TrimSpace(v) != ""
}

func responsesInputItems(input interface{}) ([]interface{}, bool) {
	switch t := input.(type) {
	case []interface{}:
		return append([]interface{}{}, t...), true
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, false
		}
		return []interface{}{map[string]interface{}{"role": "user", "content": t}}, true
	case map[string]interface{}:
		return []interface{}{t}, true
	default:
		return nil, false
	}
}

func responseOutputItems(raw []byte) []interface{} {
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	if arr, ok := root["output"].([]interface{}); ok && len(arr) > 0 {
		out := make([]interface{}, 0, len(arr))
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if typ, _ := m["type"].(string); typ == "message" {
					if _, ok := m["role"]; !ok {
						m["role"] = "assistant"
					}
				}
			}
			out = append(out, item)
		}
		return out
	}
	if text, ok := root["output_text"].(string); ok && strings.TrimSpace(text) != "" {
		return []interface{}{map[string]interface{}{
			"type": "message",
			"role": "assistant",
			"content": []interface{}{map[string]interface{}{
				"type": "output_text",
				"text": text,
			}},
		}}
	}
	return nil
}

func requiresVerbatimResponsesInput(input []interface{}) bool {
	for _, item := range input {
		if !plainResponsesMessageItem(item) {
			return true
		}
	}
	return false
}

func plainResponsesMessageItem(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok {
		_, ok = v.(string)
		return ok
	}
	typ, _ := m["type"].(string)
	role, _ := m["role"].(string)
	if typ != "" && typ != "message" && typ != "input_text" {
		return false
	}
	if role != "" && role != "user" && role != "assistant" && role != "system" && role != "developer" {
		return false
	}
	if content, ok := m["content"].([]interface{}); ok {
		for _, part := range content {
			if !plainResponsesContentPart(part) {
				return false
			}
		}
	}
	return true
}

func plainResponsesContentPart(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok {
		_, ok = v.(string)
		return ok
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "", "text", "input_text", "output_text":
		return true
	default:
		return false
	}
}

func mustJSON(v interface{}) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

type inputItem struct {
	Role    string
	Content string
	Raw     interface{}
}

func flattenInput(input interface{}) []inputItem {
	switch t := input.(type) {
	case []interface{}:
		out := make([]inputItem, 0, len(t))
		for _, item := range t {
			out = append(out, inputItem{Role: roleOf(item), Content: contentOf(item), Raw: item})
		}
		return out
	case string:
		return []inputItem{{Role: "user", Content: t, Raw: map[string]interface{}{"role": "user", "content": t}}}
	default:
		return nil
	}
}

func lastUserLike(input []interface{}) interface{} {
	for i := len(input) - 1; i >= 0; i-- {
		role := roleOf(input[i])
		typ := typeOf(input[i])
		if role == "user" || typ == "message" || typ == "input_text" {
			return input[i]
		}
	}
	if len(input) > 0 {
		return input[len(input)-1]
	}
	return nil
}

func roleOf(v interface{}) string {
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m["role"].(string); ok {
			return s
		}
	}
	return ""
}

func typeOf(v interface{}) string {
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m["type"].(string); ok {
			return s
		}
	}
	return ""
}

func contentOf(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		if s, ok := t["content"].(string); ok {
			return s
		}
		if s, ok := t["text"].(string); ok {
			return s
		}
		if arr, ok := t["content"].([]interface{}); ok {
			var parts []string
			for _, item := range arr {
				parts = append(parts, contentOf(item))
			}
			return strings.Join(parts, "\n")
		}
	case []interface{}:
		var parts []string
		for _, item := range t {
			parts = append(parts, contentOf(item))
		}
		return strings.Join(parts, "\n")
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func samePointerish(a, b interface{}) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}
