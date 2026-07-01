package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// handleCodexConfigScript serves GET /file/{apikey} (the key may also arrive as
// ?key=… or in the Authorization header): a one-shot bash script that points the
// official `codex` CLI and Claude Code at THIS pool, then best-effort installs rtk
// hooks. Running it once writes ~/.codex/config.toml and merges
// ~/.claude/settings.json env so both CLIs are ready without manual gateway setup.
//
// Config shape is verified against codex-rs (codex-model-provider-info):
//   - wire_api MUST be "responses" ("chat" was removed upstream).
//   - experimental_bearer_token carries the key as `Authorization: Bearer <token>`;
//     it is the field codex documents for programmatic/self-hosted setups, so no
//     env var, auth.json, or shell-rc edit is needed.
//   - requires_openai_auth defaults false for a custom provider, so codex skips the
//     login screen entirely.
func (s *Server) handleCodexConfigScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	// Key precedence: path (/file/<key>) → ?key= → Authorization header.
	key := strings.Trim(strings.TrimPrefix(r.URL.Path, "/file"), "/")
	if key == "" {
		key = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	if key == "" {
		key = extractAPIKey(r)
	}
	key = strings.TrimSpace(key)

	// Resolve the routing group + a sensible default model from the key when it is a
	// known downstream key. With RequireDownstreamKey on, an unknown/missing key is
	// refused up front so the user gets a clear signal instead of a script that 401s
	// on first use.
	group := s.cfg.DefaultGroup
	model := ""
	known := false
	if key != "" {
		if k, found, _ := s.store.LookupAPIKey(r.Context(), hashAPIKey(key)); found {
			known = true
			if g := strings.TrimSpace(k.GroupName); g != "" {
				group = g
			}
			model = strings.TrimSpace(k.ForceModel)
		}
	}
	if !known && s.cfg.RequireDownstreamKey {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("#!/usr/bin/env bash\necho '❌ 未知或缺失的 API Key。请在控制台创建一个 Key，然后重试：'\necho '   curl -fsSL <pool>/file/<your_api_key> | bash'\nexit 1\n"))
		return
	}
	if model == "" {
		// Operator-configured install default (gpt-5.5). Falls back to the best probed
		// codex model only if the configured default is blanked out.
		model = strings.TrimSpace(s.settingString(r.Context(), "codex_install_model", s.cfg.CodexInstallModel))
	}
	if model == "" {
		model = s.pickDefaultCodexModel(r.Context(), group)
	}

	origin := externalOrigin(r)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=setup-pool-cli.sh")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	effort := s.settingString(r.Context(), "codex_install_effort", s.cfg.CodexInstallEffort)
	approval := s.settingString(r.Context(), "codex_install_approval_policy", s.cfg.CodexInstallApprovalPolicy)
	sandbox := s.settingString(r.Context(), "codex_install_sandbox_mode", s.cfg.CodexInstallSandboxMode)
	_, _ = w.Write([]byte(buildCodexConfigScript(origin, key, model, effort, approval, sandbox)))
}

// externalOrigin reconstructs the publicly reachable scheme://host for this request,
// honoring the X-Forwarded-Proto/Host set by a fronting reverse proxy (where r.TLS
// is nil and r.Host may be the internal address). Falls back to the direct values.
func externalOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf != "" {
		scheme = strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	host := r.Host
	if xh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xh != "" {
		host = strings.TrimSpace(strings.Split(xh, ",")[0])
	}
	return scheme + "://" + strings.TrimSpace(host)
}

// pickDefaultCodexModel chooses a reasonable `model` for the generated config: the
// codex-provider model with the largest native window advertised for the group
// (pool-wide as a fallback), else a current-generation default. Claude / custom-
// provider models are skipped — the generated config targets the Codex/Responses
// path. The value is non-binding: the user can change it, and a key's force_model
// overrides it on the wire anyway.
func (s *Server) pickDefaultCodexModel(ctx context.Context, group string) string {
	const fallback = "gpt-5.4"
	caps, err := s.store.ListCapabilities(ctx, group)
	if err != nil || len(caps) == 0 {
		caps, err = s.store.ListCapabilities(ctx, "")
		if err != nil || len(caps) == 0 {
			return fallback
		}
	}
	best := ""
	var bestWin int64 = -1
	for _, c := range caps {
		if !isCodexSource(c.Source) {
			continue
		}
		if c.NativeMaxContextWindow > bestWin {
			bestWin = c.NativeMaxContextWindow
			best = c.ModelSlug
		}
	}
	if best == "" {
		return fallback
	}
	return best
}

