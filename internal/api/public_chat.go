package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/storage"
)

const (
	publicChatJSONLimit       = 1 << 20
	publicChatLimiterCapacity = 8192
)

type publicChatRateWindow struct {
	Minute   int64
	Count    int
	Sequence uint64
	Blocked  bool
}

var publicChatLimiterSequence atomic.Uint64

type publicChatAdminView struct {
	storage.PublicChatLink
	PublicURL  string `json:"public_url"`
	RouteLabel string `json:"route_label,omitempty"`
}

type publicChatConfigView struct {
	Slug               string `json:"slug"`
	Title              string `json:"title"`
	WelcomeMessage     string `json:"welcome_message"`
	Model              string `json:"model"`
	MaxHistoryMessages int    `json:"max_history_messages"`
}

type publicChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type publicChatMessageRequest struct {
	ConversationID string              `json:"conversation_id"`
	Messages       []publicChatMessage `json:"messages"`
}

func (s *Server) adminPublicChatLinks(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		links, err := s.store.ListPublicChatLinks(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s.publicChatAdminViews(r.Context(), r, links))
	case http.MethodPost:
		var req storage.PublicChatLink
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		link, err := s.store.UpsertPublicChatLink(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.publicChatAdminView(r.Context(), r, link))
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) adminPublicChatLinkAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/public-chat/links/"), "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("public chat link id required"))
		return
	}
	switch r.Method {
	case http.MethodPut, http.MethodPatch:
		var req storage.PublicChatLink
		if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		existing, found, err := s.store.GetPublicChatLink(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, errors.New("public chat link not found"))
			return
		}
		req.ID = existing.ID
		req.CreatedAt = existing.CreatedAt
		link, err := s.store.UpsertPublicChatLink(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeJSON(w, http.StatusOK, s.publicChatAdminView(r.Context(), r, link))
	case http.MethodDelete:
		if err := s.store.DeletePublicChatLink(r.Context(), id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, errors.New("public chat link not found"))
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) publicChatAdminViews(ctx context.Context, r *http.Request, links []storage.PublicChatLink) []publicChatAdminView {
	out := make([]publicChatAdminView, 0, len(links))
	for _, link := range links {
		out = append(out, s.publicChatAdminView(ctx, r, link))
	}
	return out
}

func (s *Server) publicChatAdminView(ctx context.Context, r *http.Request, link storage.PublicChatLink) publicChatAdminView {
	view := publicChatAdminView{
		PublicChatLink: link,
		PublicURL:      publicChatURL(r, link.Slug),
	}
	switch link.RouteType {
	case storage.PublicChatRouteUserGroup:
		if group, found, err := s.store.GetUserGroup(ctx, link.UserGroupID); err == nil && found {
			view.RouteLabel = group.Name
		}
	case storage.PublicChatRouteAccountPoolGroup:
		view.RouteLabel = link.GroupName
	}
	return view
}

func publicChatURL(r *http.Request, slug string) string {
	if r == nil {
		return "/chat/" + url.PathEscape(slug)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.ToLower(strings.Split(forwarded, ",")[0])
	}
	host := strings.TrimSpace(r.Host)
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	if host == "" {
		return "/chat/" + url.PathEscape(slug)
	}
	return scheme + "://" + host + "/chat/" + url.PathEscape(slug)
}

func (s *Server) publicChatPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	slug := strings.Trim(strings.TrimPrefix(r.URL.Path, "/chat/"), "/")
	if slug == "" || strings.Contains(slug, "/") {
		writeError(w, http.StatusNotFound, errors.New("public chat link not found"))
		return
	}
	link, found, err := s.store.GetPublicChatLinkBySlug(r.Context(), slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found || !link.Enabled {
		writeError(w, http.StatusNotFound, errors.New("public chat link not found"))
		return
	}
	page := publicChatHTML(link)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(page))
	}
}

func (s *Server) publicChatAPI(w http.ResponseWriter, r *http.Request) {
	slug, action, ok := parsePublicChatAPIPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("public chat endpoint not found"))
		return
	}
	link, found, err := s.store.GetPublicChatLinkBySlug(r.Context(), slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !found || !link.Enabled {
		writeError(w, http.StatusNotFound, errors.New("public chat link not found"))
		return
	}
	switch action {
	case "config":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, publicChatConfigView{
			Slug:               link.Slug,
			Title:              firstNonEmpty(link.Title, link.Name),
			WelcomeMessage:     link.WelcomeMessage,
			Model:              link.Model,
			MaxHistoryMessages: link.MaxHistoryMessages,
		})
	case "messages":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.publicChatMessages(w, r, link)
	default:
		writeError(w, http.StatusNotFound, errors.New("public chat endpoint not found"))
	}
}

