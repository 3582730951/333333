# Project Structure

This repository keeps the existing Go service layout and stable deployment paths while moving diagnostics, tooling, and historical notes out of the repository root.

## Root

- `README.md` is the project entry point.
- `go.mod` and `go.sum` define the Go module `codex-account-pool`.
- `config.example.json` and `config.lifecycle.json` are versioned examples.
- Local runtime files such as `config.local*.json`, `*.sqlite3`, `.run/`, `passwd.txt`, `wsl-list.txt`, and external symlinks stay untracked.

## Runtime Source

- `cmd/` contains Go binaries. `cmd/pool-server` is the main pool server entry point.
- `internal/` contains private Go packages for API routing, upstream clients, storage, reliability, registration, console embedding, and related runtime behavior.
- `web-spa/` contains the Vite admin console source and remains a stable path used by scripts and CI.
- `sidecar/` contains the Python `curl_cffi` sidecar.
- `gopay/plus/` contains the GoPay/Plus integration bundle.
- `services/codex_register/`, `services/chatgpt_register/`, and `services/plus_payment/` remain stable service paths.

## Operations And Deployment

- `scripts/` contains stable operator and CI entry points, including `install.sh`, `selftest.sh`, `ci.sh`, and `warp-exit.sh`.
- `deploy/` contains systemd, nginx, and caddy deployment examples.
- `docs/operations/` contains operator-facing guides and quick-start material.
- `docs/CONSOLE_ASSET_POLICY.md` documents console vendor asset provenance.

## Tools

- `tools/capture/` contains tracked capture harness utilities. Captured output under `capture/out*/` is local-only and ignored.
- `tools/visual/` contains ad hoc visual inspection and screenshot scripts.
- `tools/diagnostics/codex/` contains standalone Go diagnostics with `//go:build ignore`; run them with `go run ./tools/diagnostics/codex/<file>.go`.
- `tools/diagnostics/sidecar/` contains standalone sidecar diagnostics.

## Documentation Archive

- `docs/reports/archive/` contains historical implementation reports, session summaries, fix notes, and analysis documents.
- New long-lived operator docs should go in `docs/operations/`.
- New project reports should go in `docs/reports/` or `docs/reports/archive/` depending on whether they are active or historical.

## Compatibility Contract

Do not rename these paths without updating scripts, deployment docs, and external operator workflows:

- `scripts/install.sh`
- `scripts/selftest.sh`
- `scripts/ci.sh`
- `scripts/warp-exit.sh`
- `web-spa/`
- `sidecar/`
- `gopay/plus/`
- `services/codex_register/`
- `services/chatgpt_register/`
- `services/plus_payment/`
