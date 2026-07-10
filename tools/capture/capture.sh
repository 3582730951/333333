#!/usr/bin/env bash
# Orchestrates one capture run: start passive sniffing, drive the real Claude Code
# and Codex CLIs with throwaway credentials (the request is transmitted in full and
# carries the complete fingerprint even though it 401s), stop sniffing, parse.
set -u

OUT="${CAPTURE_OUT:-/cap/out}"
mkdir -p "$OUT"
PCAP="$OUT/cap.pcap"
KEYS="$OUT/keys.log"
: > "$KEYS"

echo "[*] clients:"
echo "    claude $(claude --version 2>&1 | head -1)"
echo "    codex  $(codex --version 2>&1 | head -1)"
echo "    node   $(node --version)"

# Passive capture on all interfaces, TLS only.
tcpdump -i any -w "$PCAP" 'tcp port 443' >/dev/null 2>&1 &
TPID=$!
sleep 1.5

echo "[*] driving Claude Code (throwaway key, expect 401)…"
SSLKEYLOGFILE="$KEYS" \
NODE_OPTIONS="--require /cap/keylog.js" \
ANTHROPIC_API_KEY="sk-ant-api03-capture-dummy-0000000000000000000000000000000000000000000000000000000000000000AA" \
  timeout 45 claude -p "say hi" --output-format text \
  >"$OUT/claude.stdout" 2>"$OUT/claude.stderr" || true

echo "[*] driving Codex (throwaway key, expect 401)…"
SSLKEYLOGFILE="$KEYS" \
NODE_OPTIONS="--require /cap/keylog.js" \
OPENAI_API_KEY="sk-capture-dummy-000000000000000000000000000000000000000000000000" \
  timeout 45 codex exec --skip-git-repo-check "say hi" \
  >"$OUT/codex.stdout" 2>"$OUT/codex.stderr" || true

sleep 1.5
kill "$TPID" 2>/dev/null
wait "$TPID" 2>/dev/null

echo "[*] capture sizes:"
ls -la "$PCAP" "$KEYS" 2>&1 | sed 's/^/    /'
echo "[*] keylog lines: $(wc -l < "$KEYS" 2>/dev/null || echo 0)"

# Phase B: point the clients at a local plaintext recorder so we capture the exact
# request (headers in order + body) regardless of how the client is packaged
# (Claude Code is a compiled Bun binary that ignores NODE_OPTIONS, so passive TLS
# keylog can't decrypt it — but it honors ANTHROPIC_BASE_URL). The client builds
# the same request whatever the destination, so this is faithful.
REQS="$OUT/requests.jsonl"
: > "$REQS"
python3 /cap/recorder.py 9100 "$REQS" anthropic >/dev/null 2>&1 &
RP1=$!
python3 /cap/recorder.py 9101 "$REQS" openai >/dev/null 2>&1 &
RP2=$!
sleep 1

echo "[*] recording Claude Code request via ANTHROPIC_BASE_URL…"
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
ANTHROPIC_BASE_URL="http://127.0.0.1:9100" \
ANTHROPIC_API_KEY="sk-ant-api03-capture-dummy-0000000000000000000000000000000000000000000000000000000000000000AA" \
  timeout 45 claude -p "say hi" --model claude-opus-4-8 --output-format text \
  >"$OUT/claude2.stdout" 2>"$OUT/claude2.stderr" || true

echo "[*] recording Claude Code third-party relay auth via ANTHROPIC_AUTH_TOKEN…"
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
ANTHROPIC_BASE_URL="http://127.0.0.1:9100" \
ANTHROPIC_AUTH_TOKEN="sk-ant-oat-capture-third-party-token" \
  timeout 45 claude -p "say hi" --model claude-opus-4-8 --output-format text \
  >"$OUT/claude_auth_token.stdout" 2>"$OUT/claude_auth_token.stderr" || true

echo "[*] recording Codex request via OPENAI_BASE_URL…"
OPENAI_BASE_URL="http://127.0.0.1:9101/v1" \
OPENAI_API_BASE="http://127.0.0.1:9101/v1" \
OPENAI_API_KEY="sk-capture-dummy-000000000000000000000000000000000000000000000000" \
  timeout 30 codex exec --skip-git-repo-check "say hi" \
  >"$OUT/codex2.stdout" 2>"$OUT/codex2.stderr" || true

sleep 1
kill "$RP1" "$RP2" 2>/dev/null
echo "[*] recorded requests: $(wc -l < "$REQS" 2>/dev/null || echo 0)"

# Phase C: intercepting proxy. The Rust codex_cli_rs binary ignores SSLKEYLOGFILE
# and speaks a WebSocket (wss://api.openai.com/v1/responses) the plain HTTP recorder
# cannot accept, so neither passive keylog nor the recorder captured it. mitmproxy
# terminates TLS with a CA we installed into the trust store, so it decrypts the
# HTTP requests AND the WS upgrade + frames. We sniff the client→proxy hop too: that
# ClientHello is the client's REAL one, so its JA3 is authentic.
CA="/root/.mitmproxy/mitmproxy-ca-cert.pem"
MPCAP="$OUT/cap_mitm.pcap"
export CAP_OUT="$REQS"
tcpdump -i lo -w "$MPCAP" 'tcp port 8080' >/dev/null 2>&1 &
MTPID=$!
mitmdump -q --listen-host 127.0.0.1 --listen-port 8080 \
  --set stream_large_bodies=10m --ssl-insecure \
  -s /cap/mitm_addon.py >"$OUT/mitm.log" 2>&1 &
MPID=$!
sleep 2.5

proxy_env() {
  export HTTP_PROXY="http://127.0.0.1:8080" HTTPS_PROXY="http://127.0.0.1:8080" \
         ALL_PROXY="http://127.0.0.1:8080" \
         SSL_CERT_FILE="$CA" REQUESTS_CA_BUNDLE="$CA" NODE_EXTRA_CA_CERTS="$CA"
}

echo "[*] capturing Claude Code through mitmproxy…"
( proxy_env
  ANTHROPIC_API_KEY="sk-ant-api03-capture-dummy-0000000000000000000000000000000000000000000000000000000000000000AA" \
    timeout 45 claude -p "say hi" --output-format text \
    >"$OUT/claude3.stdout" 2>"$OUT/claude3.stderr" || true )

echo "[*] capturing Codex (WebSocket) through mitmproxy…"
( proxy_env
  OPENAI_API_KEY="sk-capture-dummy-000000000000000000000000000000000000000000000000" \
    timeout 45 codex exec --skip-git-repo-check "say hi" \
    >"$OUT/codex3.stdout" 2>"$OUT/codex3.stderr" || true )

sleep 1.5
kill "$MPID" "$MTPID" 2>/dev/null
wait "$MTPID" 2>/dev/null
echo "[*] mitmproxy records: ws_handshake=$(grep -c ws_handshake "$REQS" 2>/dev/null || echo 0) ws_msg=$(grep -c ws_msg "$REQS" 2>/dev/null || echo 0)"

echo "[*] parsing → manifest.json"
python3 /cap/parse.py "$PCAP" "$KEYS" "$OUT/manifest.json" "$REQS" "$MPCAP" || echo "[!] parse failed"
echo "[*] done."