func parsePublicChatAPIPath(path string) (slug, action string, ok bool) {
	rest := strings.Trim(strings.TrimPrefix(path, "/public-chat/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if _, err := storage.NormalizePublicChatSlug(parts[0]); err != nil {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) publicChatMessages(w http.ResponseWriter, r *http.Request, link storage.PublicChatLink) {
	if !s.publicChatRateAllowed(link, s.clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, errors.New("public chat rate limit exceeded"))
		return
	}
	var req publicChatMessageRequest
	if err := decodeJSONRequestBody(r.Body, &req, publicChatJSONLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	messages, err := sanitizePublicChatMessages(req.Messages, link.MaxHistoryMessages)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	body := map[string]interface{}{
		"model":    link.Model,
		"messages": messages,
		"stream":   false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	pol := downstreamPolicy{
		Group:        firstNonEmpty(link.GroupName, s.cfg.DefaultGroup),
		UserGroupID:  strings.TrimSpace(link.UserGroupID),
		ProviderHint: "auto",
		KeyHash:      publicChatKeyHash(link.ID),
		KeyLabel:     "public-chat:" + link.Slug,
		Authed:       true,
	}
	ctx := withPublicChatPolicy(r.Context(), pol)
	ctx = contextWithBodySource(ctx, bodysource.Bytes(raw))
	ctx = contextWithBodyMeta(ctx, bodysource.BodyMeta{})
	gatewayReq := r.Clone(ctx)
	gatewayReq.Method = http.MethodPost
	gatewayReq.URL = &url.URL{Path: "/v1/chat/completions"}
	gatewayReq.RequestURI = ""
	gatewayReq.Body = io.NopCloser(bytes.NewReader(raw))
	gatewayReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	gatewayReq.ContentLength = int64(len(raw))
	gatewayReq.Header = r.Header.Clone()
	gatewayReq.Header.Set("Content-Type", "application/json")
	gatewayReq.Header.Del("Authorization")
	gatewayReq.Header.Del("x-api-key")
	if conversationID := normalizePublicChatConversationID(req.ConversationID); conversationID != "" {
		threadID := "public-chat:" + link.ID + ":" + conversationID
		gatewayReq.Header.Set("Thread-Id", threadID)
		gatewayReq.Header.Set("Session-Id", threadID)
	}
	w.Header().Set("X-Pool-Public-Chat", link.Slug)
	s.handleGatewayPost(w, gatewayReq)
}

func sanitizePublicChatMessages(in []publicChatMessage, maxHistory int) ([]publicChatMessage, error) {
	if maxHistory <= 0 || maxHistory > storage.MaxPublicChatHistory {
		maxHistory = storage.DefaultPublicChatHistory
	}
	if len(in) == 0 {
		return nil, errors.New("messages required")
	}
	if len(in) > maxHistory {
		in = in[len(in)-maxHistory:]
	}
	out := make([]publicChatMessage, 0, len(in))
	var total int
	for _, msg := range in {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		if len(content) > 32*1024 {
			return nil, errors.New("single message is too large")
		}
		total += len(content)
		if total > 256*1024 {
			return nil, errors.New("message history is too large")
		}
		out = append(out, publicChatMessage{Role: role, Content: content})
	}
	if len(out) == 0 {
		return nil, errors.New("messages required")
	}
	if out[len(out)-1].Role != "user" {
		return nil, errors.New("last message must be from user")
	}
	return out, nil
}

func normalizePublicChatConversationID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '_', ch == '-':
			b.WriteRune(ch)
		}
		if b.Len() >= 96 {
			break
		}
	}
	return b.String()
}

func publicChatKeyHash(linkID string) string {
	sum := sha256.Sum256([]byte("public-chat\x00" + strings.TrimSpace(linkID)))
	return hex.EncodeToString(sum[:])
}

func (s *Server) publicChatRateAllowed(link storage.PublicChatLink, ip string) bool {
	return s.publicChatRateAllowedAt(link, ip, storage.Now()/60)
}

