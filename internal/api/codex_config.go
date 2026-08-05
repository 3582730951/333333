package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// handleCodexConfigScript serves GET /file/{apikey} (the key may also arrive as
// ?key=… or in the Authorization header): a one-shot bash script that points the
// official `codex` CLI and Claude Code at THIS pool. The Codex branch only writes
// the requested config allowlist. The separate Claude branch initializes the
// gateway runtime and may install RTK when explicitly selected. Claude defaults to
// compat mode so real HOME keeps official skills/plugins/MCP; strict Linux
// isolation remains an explicit advanced option.
//
// Config shape is verified against codex-rs (codex-model-provider-info):
//   - wire_api MUST be "responses" ("chat" was removed upstream).
//   - supports_websockets MUST be true. Custom providers default this capability
//     to false, while Codex's built-in OpenAI provider enables it. The pool
//     implements the persistent Responses WebSocket protocol, including v2
//     prewarm, incremental previous_response_id continuation, and HTTPS fallback.
//   - experimental_bearer_token carries the one configured downstream API key
//     without adding another credential file or auth-command configuration.
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
		// Operator-configured install default (gpt-5.6-sol). Falls back to the best probed
		// codex model only if the configured default is blanked out.
		model = strings.TrimSpace(s.settingString(r.Context(), "codex_install_model", s.cfg.CodexInstallModel))
	}
	if model == "" {
		model = s.pickDefaultCodexModel(r.Context(), group)
	}

	origin := s.externalOrigin(r)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	codexOnly := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("client")), "codex")
	filename := "setup-pool-cli.sh"
	if codexOnly {
		filename = "setup-pool-codex.sh"
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	effort := s.settingString(r.Context(), "codex_install_effort", s.cfg.CodexInstallEffort)
	approval := s.settingString(r.Context(), "codex_install_approval_policy", s.cfg.CodexInstallApprovalPolicy)
	sandbox := s.settingString(r.Context(), "codex_install_sandbox_mode", s.cfg.CodexInstallSandboxMode)
	scriptOptions := CodexSetupScriptOptions{
		StrictLinuxDefault:     s.flagEnabled(r.Context(), "claude_gateway_strict_linux_default", s.cfg.ClaudeGatewayStrictLinuxDefault),
		DisableNonessentialEnv: s.flagEnabled(r.Context(), "claude_gateway_disable_nonessential_env", s.cfg.ClaudeGatewayDisableNonessentialEnv),
		CodexOnly:              codexOnly,
	}
	_, _ = w.Write([]byte(buildCodexConfigScript(origin, key, model, effort, approval, sandbox, scriptOptions)))
}

// externalOrigin reconstructs the publicly reachable scheme://host for this request,
// honoring the X-Forwarded-Proto/Host set by a fronting reverse proxy (where r.TLS
// is nil and r.Host may be the internal address). Falls back to the direct values.
func (s *Server) externalOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if s.isTrustedProxyRequest(r) {
		if xf := lastForwardedValue(r.Header.Get("X-Forwarded-Proto")); strings.EqualFold(xf, "http") || strings.EqualFold(xf, "https") {
			scheme = strings.ToLower(xf)
		}
		if xh := lastForwardedValue(r.Header.Get("X-Forwarded-Host")); validForwardedHost(xh) {
			host = xh
		}
	}
	return scheme + "://" + strings.TrimSpace(host)
}

func validForwardedHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/\\@?#\r\n\t ") {
		return false
	}
	return true
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

type CodexSetupScriptOptions struct {
	StrictLinuxDefault     bool
	DisableNonessentialEnv bool
	// CodexOnly emits a direct configuration-only installer. It has no Claude
	// gateway, RTK, skill, plugin, model-cache, or client-ID branch.
	CodexOnly bool
}

func defaultCodexSetupScriptOptions() CodexSetupScriptOptions {
	return CodexSetupScriptOptions{
		StrictLinuxDefault:     false,
		DisableNonessentialEnv: false,
	}
}

