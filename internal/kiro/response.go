package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type ToolCall struct{ ID, Name, Input string }
type WebSearchResult struct {
	Title, URL, Snippet string
	PublishedAt         int64
}
type WebSearchData struct {
	Query, ToolUseID string
	Results          []WebSearchResult
}
type ResponseData struct {
	Text, Thinking                                                  string
	Tools                                                           []ToolCall
	WebSearch                                                       *WebSearchData
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64
	StopReason                                                      string
}

func DecodeWebSearchResponse(raw []byte, request WebSearchRequest, inputTokens int64) (ResponseData, error) {
	var envelope struct {
		Error  interface{} `json:"error"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ResponseData{}, err
	}
	if envelope.Error != nil {
		return ResponseData{}, fmt.Errorf("kiro MCP error: %v", envelope.Error)
	}
	data := &WebSearchData{Query: request.Query, ToolUseID: request.ToolUseID}
	for _, content := range envelope.Result.Content {
		if content.Type != "text" || strings.TrimSpace(content.Text) == "" {
			continue
		}
		var decoded struct {
			Results []struct {
				Title         string `json:"title"`
				URL           string `json:"url"`
				Snippet       string `json:"snippet"`
				PublishedDate int64  `json:"publishedDate"`
			} `json:"results"`
		}
		if json.Unmarshal([]byte(content.Text), &decoded) == nil {
			for _, result := range decoded.Results {
				data.Results = append(data.Results, WebSearchResult{Title: result.Title, URL: result.URL, Snippet: result.Snippet, PublishedAt: result.PublishedDate})
			}
		}
	}
	summary := webSearchSummary(data)
	return ResponseData{Text: summary, WebSearch: data, InputTokens: max64(1, inputTokens), OutputTokens: max64(1, int64(len(summary)/4)), StopReason: "end_turn"}, nil
}

func webSearchSummary(data *WebSearchData) string {
	if data == nil {
		return "No web search results were returned."
	}
	if len(data.Results) == 0 {
		return "No web search results were returned for: " + data.Query
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web search results for %q:\n", data.Query)
	for i, result := range data.Results {
		fmt.Fprintf(&b, "\n%d. %s\n%s", i+1, result.Title, result.URL)
		if result.Snippet != "" {
			fmt.Fprintf(&b, "\n%s", result.Snippet)
		}
	}
	return b.String()
}

func DecodeResponse(r io.Reader, names map[string]string) (ResponseData, error) {
	d := NewDecoder()
	var out ResponseData
	tools := map[string]int{}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			frames, e := d.Feed(buf[:n])
			if e != nil {
				return out, e
			}
			for _, f := range frames {
				if e := applyFrame(&out, f, names, tools); e != nil {
					return out, e
				}
			}
		}
		if err == io.EOF {
			if e := d.Finish(); e != nil {
				return out, e
			}
			break
		}
		if err != nil {
			return out, err
		}
	}
	out.Thinking, out.Text = extractThinking(out.Text)
	if out.InputTokens < 1 {
		out.InputTokens = 1
	}
	if out.OutputTokens < 1 {
		out.OutputTokens = int64(len(out.Text) / 4)
		if out.OutputTokens < 1 {
			out.OutputTokens = 1
		}
	}
	if len(out.Tools) > 0 {
		out.StopReason = "tool_use"
	} else if out.StopReason == "" {
		out.StopReason = "end_turn"
	}
	return out, nil
}
func applyFrame(out *ResponseData, f Frame, names map[string]string, tools map[string]int) error {
	mt := f.HeaderString(":message-type")
	if mt == "error" || mt == "exception" {
		if mt == "exception" && f.HeaderString(":exception-type") == "ContentLengthExceededException" {
			out.StopReason = "max_tokens"
			return nil
		}
		return fmt.Errorf("kiro %s %s: %s", mt, first(f.HeaderString(":error-code"), f.HeaderString(":exception-type")), strings.TrimSpace(string(f.Payload)))
	}
	et := f.HeaderString(":event-type")
	var p map[string]interface{}
	if len(f.Payload) > 0 && json.Unmarshal(f.Payload, &p) != nil {
		return errors.New("kiro event payload is not JSON")
	}
	switch et {
	case "assistantResponseEvent":
		out.Text += stringValue(p["content"])
		if t := stringValue(p["thinking"]); t != "" {
			out.Thinking += t
		}
	case "toolUseEvent":
		id := stringValue(p["toolUseId"])
		idx, ok := tools[id]
		if !ok {
			name := stringValue(p["name"])
			if original := names[name]; original != "" {
				name = original
			}
			idx = len(out.Tools)
			tools[id] = idx
			out.Tools = append(out.Tools, ToolCall{ID: id, Name: name})
		}
		out.Tools[idx].Input += stringValue(p["input"])
	case "meteringEvent":
		out.InputTokens = max64(out.InputTokens, numberAny(p, "inputTokens", "inputTokenCount"))
		out.OutputTokens = max64(out.OutputTokens, numberAny(p, "outputTokens", "outputTokenCount"))
		out.CacheReadTokens += numberAny(p, "cacheReadInputTokens", "cacheReadTokens")
		out.CacheCreationTokens += numberAny(p, "cacheCreationInputTokens", "cacheWriteTokens")
	case "contextUsageEvent":
		if percentageAny(p, "contextUsagePercentage", "contextUsage") >= 100 {
			out.StopReason = "model_context_window_exceeded"
		}
	}
	return nil
}
func percentageAny(m map[string]interface{}, keys ...string) float64 {
	for _, key := range keys {
		switch value := m[key].(type) {
		case float64:
			return value
		case json.Number:
			f, _ := value.Float64()
			return f
		}
	}
	return 0
}
func numberAny(m map[string]interface{}, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		}
	}
	return 0
}
func extractThinking(text string) (string, string) {
	start := strings.Index(text, "<thinking>")
	if start < 0 {
		return "", text
	}
	end := strings.Index(text[start+10:], "</thinking>")
	if end < 0 {
		return "", text
	}
	end += start + 10
	thinking := strings.TrimPrefix(text[start+10:end], "\n")
	remaining := text[:start] + strings.TrimLeft(text[end+11:], "\n")
	return thinking, remaining
}

func AnthropicJSON(data ResponseData, model, id string) []byte {
	blocks := contentBlocks(data)
	body := map[string]interface{}{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": data.StopReason, "stop_sequence": nil, "usage": usageMap(data)}
	b, _ := json.Marshal(body)
	return b
}
func AnthropicSSE(data ResponseData, model, id string) []byte {
	var b strings.Builder
	emit := func(event string, v interface{}) {
		raw, _ := json.Marshal(v)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, raw)
	}
	emit("message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": id, "type": "message", "role": "assistant", "model": model, "content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]interface{}{"input_tokens": data.InputTokens, "output_tokens": 0}}})
	idx := 0
	if data.Thinking != "" {
		emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "thinking", "thinking": ""}})
		emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": idx, "delta": map[string]interface{}{"type": "thinking_delta", "thinking": data.Thinking}})
		emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	if data.WebSearch != nil {
		emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "server_tool_use", "id": data.WebSearch.ToolUseID, "name": "web_search", "input": map[string]interface{}{"query": data.WebSearch.Query}}})
		emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
		emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "web_search_tool_result", "content": webSearchResultBlocks(data.WebSearch)}})
		emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	if data.Text != "" {
		emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "text", "text": ""}})
		emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": idx, "delta": map[string]interface{}{"type": "text_delta", "text": data.Text}})
		emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	for _, tool := range data.Tools {
		emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "tool_use", "id": tool.ID, "name": tool.Name, "input": map[string]interface{}{}}})
		emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": idx, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": tool.Input}})
		emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": idx})
		idx++
	}
	deltaUsage := map[string]interface{}{"output_tokens": data.OutputTokens}
	if data.WebSearch != nil {
		deltaUsage["server_tool_use"] = map[string]interface{}{"web_search_requests": 1}
	}
	emit("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]interface{}{"stop_reason": data.StopReason, "stop_sequence": nil}, "usage": deltaUsage})
	emit("message_stop", map[string]interface{}{"type": "message_stop"})
	return []byte(b.String())
}
func contentBlocks(d ResponseData) []interface{} {
	var out []interface{}
	if d.Thinking != "" {
		out = append(out, map[string]interface{}{"type": "thinking", "thinking": d.Thinking, "signature": ""})
	}
	if d.WebSearch != nil {
		out = append(out, map[string]interface{}{"type": "server_tool_use", "id": d.WebSearch.ToolUseID, "name": "web_search", "input": map[string]interface{}{"query": d.WebSearch.Query}})
		out = append(out, map[string]interface{}{"type": "web_search_tool_result", "content": webSearchResultBlocks(d.WebSearch)})
	}
	if d.Text != "" {
		out = append(out, map[string]interface{}{"type": "text", "text": d.Text})
	}
	for _, t := range d.Tools {
		var input interface{} = map[string]interface{}{}
		_ = json.Unmarshal([]byte(t.Input), &input)
		out = append(out, map[string]interface{}{"type": "tool_use", "id": t.ID, "name": t.Name, "input": input})
	}
	return out
}
func webSearchResultBlocks(data *WebSearchData) []interface{} {
	out := make([]interface{}, 0, len(data.Results))
	for _, result := range data.Results {
		var pageAge interface{}
		if result.PublishedAt > 0 {
			pageAge = time.UnixMilli(result.PublishedAt).UTC().Format("January 2, 2006")
		}
		out = append(out, map[string]interface{}{"type": "web_search_result", "title": result.Title, "url": result.URL, "encrypted_content": result.Snippet, "page_age": pageAge})
	}
	return out
}
func usageMap(d ResponseData) map[string]interface{} {
	usage := map[string]interface{}{"input_tokens": d.InputTokens, "output_tokens": d.OutputTokens, "cache_read_input_tokens": d.CacheReadTokens, "cache_creation_input_tokens": d.CacheCreationTokens}
	if d.WebSearch != nil {
		usage["server_tool_use"] = map[string]interface{}{"web_search_requests": 1}
	}
	return usage
}
