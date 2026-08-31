package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// CodexThreadRuntime is the narrow v2 app-server contract used by the thread
// control plane. It is deliberately separate from login sessions, account
// affinity sessions, and CPA mappings.
type CodexThreadRuntime interface {
	ListThreads(ctx context.Context, runtimeID string, p ThreadListParams) (ThreadListResponse, error)
	ReadThread(ctx context.Context, runtimeID, threadID string, includeTurns bool) (Thread, error)
	ResumeThread(ctx context.Context, runtimeID string, p ThreadResumeParams) (Thread, error)
	InterruptTurn(ctx context.Context, runtimeID, threadID, turnID string) error
	SubscribeThreadStatus(ctx context.Context, runtimeID string) (<-chan ThreadStatusChanged, error)
}

// ThreadListParams contains only the allow-listed thread/list filters. Cursor
// is an internal app-server cursor; callers of the admin API only see a sealed
// cursor bound to their principal, runtime, generation, and filters.
type ThreadListParams struct {
	Cursor           string   `json:"cursor,omitempty"`
	SortKey          string   `json:"sort_key,omitempty"`
	SortDirection    string   `json:"sort_direction,omitempty"`
	ModelProviders   []string `json:"model_providers,omitempty"`
	SourceKinds      []string `json:"source_kinds,omitempty"`
	Archived         *bool    `json:"archived,omitempty"`
	IsPinned         *bool    `json:"is_pinned,omitempty"`
	CWD              string   `json:"cwd,omitempty"`
	SearchTerm       string   `json:"search_term,omitempty"`
	ParentThreadID   string   `json:"parent_thread_id,omitempty"`
	AncestorThreadID string   `json:"ancestor_thread_id,omitempty"`
	Limit            int      `json:"limit,omitempty"`
}

type ThreadListResponse struct {
	Data            []Thread `json:"data"`
	NextCursor      string   `json:"next_cursor,omitempty"`
	BackwardsCursor string   `json:"backwards_cursor,omitempty"`
}

type ThreadResumeParams struct {
	ThreadID string `json:"thread_id"`
}

// Thread is an adapter-only DTO. Raw IDs and cwd are converted to encrypted
// handles/safe labels before an HTTP response is written.
type Thread struct {
	ID            string    `json:"id"`
	Model         string    `json:"model,omitempty"`
	ModelProvider string    `json:"model_provider,omitempty"`
	Source        string    `json:"source,omitempty"`
	CWD           string    `json:"cwd,omitempty"`
	Status        string    `json:"status,omitempty"`
	WaitingReason string    `json:"waiting_reason,omitempty"`
	ActiveTurnID  string    `json:"active_turn_id,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
	DirectInput   bool      `json:"direct_input,omitempty"`
}

// ThreadStatusChanged is a normalized app-server status notification. Revision
// must be monotonic per thread so stale events can be discarded safely.
type ThreadStatusChanged struct {
	ThreadID      string `json:"-"`
	TurnID        string `json:"-"`
	Status        string `json:"status"`
	WaitingReason string `json:"waiting_reason,omitempty"`
	Revision      uint64 `json:"revision"`
}

// JSONRPCCodexRuntime adapts the request/response portion of a v2 JSON-RPC
// app-server. Deployments provide StatusSubscribe for their WebSocket/stdio
// notification transport. Without it, interruption is intentionally unavailable
// because a polling result is not proof of TurnAborted.
type JSONRPCCodexRuntime struct {
	URL             string
	Client          *http.Client
	Headers         http.Header
	StatusSubscribe func(context.Context, string) (<-chan ThreadStatusChanged, error)
	sequence        atomic.Uint64
}

func (c *JSONRPCCodexRuntime) call(ctx context.Context, method string, params any, out any) error {
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("codex app-server URL is empty")
	}
	payload, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{JSONRPC: "2.0", ID: c.sequence.Add(1), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, values := range c.Headers {
		req.Header[key] = append([]string(nil), values...)
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("app-server HTTP status %d", resp.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("app-server RPC %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

func (c *JSONRPCCodexRuntime) ListThreads(ctx context.Context, _ string, p ThreadListParams) (ThreadListResponse, error) {
	var result ThreadListResponse
	err := c.call(ctx, "thread/list", p, &result)
	return result, err
}

func (c *JSONRPCCodexRuntime) ReadThread(ctx context.Context, _ string, threadID string, includeTurns bool) (Thread, error) {
	var result Thread
	err := c.call(ctx, "thread/read", map[string]any{
		"thread_id": threadID, "include_turns": includeTurns,
	}, &result)
	return result, err
}

func (c *JSONRPCCodexRuntime) ResumeThread(ctx context.Context, _ string, p ThreadResumeParams) (Thread, error) {
	var result Thread
	err := c.call(ctx, "thread/resume", map[string]any{"thread_id": p.ThreadID}, &result)
	return result, err
}

func (c *JSONRPCCodexRuntime) InterruptTurn(ctx context.Context, _ string, threadID, turnID string) error {
	return c.call(ctx, "turn/interrupt", map[string]any{"thread_id": threadID, "turn_id": turnID}, nil)
}

func (c *JSONRPCCodexRuntime) SubscribeThreadStatus(ctx context.Context, runtimeID string) (<-chan ThreadStatusChanged, error) {
	if c.StatusSubscribe == nil {
		return nil, errors.New("codex app-server status subscription is unavailable")
	}
	return c.StatusSubscribe(ctx, runtimeID)
}
