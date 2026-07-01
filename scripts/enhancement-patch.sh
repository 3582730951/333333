#!/usr/bin/env bash
# enhancement-patch.sh — 为 update.sh 和 install.sh 添加 3 个增强功能
# 基于 Session 27 审计报告（docs/deployment-audit-report.md）
#
# 增强内容：
#   1. install.sh 集成 Chrome headless 安装（PayPal 自动化需要）
#   2. update.sh 自动修复 registry.go 损坏（Session 27 sed 污染）
#   3. 生成 Python requirements.lock（版本锁定）
#
# 运行方法：
#   sudo bash scripts/enhancement-patch.sh
#
set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

log() { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# ============================================================================
# 增强 1: install.sh 集成 Chrome 安装
# ============================================================================
patch_install_sh_chrome() {
  local f="scripts/install.sh"
  [[ -f "$f" ]] || die "$f not found"

  # 检查是否已打补丁
  if grep -q "install_chrome_for_paypal" "$f"; then
    log "install.sh Chrome 补丁已应用，跳过"
    return 0
  fi

  log "打补丁: install.sh 集成 Chrome 安装..."

  # 在 install_gopay 函数后添加 install_chrome_for_paypal 函数
  local insert_line=$(grep -n "^install_gopay()" "$f" | tail -1 | cut -d: -f1)
  if [[ -z "$insert_line" ]]; then
    warn "未找到 install_gopay 函数，跳过 Chrome 补丁"
    return 1
  fi

  # 找到 install_gopay 函数的结束行（下一个空行）
  insert_line=$(awk -v start="$insert_line" 'NR>start && /^$/{print NR; exit}' "$f")

  # 插入新函数
  sed -i "${insert_line}i\\
# install_chrome_for_paypal installs Chrome/Chromium for PayPal headless automation.\\
# Skipped if Chrome is already installed or if scripts/install_chrome_headless.sh is missing.\\
install_chrome_for_paypal() {\\
  [[ \"\$WITH_GOPAY\" -eq 1 ]] || return 0\\
  [[ -f \"\${PROJECT_ROOT}/scripts/install_chrome_headless.sh\" ]] || {\\
    warn \"scripts/install_chrome_headless.sh not found — PayPal automation requires Chrome\"\\
    return 0\\
  }\\
  \\
  # Check if Chrome is already installed\\
  if command -v google-chrome >/dev/null 2>&1 || command -v google-chrome-stable >/dev/null 2>&1 || command -v chromium-browser >/dev/null 2>&1; then\\
    log \"Chrome/Chromium already installed, skipping\"\\
    return 0\\
  fi\\
  \\
  log \"Installing Chrome for PayPal headless automation (PayPal Plus auto-subscribe requires headless browser)...\"\\
  if run_root bash \"\${PROJECT_ROOT}/scripts/install_chrome_headless.sh\"; then\\
    log \"Chrome installed successfully\"\\
  else\\
    warn \"Chrome installation failed — PayPal automation will not work\"\\
    warn \"Run manually: sudo bash scripts/install_chrome_headless.sh\"\\
  fi\\
}\\
" "$f"

  # 在 main() 中调用（在 install_gopay 之后）
  local main_line=$(grep -n "^main()" "$f" | cut -d: -f1)
  local gopay_call=$(awk -v start="$main_line" 'NR>start && /install_gopay/{print NR; exit}' "$f")

  if [[ -n "$gopay_call" ]]; then
    sed -i "${gopay_call}a\\  install_chrome_for_paypal" "$f"
    log "✅ install.sh Chrome 补丁已应用"
  else
    warn "未找到 install_gopay 调用，请手动添加 install_chrome_for_paypal"
  fi
}

# ============================================================================
# 增强 2: update.sh 自动修复 registry.go
# ============================================================================
patch_update_sh_registry() {
  local f="update.sh"
  [[ -f "$f" ]] || die "$f not found"

  # 检查是否已打补丁
  if grep -q "repair_known_corruptions" "$f"; then
    log "update.sh registry 修复补丁已应用，跳过"
    return 0
  fi

  log "打补丁: update.sh 自动修复 registry.go..."

  # 在 diagnose_build_failure 之前添加 repair_known_corruptions 函数
  local insert_line=$(grep -n "^diagnose_build_failure()" "$f" | cut -d: -f1)
  if [[ -z "$insert_line" ]]; then
    warn "未找到 diagnose_build_failure 函数，跳过 registry 补丁"
    return 1
  fi

  sed -i "${insert_line}i\\
# repair_known_corruptions auto-fixes known source file corruptions (e.g. Session 27\\
# registry.go sed pollution). This is an evolving self-healing mechanism: when a new\\
# corruption pattern is discovered, add its fix here so every deployment converges to\\
# a clean state without manual intervention.\\
repair_known_corruptions() {\\
  local f=\"\${PROJECT_ROOT}/internal/registration/provider/registry.go\"\\
  [[ -f \"\$f\" ]] || return 0\\
  \\
  # Session 27 sed pollution: JSON map literals injected mid-function\\
  if ! grep -q '^\\\t\\\t\\\t\"guerrillamail\":' \"\$f\" 2>/dev/null; then\\
    return 0  # file is clean\\
  fi\\
  \\
  log \"检测到 registry.go 损坏（历史 sed 操作污染的 JSON 残留），自动修复中...\"\\
  \\
  if command -v python3 >/dev/null 2>&1; then\\
    python3 << 'EOPYTHON'\\
import sys\\
f = \"internal/registration/provider/registry.go\"\\
try:\\
    with open(f) as fp: lines = fp.readlines()\\
except FileNotFoundError:\\
    sys.exit(0)\\
cleaned, skip = [], False\\
for line in lines:\\
    if '\\\\t\\\\t\\\\t\"guerrillamail\":' in line or '\\\\t\\\\t\\\\t\"mail_tm\":' in line or '\\\\t\\\\t\\\\t\"tenminutemail\":' in line:\\
        skip = True\\
        continue\\
    if skip and line.strip() == '}':\\
        skip = False\\
        continue\\
    if not skip:\\
        cleaned.append(line)\\
with open(f, 'w') as fp: fp.writelines(cleaned)\\
print(f\"✅ 已自动修复 registry.go（清理 {len(lines)-len(cleaned)} 行损坏代码）\")\\
EOPYTHON\\
  else\\
    warn \"Python3 未安装，无法自动修复 registry.go — 请手动运行修复脚本（见 docs/deployment-audit-report.md）\"\\
    return 1\\
  fi\\
}\\
\\
" "$f"

  # 在 main() 中调用（在 remove_stale_sources 之后）
  local main_line=$(grep -n "^main()" "$f" | cut -d: -f1)
  local stale_call=$(awk -v start="$main_line" 'NR>start && /remove_stale_sources/{print NR; exit}' "$f")

  if [[ -n "$stale_call" ]]; then
    sed -i "${stale_call}a\\  repair_known_corruptions" "$f"
    log "✅ update.sh registry 修复补丁已应用"
  else
    warn "未找到 remove_stale_sources 调用，请手动添加 repair_known_corruptions"
  fi
}

# ============================================================================
# 增强 3: 生成 Python requirements.lock
# ============================================================================
generate_requirements_lock() {
  local dirs=(
    "sidecar"
    "gopay/plus"
    "services/chatgpt_register"
    "services/plus_payment"
  )

  log "生成 Python requirements.lock 文件..."

  for dir in "${dirs[@]}"; do
    [[ -d "$dir" ]] || continue
    [[ -f "$dir/requirements.txt" ]] || continue

    # 检查是否已有 lock 文件
    if [[ -f "$dir/requirements.lock" ]]; then
      log "  $dir/requirements.lock 已存在，跳过"
      continue
    fi

    log "  生成 $dir/requirements.lock..."

    # 创建临时 venv
    local tmp_venv="/tmp/lock-venv-$$"
    python3 -m venv "$tmp_venv" 2>/dev/null || {
      warn "无法创建 venv，跳过 $dir"
      continue
    }

    # 安装依赖并生成 lock
    (
      source "$tmp_venv/bin/activate"
      cd "$dir"
      pip install --quiet -r requirements.txt 2>/dev/null || {
        warn "依赖安装失败，跳过 $dir"
        return 1
      }
      pip freeze > requirements.lock
      echo "✅ $dir/requirements.lock 已生成（$(wc -l < requirements.lock) 个包）"
      deactivate
    )

    rm -rf "$tmp_venv"
  done
}

# ============================================================================
# Main
# ============================================================================
main() {
  log "应用 3 个增强补丁..."
  echo ""

  # 补丁 1: Chrome 安装
  if patch_install_sh_chrome; then
    echo ""
  fi

  # 补丁 2: registry.go 自愈
  if patch_update_sh_registry; then
    echo ""
  fi

  # 补丁 3: requirements.lock
  if command -v python3 >/dev/null 2>&1; then
    generate_requirements_lock
  else
    warn "Python3 未安装，跳过 requirements.lock 生成"
  fi

  echo ""
  log "补丁应用完成！"
  echo ""
  echo "建议："
  echo "  1. 检查修改: git diff update.sh scripts/install.sh"
  echo "  2. 测试更新: sudo ./update.sh --no-tests"
  echo "  3. 验证服务: curl http://localhost:8787/healthz"
  echo ""
  echo "注意："
  echo "  - Chrome 会在下次 install.sh 运行时自动安装"
  echo "  - registry.go 会在下次 update.sh 运行时自动修复"
  echo "  - requirements.lock 需手动提交到版本控制"
}

main "$@"
