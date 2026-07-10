package upstream

import (
	"net/url"
	"strings"
)

func NormalizeBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "https://chatgpt.com/backend-api/codex"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if (u.Host == "chatgpt.com" || u.Host == "chat.openai.com") && (u.Path == "" || u.Path == "/") {
		u.Path = "/backend-api/codex"
	}
	return strings.TrimRight(u.String(), "/")
}

func ComputeURL(base, downstreamPath string) string {
	base = NormalizeBaseURL(base)
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimRight(base, "/") + downstreamPath
	}
	path := downstreamPath
	query := ""
	if idx := strings.Index(path, "?"); idx >= 0 {
		query = path[idx+1:]
		path = path[:idx]
	}
	if strings.Contains(u.Path, "/backend-api/codex") || strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	u.RawQuery = query
	return u.String()
}

func ComputeCodexResponsesWebSocketURL(base, downstreamPath string) string {
	target := ComputeURL(base, downstreamPath)
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return target
	}
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else {
		u.Scheme = "wss"
	}
	return u.String()
}
