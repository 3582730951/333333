# GoPay Protocol Deploy

Pure-API GoPay registration + Midtrans payment pipeline. No browser, no ADB, no emulator.

**三项目流水线（PP自动注册 + plus_gopay_links + opai）见 [docs/PIPELINE.md](docs/PIPELINE.md)。**

## Quick Start

```bash
# Set Hero-SMS API key
set OPAI_HEROSMS_API_KEY=your_key_here

# Run 3 parallel workers (register + wait for balance + pay)
./start_worker.bat --workers 3 --pin 147258

# Or via Python directly
cd app
python -m opai worker run --workers 3 --pin 147258

# Dry run (register one account only, no payment)
python -m opai worker dry-run --pin 147258

# Test a single Midtrans payment
python -m opai pay "https://app.midtrans.com/snap/v4/redirection/<snap_id>" --phone 85142447768 --pin 147258

# Check balance of a saved account
python -m opai worker balance +6285142447768

# Resume from existing accounts (skip registration)
python -m opai worker run --resume +6285142447768 +6281234567890
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `OPAI_HEROSMS_API_KEY` | (required) | Hero-SMS API key for phone rental |
| `OPAI_HEROSMS_API_KEY_FILE` | | Path to file containing API key |
| `OPAI_PAYMENT_INBOX_BASE_URL` | (required) | Payment Inbox server URL |
| `OPAI_PAYMENT_INBOX_BASIC_USER` | (required) | Inbox auth user |
| `OPAI_PAYMENT_INBOX_BASIC_PASS` | (required) | Inbox auth password |
| `OPAI_GOPAY_POLL_INTERVAL` | `10` | Inbox poll frequency (seconds) |
| `OPAI_GOPAY_MIN_REMAINING_SEC` | `300` | Min job remaining time to claim |
| `OPAI_GOPAY_DEFAULT_PIN` | `147258` | Default 6-digit PIN |
| `OPAI_GOPAY_MIN_BALANCE_RP` | `1` | Min balance before payment |
| `OPAI_GOPAY_ACCOUNT_TTL_SEC` | `1200` | Account cleanup TTL |
| `OPAI_GOPAY_REGISTER_PROXY` | (none) | Override proxy for registration |
| `OPAI_GOPAY_PROXY_TEMPLATE` | (none) | Proxy URL template with `{sid}` placeholder |
| `OPAI_GOPAY_ACCOUNTS_FILE` | `config/gopay_worker_accounts.json` | Local account store |

## Architecture

```
worker thread N
  |
  +--> _register_one()
  |      rent phone (hero-sms) -> signup (gojek API) -> refresh -> GoPay init -> PIN setup
  |      uses: gojek_client.py + gopay_signer_v2.py (HMAC-SHA256 V2 signing)
  |
  +--> wait balance >= 1 Rp (poll gopay API, keep phone alive via reactivate)
  |
  +--> _claim_job() from Payment Inbox
  |
  +--> _pay_job()
  |      uses: gopay_payment_protocol.py (Midtrans snap linking + charge + PIN challenge)
  |
  +--> loop back to register
```

## Protocol Modules

| Module | Purpose |
|---|---|
| `gopay_signer_v2.py` | HMAC-SHA256 V2 request signing (Frida-verified) |
| `gojek_client.py` | Complete Gojek/GoPay API client (signup, login, PIN, wallet, envelope) |
| `gopay_payment_protocol.py` | Midtrans GoPay payment (linking + charge + challenge, 14 steps) |
| `gopay_protocol_worker.py` | Multi-threaded worker orchestrating register + pay |
| `sms_helpers.py` | Hero-SMS API utilities (rent, wait OTP, cancel) |
| `envelope_manager.py` | GoPay red envelope link manager |
| `payment_inbox.py` | Payment Inbox HTTP client + SQLite server |

## Dependencies

- Python 3.11+
- `tls_client` (TLS fingerprint spoofing for Gojek API)