// buildCodexConfigScript renders the bash installer. It backs up any existing
// config.toml, honors CODEX_HOME, and normally manages only model, model_provider,
// and Pool's provider authentication table. Reasoning effort, approval, and sandbox
// become managed root keys only when an operator supplied a non-empty override;
// all other Codex settings remain client-owned. Claude Code remains a separate
// local-gateway branch.
func buildCodexConfigScript(origin, apiKey, model, effort, approval, sandbox string, options ...CodexSetupScriptOptions) string {
	scriptOptions := defaultCodexSetupScriptOptions()
	if len(options) > 0 {
		scriptOptions = options[0]
	}
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	safeKey := strings.ReplaceAll(apiKey, "'", "")
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
	managedRootKeys := []string{"model", "model_provider"}
	if e := clean(effort); e != "" {
		fmt.Fprintf(&extra, "model_reasoning_effort = \"%s\"\n", e)
		managedRootKeys = append(managedRootKeys, "model_reasoning_effort")
	}
	if a := clean(approval); a != "" {
		fmt.Fprintf(&extra, "approval_policy = \"%s\"\n", a)
		managedRootKeys = append(managedRootKeys, "approval_policy")
	}
	if sb := clean(sandbox); sb != "" {
		fmt.Fprintf(&extra, "sandbox_mode = \"%s\"\n", sb)
		managedRootKeys = append(managedRootKeys, "sandbox_mode")
	}
	managedRootPattern := strings.Join(managedRootKeys, "|")
	if scriptOptions.CodexOnly {
		return buildCodexOnlyConfigScript(
			shellQuote(origin),
			shellQuote(safeKey),
			shellQuote(model),
			extra.String(),
			shellQuote(managedRootPattern),
		)
	}
	strictDefault := "1"
	if !scriptOptions.StrictLinuxDefault {
		strictDefault = "0"
	}
	runtimeDefault := "strict"
	if !scriptOptions.StrictLinuxDefault {
		runtimeDefault = "compat"
	}
	disableNonessentialEnv := renderClaudeDisableNonessentialEnvExports(scriptOptions.DisableNonessentialEnv, "  ")
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

ORIGIN=%s
API_KEY=%s
MODEL=%s
PROVIDER_ID="poolserver"

say() { printf '%%s\n' "$*"; }

usage_setup() {
  cat <<'EOF_USAGE'
Usage: setup-pool-cli.sh [options]

Options:
  --client-runtime compat|strict   Claude Code runtime mode. Default is server configured.
  --doctor-only                    Print /admin/compat/skills and exit. Set ADMIN_TOKEN if required.
  -h, --help                       Show this help.

Environment:
  POOL_CLIENT=claude|codex
  POOL_INSTALL_RTK=1|0
  POOL_CLIENT_RUNTIME=compat|strict
  ADMIN_TOKEN=<token>              Optional admin token for --doctor-only.
EOF_USAGE
}

DOCTOR_ONLY=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --client-runtime)
      POOL_CLIENT_RUNTIME="${2:?missing value for --client-runtime}"
      shift 2
      ;;
    --client-runtime=*)
      POOL_CLIENT_RUNTIME="${1#*=}"
      shift
      ;;
    --doctor-only)
      DOCTOR_ONLY=1
      shift
      ;;
    -h|--help)
      usage_setup
      exit 0
      ;;
    *)
      say "Unknown option: $1"
      usage_setup
      exit 1
      ;;
  esac
done

POOL_CLIENT_RUNTIME="${POOL_CLIENT_RUNTIME:-%s}"
case "$POOL_CLIENT_RUNTIME" in
  compat|strict) ;;
  *) say "Invalid POOL_CLIENT_RUNTIME=$POOL_CLIENT_RUNTIME. Use compat or strict."; exit 1 ;;
esac
if [ -z "${POOL_STRICT_LINUX+x}" ]; then
  if [ "$POOL_CLIENT_RUNTIME" = "strict" ]; then
    POOL_STRICT_LINUX=1
  else
    POOL_STRICT_LINUX=0
  fi
fi
export POOL_CLIENT_RUNTIME POOL_STRICT_LINUX

run_doctor() {
  local curl_args=()
  if [ -n "${ADMIN_TOKEN:-}" ]; then
    curl_args=(-H "Authorization: Bearer $ADMIN_TOKEN")
  fi
  curl -fsSL "${curl_args[@]}" "$ORIGIN/admin/compat/skills"
  printf '\n'
}

if [ "$DOCTOR_ONLY" = "1" ]; then
  run_doctor
  exit 0
fi

