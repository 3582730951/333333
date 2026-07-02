# 三项目 Plus 流水线

横跨 **PP自动注册** → **plus_gopay_links** → **gopay/opai**，对应教程四阶段。

## 项目分工

| 项目 | 路径 | 负责 |
|------|------|------|
| PP自动注册 | `<PP_AUTO_REGISTER_ROOT>` | ChatGPT 注册、Free offer、`access_token` 入库 |
| plus_gopay_links | `gopay plus/plus_gopay_links` | `access_token` → IDR Checkout → **Midtrans snap URL** |
| gopay/opai | `gopay plus/gopay` | GoPay 注册、1 Rp、Midtrans **协议付款**（第 3 次 SMS） |

## 架构

```
PP自动注册 (MySQL product_assets.token)
        │
        ▼  export → gopay/config/chatgpt_token.json
plus_gopay_links/checkout_to_midtrans.py  (日本 IP 代理)
        │
        ▼  midtrans_url
Payment Inbox  ──►  opai worker run  (印尼 Cliproxy 链)
        │
        ▼
ChatGPT Plus 开通
```

## 一次性准备

### 1. 依赖

```powershell
# GoPay 协议
cd gopay\app
pip install -e .

# Checkout 桥接（Stripe TLS 指纹）
pip install -r ..\..\plus_gopay_links\requirements.txt
```

### 2. 主配置（已写入 `config/pipeline.json`）

| 项 | 当前值 |
|----|--------|
| PP MySQL | `root@127.0.0.1/plus_papay`（与 PP自动注册 `.env` 同步） |
| 邮箱池待激活 | **28** 个（`setup_verify.ps1` 可复查） |
| Payment Inbox | `http://127.0.0.1:8765` / `opai` |
| Checkout 代理 | `http://127.0.0.1:10809` |
| GoPay 链 | v2rayN 10809 → `cliproxy.url` 印尼 |
| 默认 PIN | `147258` |
| Hero-SMS | `config/herosms_api_key` |

```powershell
cd gopay
.\setup_verify.ps1   # 一键检查依赖 + MySQL + 池子
```

所有 `start_*.ps1` / `run_pipeline.ps1` 会自动加载 `scripts/load_env.ps1`。

### 3. 代理

v2rayN 需常开（10809）。PP 注册代理在 PP 项目里是 `42005`，GoPay/Checkout 流水线用 **10809**。

## 从 PP 邮箱池批量激活（推荐）

**不再新注册 ChatGPT**。从 `PP自动注册` 的 `pool_emails` 里取「已注册、未开 Plus、有 token」的号。

### 环境

已写入 `config/pipeline.json`，无需再设 `$env:PP_MYSQL_PASSWORD`。

### 查看池子

```powershell
cd gopay
.\run_pipeline.ps1 pipeline pool stats
.\run_pipeline.ps1 pipeline pool list --limit 30
```

### 批量入队（ChatGPT 用池子，GoPay 仍由 worker 新注册）

```powershell
# 终端 1: .\start_inbox.ps1
# 终端 2: .\start_worker.ps1 --workers 3

# 终端 3: 一次入队 5 个 ChatGPT 成品
.\run_pipeline.ps1 pipeline from-pool --count 5 --enqueue-only
```

Worker 付款成功后会自动把 PP 池里对应邮箱标为 `activated`（job notes 含 `pool_id=`）。

### 单号直跑

```powershell
.\run_pipeline.ps1 pipeline from-pool --email user@outlook.com --pin 147258
# 或自动取下一个
.\run_pipeline.ps1 pipeline from-pool --pin 147258
```

---

## 三种跑法（手动 token 文件）

### 模式 A：批量（Inbox + Worker）

三个终端：

```powershell
# 终端 1 — Payment Inbox
cd gopay
.\start_inbox.ps1

# 终端 2 — GoPay Worker（自动注册 + 付款）
.\start_worker.ps1 --workers 1 --pin 147258

# 终端 3 — 从 PP token 入队
.\run_pipeline.ps1 pipeline enqueue `
  --token-file config\chatgpt_token.json `
  --email 成品邮箱@outlook.com
```

Worker 会从 Inbox 取 `provider_url`（Midtrans），注册 GoPay、等 1 Rp、协议付款。

### 模式 B：单号一条龙

```powershell
cd gopay
.\run_pipeline.ps1 pipeline run-one `
  --token-file config\chatgpt_token.json `
  --email 成品邮箱@outlook.com `
  --pin 147258
```

### 模式 C：分步调试

```powershell
# 1. 只测 checkout（ChatGPT → Midtrans URL）
.\run_pipeline.ps1 pipeline checkout --token-file config\chatgpt_token.json

# 2. GoPay 注册
.\start_worker.ps1 worker dry-run --pin 147258

# 3. 领 1 Rp
python -m opai worker fund +628xxxxxxxxxx

# 4. 单独付款（自动第 3 次 SMS，从 accounts 文件读 activation_id）
python -m opai pay "https://app.midtrans.com/snap/v4/redirection/..." `
  --phone 838xxxxxxxxxx --pin 147258
```

## 短信时间线（3 次）

| 次序 | 阶段 | 模块 |
|------|------|------|
| 1 | Gojek 注册 OTP | opai worker |
| 2 | 设 PIN OTP | opai worker（`sms_request_another`） |
| 3 | Midtrans 付款 OTP | opai pay / worker `_pay_job` |

PP自动注册 的 ChatGPT 注册短信 **不计入** 这 3 次。

## 常见问题

**Checkout 报 `not_eligible`**
PP 成品无 `plus-1-month-free` 资格，换号。

**GoPay 余额 0**
等新人礼 / App Hadiah / 有效红包链接 → `envelope_links.txt`。

**Checkout 403 / Cloudflare**
安装 `curl_cffi`；确认日本 IP 代理。

**Worker 不取 job**
检查 Inbox 地址、`OPAI_PAYMENT_INBOX_BASIC_*` 与 `start_inbox.ps1` 一致。

## CLI 速查

```text
opai pipeline checkout   # token → midtrans_url
opai pipeline enqueue    # token → Inbox job
opai pipeline run-one    # 单号全流程
opai pipeline inbox      # 启动 Inbox
opai worker run          # 消费 Inbox
opai pay                 # 单独 Midtrans 付款
```

教程原文：`docs/gopay-plus-tutorial/GoPay-GoJek-支付GPT-Plus-手搓指南.md`
