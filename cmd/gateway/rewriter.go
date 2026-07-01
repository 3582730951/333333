package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// rewriteBody 改写请求体中的本地指纹
func rewriteBody(body []byte, identity *CachedIdentity) ([]byte, error) {
	// P0: 精确 JSON 路径替换 (metadata.user_id)
	body, err := rewriteMetadataUserID(body, identity)
	if err != nil {
		// 如果不是 JSON 或没有 metadata，继续（可能是其他格式）
	}

	// P1: 文本流式替换（环境变量、主机名、用户名）
	body = rewriteTextPatterns(body, identity)

	// P2: <env> 块替换
	body = rewriteEnvBlock(body, identity)

	return body, nil
}

// rewriteMetadataUserID 改写 metadata.user_id
func rewriteMetadataUserID(body []byte, identity *CachedIdentity) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return body, err
	}

	// 替换 metadata.user_id
	if md, ok := root["metadata"].(map[string]interface{}); ok {
		if _, hasUserID := md["user_id"]; hasUserID {
			// Claude: user_id 是 JSON 字符串
			md["user_id"] = fmt.Sprintf(`{"device_id":"%s","account_uuid":"","session_id":"%s"}`,
				identity.Virtual.UserID, identity.Virtual.SessionID)
		}
	}

	// 替换 session_id (Codex)
	if _, hasSession := root["session_id"]; hasSession {
		root["session_id"] = identity.Virtual.SessionID
	}
	if _, hasThread := root["parent_thread_id"]; hasThread {
		root["parent_thread_id"] = identity.Virtual.SessionID
	}

	out, err := json.Marshal(root)
	if err != nil {
		return body, err
	}
	return out, nil
}

// rewriteTextPatterns 使用 Aho-Corasick 风格的多模式匹配替换文本
func rewriteTextPatterns(body []byte, identity *CachedIdentity) []byte {
	replacements := []struct {
		pattern string
		repl    string
	}{
		{identity.Local.Username, identity.Virtual.Username},
		{identity.Local.Hostname, identity.Virtual.Hostname},
		{identity.Local.HomeDir, identity.Virtual.HomeDir},
	}

	result := body
	for _, r := range replacements {
		if r.pattern == "" {
			continue
		}
		result = bytes.ReplaceAll(result, []byte(r.pattern), []byte(r.repl))
	}

	return result
}

// rewriteEnvBlock 改写 <env> 块
func rewriteEnvBlock(body []byte, identity *CachedIdentity) []byte {
	// 匹配 <env>...</env>
	envRe := regexp.MustCompile(`(?s)<env>(.*?)</env>`)

	return envRe.ReplaceAllFunc(body, func(match []byte) []byte {
		content := string(match)

		// 替换关键行
		content = replaceMarkerValue(content, "Platform:", identity.Virtual.OSName)
		content = replaceMarkerValue(content, "OS Version:", identity.Virtual.OSRelease)
		content = replaceMarkerValue(content, "Terminal:", identity.Virtual.Terminal)
		content = replaceMarkerValue(content, "Hostname:", identity.Virtual.Hostname)
		content = replaceMarkerValue(content, "Architecture:", identity.Virtual.Arch)

		// 环境变量引用
		content = strings.ReplaceAll(content, identity.Local.HomeDir, identity.Virtual.HomeDir)
		content = strings.ReplaceAll(content, identity.Local.Username, identity.Virtual.Username)

		return []byte(content)
	})
}

// replaceMarkerValue 替换标记后的值（如 "Platform: darwin" → "Platform: Mac OS"）
func replaceMarkerValue(s, marker, value string) string {
	if value == "" {
		return s
	}

	idx := strings.Index(s, marker)
	if idx < 0 {
		return s
	}

	// 找到标记后的值起始位置
	valStart := idx + len(marker)
	for valStart < len(s) && (s[valStart] == ' ' || s[valStart] == '\t') {
		valStart++
	}

	// 找到行尾
	end := valStart
	for end < len(s) && s[end] != '\n' && s[end] != '\r' {
		end++
	}

	// 提取行尾（\n 或 \r\n）
	rest := ""
	if end < len(s) {
		rest = s[end:]
	}

	return s[:valStart] + value + rest
}