// isCodexSource reports whether a capability row belongs to a Codex/GPT account
// (anything that is not a Claude or custom-provider capability).
func isCodexSource(source string) bool {
	s := strings.ToLower(strings.TrimSpace(source))
	return !strings.HasPrefix(s, "claude") && !strings.HasPrefix(s, "custom:")
}

// buildCodexConfigScript renders the bash installer. It backs up any existing
// config.toml, honors CODEX_HOME, and writes a config selecting the pool provider plus
// the operator-configured Codex defaults (model, reasoning effort, and the
// approval/sandbox pair that together enable fully-automated "goal mode"). It also
// merges Claude Code env and best-effort installs rtk hooks. The key is generated as
// cap_<hex>, but the renderer still strips quote characters defensively.
func buildCodexConfigScript(origin, apiKey, model, effort, approval, sandbox string) string {
	safeKey := strings.ReplaceAll(apiKey, "'", "")
	keyLine := ""
	if safeKey != "" {
		keyLine = "experimental_bearer_token = \"$API_KEY\""
	} else {
		// No key (open pool): codex still needs a non-empty token shape downstream,
		// but an open pool accepts any. Emit a placeholder the user can edit.
		keyLine = "experimental_bearer_token = \"any-non-empty\""
	}
	// Operator-configured behavior lines, emitted only when set. Values are
	// config-sourced (not request input); still strip quotes/newlines defensively so a
	// stray value can't break out of the TOML string or inject extra keys.
	clean := func(v string) string {
		v = strings.TrimSpace(v)
		v = strings.ReplaceAll(v, "\"", "")
		v = strings.ReplaceAll(v, "'", "")
		v = strings.ReplaceAll(v, "\n", "")
		v = strings.ReplaceAll(v, "\r", "")
		return v
	}
	model = clean(model)
	if model == "" {
		model = "gpt-5.4"
	}
	shellQuote := func(v string) string {
		return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
	}
	var extra strings.Builder
	if e := clean(effort); e != "" {
		fmt.Fprintf(&extra, "model_reasoning_effort = \"%s\"\n", e)
	}
	if a := clean(approval); a != "" {
		fmt.Fprintf(&extra, "approval_policy = \"%s\"\n", a)
	}
	if sb := clean(sandbox); sb != "" {
		fmt.Fprintf(&extra, "sandbox_mode = \"%s\"\n", sb)
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

# ── Pool → Codex 一键配置脚本（执行一次即可）─────────────────────────────
ORIGIN=%s
API_KEY=%s
MODEL=%s
PROVIDER_ID="poolserver"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
CONFIG="$CODEX_HOME/config.toml"

echo "🚀 配置 Codex + Claude Code 接入 Pool: $ORIGIN"
mkdir -p "$CODEX_HOME"

if [ -f "$CONFIG" ]; then
  BAK="$CONFIG.bak.$(date +%%Y%%m%%d%%H%%M%%S)"
  cp "$CONFIG" "$BAK"
  echo "📦 已备份原有配置 → $BAK"
fi

cat > "$CONFIG" <<EOF
# Generated by Pool — points the codex CLI at a self-hosted pool.
model = "$MODEL"
model_provider = "$PROVIDER_ID"
%s
[model_providers.$PROVIDER_ID]
name = "Pool Server"
base_url = "$ORIGIN/v1"
wire_api = "responses"
%s
EOF

echo "✅ 已写入 $CONFIG"
echo "   model=$MODEL  (reasoning/approval/sandbox 见 $CONFIG)"
echo ""
echo "现在直接运行即可使用本池："
echo "    codex \"你的需求\""
echo ""
echo "• 可用模型列表： $ORIGIN/v1/models"
echo "• 切换模型：编辑 $CONFIG 中的 model = 字段"
echo "• 关闭全自动：把 approval_policy 改为 \"on-request\"、sandbox_mode 改为 \"workspace-write\""
echo "• 还原配置：恢复上面备份的 .bak 文件"

# ── Claude Code 配置（写入/合并 ~/.claude/settings.json 的 env）──────────────
CLAUDE_DIR="$HOME/.claude"
CLAUDE_SETTINGS="$CLAUDE_DIR/settings.json"
echo ""
echo "🟣 配置 Claude Code 接入 Pool…"
mkdir -p "$CLAUDE_DIR"
if [ -f "$CLAUDE_SETTINGS" ]; then
  cp "$CLAUDE_SETTINGS" "$CLAUDE_SETTINGS.bak.$(date +%%Y%%m%%d%%H%%M%%S)" 2>/dev/null || true
fi
if command -v python3 >/dev/null 2>&1; then
  if python3 - "$CLAUDE_SETTINGS" "$ORIGIN" "$API_KEY" <<'PY'
import json, os, sys
path, origin, key = sys.argv[1], sys.argv[2], sys.argv[3]
data = {}
if os.path.exists(path):
    try:
        with open(path) as f: data = json.load(f)
    except Exception: data = {}
if not isinstance(data, dict): data = {}
env = data.get("env") if isinstance(data.get("env"), dict) else {}
env["ANTHROPIC_BASE_URL"] = origin
env["ANTHROPIC_AUTH_TOKEN"] = key if key else "any-non-empty"
data["env"] = env
with open(path, "w") as f: json.dump(data, f, indent=2)
PY
  then echo "✅ 已写入 Claude Code 配置 → $CLAUDE_SETTINGS (ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN)"
  else echo "⚠️  Claude Code 自动配置失败，请手动：export ANTHROPIC_BASE_URL=$ORIGIN ; export ANTHROPIC_AUTH_TOKEN=$API_KEY"
  fi
else
  echo "⚠️  未找到 python3，无法自动合并 Claude Code 配置；请手动设置环境变量："
  echo "      export ANTHROPIC_BASE_URL=$ORIGIN"
  echo "      export ANTHROPIC_AUTH_TOKEN=${API_KEY:-any-non-empty}"
fi
echo "   (Claude Code 直接运行 claude 即可走本池；模型用 Claude 系，由池路由)"

# ── (自动) rtk 客户端 token 压缩：命令感知地压缩工具输出，省上游 token ──────
# rtk 是第三方开源工具(Apache-2.0, github.com/rtk-ai/rtk)，在客户端命令执行前/后
# 压缩输出，比服务端通用压缩更精准。此步非致命且可跳过：重跑时加 POOL_SKIP_RTK=1。
if [ "${POOL_SKIP_RTK:-0}" != "1" ]; then
  echo ""
  echo "🔧 安装并接入 rtk（客户端 token 压缩，可用 POOL_SKIP_RTK=1 跳过）…"
  if ! command -v rtk >/dev/null 2>&1; then
    curl -fsSL https://raw.githubusercontent.com/rtk-ai/rtk/refs/heads/master/install.sh | sh \
      || echo "⚠️  rtk 安装失败（已跳过，不影响 Codex 配置）"
  fi
  export PATH="$HOME/.local/bin:$PATH"
  if command -v rtk >/dev/null 2>&1; then
    rtk init --codex >/dev/null 2>&1 && echo "✅ 已为 Codex 接入 rtk" || echo "⚠️  rtk Codex 接入未完成"
    if [ -d "$HOME/.claude" ]; then
      rtk init -g --hook-only --auto-patch >/dev/null 2>&1 \
        && echo "✅ 已为 Claude Code 接入 rtk PreToolUse hook" || true
    fi
  fi
fi
`, shellQuote(origin), shellQuote(safeKey), shellQuote(model), extra.String(), keyLine)
}