func (s *Server) publicChatRateAllowedAt(link storage.PublicChatLink, ip string, nowMinute int64) bool {
	limit := link.RateLimitPerMinute
	if limit <= 0 {
		limit = storage.DefaultPublicChatRateLimit
	}
	if limit > storage.MaxPublicChatRateLimit {
		limit = storage.MaxPublicChatRateLimit
	}
	key := link.ID + "\x00" + ip
	s.publicChatLimiterMu.Lock()
	defer s.publicChatLimiterMu.Unlock()
	if s.publicChatLimiter == nil {
		s.publicChatLimiter = map[string]publicChatRateWindow{}
	}
	// Sweep at most once per minute. If the current minute still fills the bounded
	// table, the oldest surviving window is reclaimed below for a new identity.
	if s.publicChatLastMinute != nowMinute {
		for k, window := range s.publicChatLimiter {
			if window.Minute < nowMinute {
				delete(s.publicChatLimiter, k)
			}
		}
		s.publicChatLastMinute = nowMinute
	}
	window, exists := s.publicChatLimiter[key]
	if !exists {
		if len(s.publicChatLimiter) >= publicChatLimiterCapacity {
			if !s.evictOldestPublicChatRateWindow() {
				return false
			}
		}
		window = publicChatRateWindow{Minute: nowMinute, Sequence: publicChatLimiterSequence.Add(1)}
	} else if window.Minute != nowMinute {
		window = publicChatRateWindow{Minute: nowMinute, Sequence: publicChatLimiterSequence.Add(1)}
	}
	if window.Count >= limit {
		window.Blocked = true
		s.publicChatLimiter[key] = window
		return false
	}
	window.Count++
	window.Blocked = window.Count >= limit
	s.publicChatLimiter[key] = window
	return true
}

// evictOldestPublicChatRateWindow is called with publicChatLimiterMu held. Sequence
// records admission order within a minute; exhausted windows are active rate-limit
// decisions and cannot be evicted to let a rotating identity reset its allowance.
// Minute and key provide deterministic ordering for legacy zero-sequence entries.
func (s *Server) evictOldestPublicChatRateWindow() bool {
	oldestKey := ""
	oldest := publicChatRateWindow{}
	found := false
	for key, window := range s.publicChatLimiter {
		if window.Blocked {
			continue
		}
		if !found || window.Sequence < oldest.Sequence ||
			(window.Sequence == oldest.Sequence && (window.Minute < oldest.Minute ||
				(window.Minute == oldest.Minute && key < oldestKey))) {
			oldestKey = key
			oldest = window
			found = true
		}
	}
	if found {
		delete(s.publicChatLimiter, oldestKey)
	}
	return found
}