prompt_line() {
  if [ -w /dev/tty ]; then
    printf '%%s\n' "$*" > /dev/tty
  else
    printf '%%s\n' "$*" >&2
  fi
}

read_tty() {
  local prompt="$1"
  local answer=""
  if [ -r /dev/tty ]; then
    printf '%%s' "$prompt" > /dev/tty
    IFS= read -r answer < /dev/tty
  else
    printf '%%s' "$prompt" >&2
    IFS= read -r answer || true
  fi
  printf '%%s' "$answer"
}

select_client() {
  case "${POOL_CLIENT:-}" in
    claude|Claude|CLAUDE) printf 'claude'; return ;;
    codex|Codex|CODEX) printf 'codex'; return ;;
    "") ;;
    *) say "Invalid POOL_CLIENT=${POOL_CLIENT}. Use claude or codex."; exit 1 ;;
  esac
  prompt_line "选择客户端:"
  prompt_line "  1) Claude Code"
  prompt_line "  2) Codex"
  local choice
  choice="$(read_tty '请输入 1 或 2: ')"
  case "$choice" in
    1) printf 'claude' ;;
    2) printf 'codex' ;;
    *) say "未选择有效客户端。自动化请设置 POOL_CLIENT=claude|codex"; exit 1 ;;
  esac
}

select_rtk() {
  if [ "${POOL_SKIP_RTK:-0}" = "1" ]; then
    printf '0'
    return
  fi
  case "${POOL_INSTALL_RTK:-}" in
    1|true|TRUE|yes|YES) printf '1'; return ;;
    0|false|FALSE|no|NO) printf '0'; return ;;
    "") ;;
    *) say "Invalid POOL_INSTALL_RTK=${POOL_INSTALL_RTK}. Use 1 or 0."; exit 1 ;;
  esac
  prompt_line ""
  prompt_line "选择 skills/RTK 安装:"
  prompt_line "  1) 安装 RTK"
  prompt_line "  2) 不安装任何 skills"
  local choice
  choice="$(read_tty '请输入 1 或 2: ')"
  case "$choice" in
    1) printf '1' ;;
    2) printf '0' ;;
    *) say "未选择有效 RTK 选项。自动化请设置 POOL_INSTALL_RTK=1|0"; exit 1 ;;
  esac
}

ensure_rtk() {
  if command -v rtk >/dev/null 2>&1; then
    return 0
  fi
  curl -fsSL https://raw.githubusercontent.com/rtk-ai/rtk/refs/heads/master/install.sh | sh
  export PATH="$HOME/.local/bin:$PATH"
  command -v rtk >/dev/null 2>&1
}

ensure_user_local_bin_on_path() {
  mkdir -p "$HOME/.local/bin"
  case ":$PATH:" in
    *":$HOME/.local/bin:"*) ;;
    *) export PATH="$HOME/.local/bin:$PATH" ;;
  esac
  local profile="$HOME/.profile"
  if [ -n "${BASH_VERSION:-}" ]; then
    profile="$HOME/.bashrc"
  fi
  touch "$profile"
  if ! grep -F 'export PATH="$HOME/.local/bin:$PATH"' "$profile" >/dev/null 2>&1; then
    printf '\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$profile"
    say "已写入 PATH -> $profile"
  fi
}