func publicChatHTML(link storage.PublicChatLink) string {
	title := html.EscapeString(firstNonEmpty(link.Title, link.Name, "在线聊天"))
	slug := html.EscapeString(link.Slug)
	return `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>` + title + `</title>
  <style>
    :root { color-scheme: light dark; --bg:#0f172a; --panel:#111827; --line:#273449; --text:#e5e7eb; --muted:#94a3b8; --brand:#60a5fa; --user:#2563eb; --assistant:#1f2937; --danger:#ef4444; }
    @media (prefers-color-scheme: light) { :root { --bg:#f7f8fb; --panel:#ffffff; --line:#e5e7eb; --text:#0f172a; --muted:#64748b; --assistant:#f1f5f9; } }
    * { box-sizing: border-box; }
    body { margin:0; min-height:100vh; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: radial-gradient(circle at top, rgba(96,165,250,.18), transparent 32rem), var(--bg); color:var(--text); }
    .wrap { min-height:100vh; display:flex; justify-content:center; padding:24px; }
    .chat { width:min(920px, 100%); min-height:calc(100vh - 48px); display:grid; grid-template-rows:auto 1fr auto; background:rgba(17,24,39,.72); border:1px solid var(--line); border-radius:24px; overflow:hidden; box-shadow:0 24px 70px rgba(0,0,0,.24); backdrop-filter: blur(14px); }
    @media (prefers-color-scheme: light) { .chat { background:rgba(255,255,255,.82); } }
    header { padding:18px 22px; border-bottom:1px solid var(--line); display:flex; justify-content:space-between; gap:12px; align-items:center; }
    h1 { font-size:18px; margin:0; letter-spacing:.01em; }
    .model { font-size:12px; color:var(--muted); padding:5px 9px; border:1px solid var(--line); border-radius:999px; }
    .log { padding:22px; overflow:auto; display:flex; flex-direction:column; gap:14px; }
    .msg { max-width:82%; padding:12px 14px; border-radius:16px; line-height:1.62; white-space:pre-wrap; word-break:break-word; border:1px solid var(--line); }
    .msg.user { align-self:flex-end; background:var(--user); color:white; border-color:transparent; border-bottom-right-radius:5px; }
    .msg.assistant { align-self:flex-start; background:var(--assistant); border-bottom-left-radius:5px; }
    .msg.system { align-self:center; max-width:92%; color:var(--muted); background:transparent; border-style:dashed; font-size:14px; }
    form { display:grid; grid-template-columns:1fr auto; gap:12px; padding:16px; border-top:1px solid var(--line); background:rgba(15,23,42,.16); }
    textarea { width:100%; min-height:48px; max-height:180px; resize:vertical; border-radius:14px; border:1px solid var(--line); padding:13px 14px; background:var(--panel); color:var(--text); outline:none; font:inherit; line-height:1.45; }
    textarea:focus { border-color:var(--brand); box-shadow:0 0 0 3px rgba(96,165,250,.22); }
    button { border:0; border-radius:14px; padding:0 20px; font-weight:700; color:white; background:linear-gradient(135deg, #2563eb, #7c3aed); cursor:pointer; min-width:92px; }
    button:disabled { opacity:.56; cursor:not-allowed; }
    .error { color:var(--danger); font-size:13px; padding:0 18px 12px; }
    @media (max-width: 640px) { .wrap { padding:0; } .chat { min-height:100vh; border-radius:0; border:0; } .msg { max-width:92%; } form { grid-template-columns:1fr; } button { height:46px; } }
  </style>
</head>
<body>
  <main class="wrap">
    <section class="chat" data-slug="` + slug + `">
      <header><h1 id="title">` + title + `</h1><span id="model" class="model"></span></header>
      <div id="log" class="log"></div>
      <div id="error" class="error" hidden></div>
      <form id="form"><textarea id="input" placeholder="输入消息，Enter 发送，Shift+Enter 换行" autocomplete="off"></textarea><button id="send" type="submit">发送</button></form>
    </section>
  </main>
  <script>
  (() => {
    const slug = document.querySelector('.chat').dataset.slug;
    const log = document.getElementById('log');
    const input = document.getElementById('input');
    const send = document.getElementById('send');
    const form = document.getElementById('form');
    const error = document.getElementById('error');
    const storageKey = 'public-chat:' + slug;
    let state = loadState();
    let config = { title: document.title, welcome_message: '', model: '', max_history_messages: 24 };
    function uuid() {
      if (crypto.randomUUID) return crypto.randomUUID();
      return Math.random().toString(36).slice(2) + Date.now().toString(36);
    }
    function loadState() {
      try {
        const raw = JSON.parse(localStorage.getItem(storageKey) || '{}');
        return { conversation_id: raw.conversation_id || uuid(), messages: Array.isArray(raw.messages) ? raw.messages : [] };
      } catch { return { conversation_id: uuid(), messages: [] }; }
    }
    function saveState() {
      localStorage.setItem(storageKey, JSON.stringify(state));
    }
    function setError(message) {
      error.textContent = message || '';
      error.hidden = !message;
    }
    function addMessage(role, content, persist = true) {
      const node = document.createElement('div');
      node.className = 'msg ' + role;
      node.textContent = content;
      log.appendChild(node);
      log.scrollTop = log.scrollHeight;
      if (persist && (role === 'user' || role === 'assistant')) {
        state.messages.push({ role, content });
        const max = Math.max(2, Number(config.max_history_messages || 24));
        if (state.messages.length > max) state.messages = state.messages.slice(-max);
        saveState();
      }
      return node;
    }
    function render() {
      log.innerHTML = '';
      if (config.welcome_message) addMessage('system', config.welcome_message, false);
      for (const msg of state.messages) addMessage(msg.role, msg.content, false);
      log.scrollTop = log.scrollHeight;
    }
    function extractText(data) {
      return data?.choices?.[0]?.message?.content || data?.choices?.[0]?.delta?.content || data?.output_text || data?.message || '';
    }
    async function loadConfig() {
      const res = await fetch('/public-chat/' + encodeURIComponent(slug) + '/config');
      if (!res.ok) throw new Error('聊天链接不可用');
      config = await res.json();
      document.title = config.title || document.title;
      document.getElementById('title').textContent = config.title || document.title;
      document.getElementById('model').textContent = config.model || '';
      render();
    }
    async function sendMessage(text) {
      addMessage('user', text);
      const pending = addMessage('assistant', '正在生成…', false);
      send.disabled = true;
      input.disabled = true;
      setError('');
      try {
        const res = await fetch('/public-chat/' + encodeURIComponent(slug) + '/messages', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ conversation_id: state.conversation_id, messages: state.messages })
        });
        const raw = await res.text();
        let data = null;
        try { data = JSON.parse(raw); } catch {}
        if (!res.ok) throw new Error(data?.error?.message || data?.error || raw || '请求失败');
        const text = extractText(data).trim() || raw;
        pending.textContent = text;
        state.messages.push({ role: 'assistant', content: text });
        saveState();
      } catch (e) {
        pending.remove();
        state.messages = state.messages.filter((msg, idx) => idx !== state.messages.length - 1 || msg.role !== 'user' || msg.content !== text);
        saveState();
        setError(e.message || String(e));
      } finally {
        send.disabled = false;
        input.disabled = false;
        input.focus();
      }
    }
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const text = input.value.trim();
      if (!text) return;
      input.value = '';
      sendMessage(text);
    });
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); form.requestSubmit(); }
    });
    loadConfig().catch((e) => { setError(e.message || String(e)); render(); });
  })();
  </script>
</body>
</html>`
}