configure_codex() {
  local CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
  local CONFIG="$CODEX_HOME/config.toml"

  say "配置 Codex 接入 Pool: $ORIGIN"
  mkdir -p "$CODEX_HOME"
  umask 077
  if [ -L "$CONFIG" ]; then
    say "Refusing to replace a symlinked Codex config file."
    exit 1
  fi
  local CONFIG_TMP
  CONFIG_TMP="$(mktemp "$CODEX_HOME/.config.toml.tmp.XXXXXX")"

  if [ -f "$CONFIG" ]; then
    local BAK="$CONFIG.bak.$(date +%%Y%%m%%d%%H%%M%%S).$$"
    cp "$CONFIG" "$BAK"
    say "已备份原有配置 -> $BAK"
  fi

  # TOML root keys must precede every table. Seed only explicitly managed roots,
  # preserve every client-owned setting/table, and replace only Pool's provider.
  cat > "$CONFIG_TMP" <<EOF
# Generated by Pool — points the codex CLI at a self-hosted pool.
model = "$MODEL"
model_provider = "$PROVIDER_ID"
%s
EOF

  if [ -f "$CONFIG" ]; then
    awk -v provider="$PROVIDER_ID" -v managed_root_keys=%s '
      BEGIN { section=""; skip=0 }
      /^\[/ {
        section=$0
        skip=0
        if ($0 == "[model_providers." provider "]" ||
            index($0, "[model_providers." provider ".") == 1) { skip=1; next }
      }
      skip { next }
	      section == "" && $0 ~ ("^[[:space:]]*(" managed_root_keys ")[[:space:]]*=") { next }
      { print }
    ' "$CONFIG" >> "$CONFIG_TMP"
  fi

  cat >> "$CONFIG_TMP" <<EOF
[model_providers.$PROVIDER_ID]
name = "OpenAI"
base_url = "$ORIGIN/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
experimental_bearer_token = "$API_KEY"
EOF

  if ! mv "$CONFIG_TMP" "$CONFIG"; then
    say "Codex config commit failed."
    exit 1
  fi

  say "已写入 $CONFIG"
  say "已配置模型、Pool provider 与 API key；其余 Codex 配置保持原值"
  say "model=$MODEL"
}

GATEWAY_BIN=""
install_gateway_binary() {
  local os arch target tmp existing_gateway
  existing_gateway="$(command -v gateway 2>/dev/null || true)"
  if [ -n "$existing_gateway" ]; then
    "$existing_gateway" stop || true
  fi
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
  esac
  tmp="$(mktemp)"
  curl -fsSL "$ORIGIN/download/gateway?os=$os&arch=$arch" -o "$tmp"
  chmod +x "$tmp"
  if [ -n "$existing_gateway" ] && [ -w "$existing_gateway" ]; then
    target="$existing_gateway"
  elif [ "$(id -u)" = "0" ]; then
    mkdir -p /usr/local/bin
    target="/usr/local/bin/gateway"
  else
    ensure_user_local_bin_on_path
    target="$HOME/.local/bin/gateway"
  fi
  mv "$tmp" "$target"
  GATEWAY_BIN="$target"
  export PATH="$(dirname "$target"):$PATH"
  say "已安装/更新 gateway -> $target ($os/$arch)"
}

configure_claude() {
  local install_rtk="$1"
  local GATEWAY_KEY="${API_KEY:-any-non-empty}"
  if [ "$POOL_CLIENT_RUNTIME" = "strict" ] && [ "$(uname -s)" != "Linux" ]; then
    say "Claude Code strict runtime only supports Linux. Use POOL_CLIENT_RUNTIME=compat to preserve the official local client ecosystem."
    exit 1
  fi
  export POOL_CLIENT_RUNTIME="${POOL_CLIENT_RUNTIME:-%s}"
  export POOL_STRICT_LINUX="${POOL_STRICT_LINUX:-%s}"
  # Gateway mode uses the bearer-token credential path recommended for custom
  # Anthropic-compatible endpoints. Remove an inherited API key so Claude Code
  # does not enter its direct API-key approval/login flow.
  export ANTHROPIC_BASE_URL="$ORIGIN"
  export ANTHROPIC_AUTH_TOKEN="$GATEWAY_KEY"
  unset ANTHROPIC_API_KEY
  export CLAUDE_CODE_ENABLE_AUTO_MODE=1
  export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
  export CLAUDE_CODE_ENABLE_FINE_GRAINED_TOOL_STREAMING=1
%s

  say "配置 Claude Code 本地 gateway runtime ($POOL_CLIENT_RUNTIME): $ORIGIN"
  say "Claude Code API: ANTHROPIC_BASE_URL=$ORIGIN"
  say "Claude Code 模型由客户端自行选择；VPS force_model 可能在服务端覆盖"
  install_gateway_binary
  "$GATEWAY_BIN" init --pool-url "$ORIGIN" --key "$GATEWAY_KEY"
  "$GATEWAY_BIN" stop || true
  if ! "$GATEWAY_BIN" trust-ca; then
    say "CA 自动信任失败；安装尚未完成。请执行 $GATEWAY_BIN trust-ca --print-commands 后重跑本脚本"
    exit 1
  fi

  if [ "$install_rtk" = "1" ]; then
    say "安装并接入 RTK (Claude Code hook)..."
    if ensure_rtk; then
      rtk init -g --hook-only --auto-patch >/dev/null 2>&1 && say "已为 Claude Code 接入 RTK hook" || say "RTK Claude hook 接入未完成"
    else
      say "RTK 安装失败，已跳过"
    fi
  fi

  "$GATEWAY_BIN" probe-identity
  if command -v claude >/dev/null 2>&1; then
    "$GATEWAY_BIN" install-wrapper || say "包装器安装失败；可直接运行 $GATEWAY_BIN run-claude -- <args>"
  fi
  "$GATEWAY_BIN" start-background || say "后台启动失败；请查看 ~/.claude-gateway/gateway.log"
  say "Claude Code 运行方式:"
  say "  claude \"你的需求\""
  say "  claude-plan \"先写计划再执行\""
  say "  $GATEWAY_BIN status"
  say "  tail -f ~/.claude-gateway/gateway.log"
}

main() {
  local client
  client="$(select_client)"
  case "$client" in
    codex) configure_codex ;;
    claude)
      local install_rtk
      install_rtk="$(select_rtk)"
      configure_claude "$install_rtk"
      ;;
  esac
}

main "$@"
`, shellQuote(origin), shellQuote(safeKey), shellQuote(model), runtimeDefault, extra.String(), shellQuote(managedRootPattern), runtimeDefault, strictDefault, disableNonessentialEnv)
}

// buildCodexOnlyConfigScript is intentionally self-contained and configuration
// only. It does not install Codex or companion software. Its sole mutation is an
// atomic merge of the managed Codex allowlist into config.toml, preceded by a
// timestamped backup when that file already exists.
func buildCodexOnlyConfigScript(origin, apiKey, model, behaviorLines, managedRootKeys string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

ORIGIN=%s
API_KEY=%s
MODEL=%s
PROVIDER_ID="poolserver"

say() { printf '%%s\n' "$*"; }

CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"
CONFIG="$CODEX_HOME/config.toml"
mkdir -p "$CODEX_HOME"
umask 077

if [ -L "$CONFIG" ]; then
  say "Refusing to replace a symlinked Codex config file."
  exit 1
fi

CONFIG_TMP="$(mktemp "$CODEX_HOME/.config.toml.tmp.XXXXXX")"
cleanup() { rm -f "$CONFIG_TMP"; }
trap cleanup EXIT

if [ -f "$CONFIG" ]; then
  BAK="$CONFIG.bak.$(date +%%Y%%m%%d%%H%%M%%S).$$"
  cp "$CONFIG" "$BAK"
  say "已备份原有配置 -> $BAK"
fi

cat > "$CONFIG_TMP" <<EOF
# Generated by Pool — points the Codex CLI at this pool.
model = "$MODEL"
model_provider = "$PROVIDER_ID"
%s
EOF

if [ -f "$CONFIG" ]; then
  awk -v provider="$PROVIDER_ID" -v managed_root_keys=%s '
    BEGIN { section=""; skip=0 }
    /^\[/ {
      section=$0
      skip=0
      if ($0 == "[model_providers." provider "]" ||
          index($0, "[model_providers." provider ".") == 1) { skip=1; next }
    }
    skip { next }
    section == "" && $0 ~ ("^[[:space:]]*(" managed_root_keys ")[[:space:]]*=") { next }
    { print }
  ' "$CONFIG" >> "$CONFIG_TMP"
fi

cat >> "$CONFIG_TMP" <<EOF
[model_providers.$PROVIDER_ID]
name = "OpenAI"
base_url = "$ORIGIN/v1"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
experimental_bearer_token = "$API_KEY"
EOF

mv "$CONFIG_TMP" "$CONFIG"
trap - EXIT
say "已写入 $CONFIG"
say "已配置模型、Pool provider 与 API key；其余 Codex 配置保持原值"
say "model=$MODEL"
`, origin, apiKey, model, behaviorLines, managedRootKeys)
}

func renderClaudeDisableNonessentialEnvExports(enabled bool, indent string) string {
	if !enabled {
		return ""
	}
	lines := []string{
		"export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"export DO_NOT_TRACK=1",
		"export DISABLE_TELEMETRY=1",
		"export DISABLE_ERROR_REPORTING=1",
		"export DISABLE_AUTOUPDATER=1",
		"export OTEL_METRICS_EXPORTER=none",
		"export OTEL_LOGS_EXPORTER=none",
	}
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}
