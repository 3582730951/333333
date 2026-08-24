#!/usr/bin/env bash
# claude_fetch_fingerprint.sh — deployment-side Claude Code fingerprint auto-fetch.
#
# Runs the OFFICIAL Claude Code linux-x64 binaries against a local capture server and
# extracts the wire fingerprints the gateway advertises upstream (the claudeCLIFingerprints
# library in internal/config/config.go). It is the task-#12 "deployment VPS fetch": safe to
# run at gateway startup and/or on a low-occupancy systemd timer / cron every N hours.
#
# What it observes per version (from the real binary's own wire):
#   - claude-cli version + entrypoint (User-Agent) + X-Stainless-* versions
#   - the anthropic-beta list for a model (model- AND entitlement-dependent)
#   - the billing block's cc_version VERBATIM and whether it carries the message-derived
#     attribution suffix (cc_version=<v>.<3-hex>) or a cch field — the §8.4 remote-config
#     rollout signal. The billing block IS sent even with a fake token (verified
#     2026-08-24), so the suffix/plain state is observable in the default mode; a REAL
#     ENTITLED TOKEN (CLAUDE_FETCH_REAL_TOKEN) additionally confirms the real-account
#     billing path and the entitlement-gated betas (e.g. context-1m on a native-1M model).
#
# Both modes set the telemetry-suppression env vars (§7.4) so the fetch host never leaks
# telemetry of its own (no metrics_enabled probe, no 1P events).
#
# Output: data/claude-captures/fingerprint-report.json — one entry per version plus a
# diff-vs-builtin block and "attribution": "live" | "plain" | "unknown" compared against
# CLAUDE_FETCH_EXPECTED_ATTRIBUTION (default "live" — the pool default and the verified
# 2026-08-24 state). With CODEX_POOL_COMPAT_MANIFEST_KEY set, it ALSO writes a signed
# compatmanifest payload (claude.cli/node/stainless + attribution_suffix: live|plain)
# ready for the gateway's `signed_custom` compatibility-manifest source.
#
# Exit: 0 = clean (report written, library current) · 1 = drift detected (report written)
#       2 = tooling error (report may not exist).

set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA="${CLAUDE_CAPTURE_DATA_DIR:-${ROOT}/data/claude-captures}"
BIN_DIR="${DATA}/bin"
REPORT="${DATA}/fingerprint-report.json"
MANIFEST_OUT="${DATA}/compatibility-manifest.json"
CAPTURE_LOG="${DATA}/captured_requests.log"
CAPTURE_BODY="${DATA}/captured_requests.log.body"
CAPTURE_PORT="${CLAUDE_CAPTURE_PORT:-18999}"
mkdir -p "$DATA" "$BIN_DIR"

WINDOW="${CLAUDE_FETCH_WINDOW:-6}"                 # latest + (WINDOW-1) prior
FAKE_TOKEN="${CLAUDE_FETCH_FAKE_TOKEN:-cap_faketoken123}"
REAL_TOKEN="${CLAUDE_FETCH_REAL_TOKEN:-}"          # entitle-gated billing/context-1m capture
SIGNING_KEY="${CODEX_POOL_COMPAT_MANIFEST_KEY:-}"  # PEM Ed25519 private key for signed_custom
MODEL="${CLAUDE_FETCH_MODEL:-claude-opus-5}"
RUN_TIMEOUT="${CLAUDE_FETCH_RUN_TIMEOUT:-90}"      # per-binary run bound; the 401 lands in ~2s
# What the pool is currently configured to emit. "live" (the pool default, and the
# verified 2026-08-24 state) means cc_version SHOULD carry the .xxx suffix; a capture
# showing a different state is the actionable drift the operator acts on.
EXPECTED_ATTRIBUTION="${CLAUDE_FETCH_EXPECTED_ATTRIBUTION:-live}"

log() { printf '[claude-fetch] %s\n' "$*" >&2; }

command -v curl >/dev/null || { log "curl required"; exit 2; }
command -v python3 >/dev/null || { log "python3 required"; exit 2; }
if [[ -n "$SIGNING_KEY" ]] && ! command -v openssl >/dev/null; then
  log "openssl required for manifest signing"; exit 2
fi

# --- local capture server (headers + body; same recorder the library was captured with) ---
start_capture() {
  : >"$CAPTURE_LOG" ; : >"$CAPTURE_BODY"
  python3 - "$CAPTURE_LOG" "$CAPTURE_BODY" "$CAPTURE_PORT" <<'PY' &
import http.server, socketserver, sys, json, time, os
LOG, BODY, PORT = sys.argv[1], sys.argv[2], int(sys.argv[3])
class H(http.server.BaseHTTPRequestHandler):
    def _log(self):
        with open(LOG, "a") as f:
            f.write(json.dumps({"time": time.time(), "method": self.command, "path": self.path,
                                "headers": {k: v for k, v in self.headers.items()}})+"\n")
    def do_GET(self): self._log(); self.send_response(401); self.end_headers()
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n) if n else b""
        with open(BODY, "ab") as f:
            f.write(json.dumps({"path": self.path, "body": body.decode("utf-8", "replace")}).encode()+b"\n")
        self._log(); self.send_response(401); self.end_headers()
    def log_message(self, *a): pass
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as d: d.serve_forever()
PY
  CAP_PID=$!
  sleep 1.2
}
stop_capture() { kill "$CAP_PID" 2>/dev/null || true; wait "$CAP_PID" 2>/dev/null || true; }
trap stop_capture EXIT

# --- version window from npm -----------------------------------------------------------
log "resolving Claude Code window (latest + $((WINDOW-1))) from npm"
mapfile -t VERSIONS < <(curl -sf --max-time 20 "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64" \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)
vs = sorted((v for v in d['versions'] if '-' not in v),
            key=lambda s: [int(x) for x in s.split('.')])
for v in vs[-int('$WINDOW'):]: print(v)
" || true)
[[ ${#VERSIONS[@]} -gt 0 ]] || { log "could not resolve versions"; exit 2; }
log "window: ${VERSIONS[*]}"

# --- fetch + run one version -----------------------------------------------------------
run_one() { # run_one <version> <token>  → one /v1/messages request through the capture server
  local v="$1" token="$2" bin="$BIN_DIR/claude-$v" tgz="$BIN_DIR/claude-$v.tgz"
  # Truncate before each run so extract() sees ONLY this version's capture (the log
  # otherwise accumulates every prior version and the first-request read skews).
  : >"$CAPTURE_LOG" ; : >"$CAPTURE_BODY"
  if [[ ! -x "$bin" ]]; then
    [[ -f "$tgz" ]] || { log "download $v"; curl -sfL --max-time 180 \
      "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-x64/-/claude-code-linux-x64-$v.tgz" \
      -o "$tgz" || { log "download failed $v"; return 1; }; }
    rm -rf "$BIN_DIR/pkg-$v"; mkdir -p "$BIN_DIR/pkg-$v"
    tar xzf "$tgz" -C "$BIN_DIR/pkg-$v"
    if [[ -f "$BIN_DIR/pkg-$v/package/bin/claude" ]]; then mv "$BIN_DIR/pkg-$v/package/bin/claude" "$bin";
    else find "$BIN_DIR/pkg-$v" -type f -name claude -exec mv {} "$bin" \;; fi
    chmod +x "$bin"
  fi
  printf 'hello\n' | ANTHROPIC_BASE_URL="http://127.0.0.1:$CAPTURE_PORT" \
    ANTHROPIC_AUTH_TOKEN="$token" ANTHROPIC_MODEL="$MODEL" \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_TELEMETRY=1 \
    timeout "$RUN_TIMEOUT" "$bin" -p --max-turns 1 >/dev/null 2>&1 || true
}

# --- extraction --------------------------------------------------------------------------
extract() { # extract <real:0|1>  → JSON per version: tuple + beta + billing + attribution
  python3 - "$CAPTURE_LOG" "$CAPTURE_BODY" "$MODEL" <<'PY'
import sys, json, re
LOG, BODY, MODEL = sys.argv[1], sys.argv[2], sys.argv[3]
rows = []
for f in (LOG,):
    for line in open(f):
        line = line.strip()
        if line: rows.append(json.loads(line))
msg = next((r for r in rows if r["path"].startswith("/v1/messages")), None)
entry = {"error": "no /v1/messages request captured"}
if msg:
    h = msg["headers"]
    m = re.search(r"claude-cli/([0-9.]+) \(external, (cli|sdk-cli)\)", h.get("User-Agent", ""))
    entry = {
        "ua": h.get("User-Agent", ""),
        "cli_version": m.group(1) if m else "",
        "entrypoint": m.group(2) if m else "",
        "stainless_package": h.get("X-Stainless-Package-Version", ""),
        "stainless_runtime": h.get("X-Stainless-Runtime-Version", ""),
        "beta": h.get("anthropic-beta", ""),
    }
# billing block lives in the request body system prompt (x-anthropic-billing-header:)
entry["billing"] = ""; entry["attribution"] = "unknown"
try:
    with open(BODY) as f:
        for line in f:
            line = line.strip()
            if not line: continue
            r = json.loads(line)
            if not r["path"].startswith("/v1/messages"): continue
            body = r.get("body", "")
            for mm in re.finditer(r"x-anthropic-billing-header:\s*([^\"\\]*)", body):
                blk = mm.group(1).strip()
                entry["billing"] = blk
                mver = re.search(r"cc_version=([0-9]+\.[0-9]+\.[0-9]+)(\.([0-9a-f]{3}))?", blk)
                if mver and mver.group(2):
                    entry["attribution"] = "live"
                elif mver and "cch=" in blk:
                    entry["attribution"] = "live"
                elif "cc_version=" in blk:
                    entry["attribution"] = "plain"
                break
except FileNotFoundError:
    pass
print(json.dumps(entry))
PY
}

# --- main ---------------------------------------------------------------------------------
start_capture
RESULTS=()
DRIFT=0
for v in "${VERSIONS[@]}"; do
  log "capture $v (fake token)"
  run_one "$v" "$FAKE_TOKEN" || true
  RESULTS+=("$v|$(extract 0)")
  if [[ -n "$REAL_TOKEN" ]]; then
    log "capture $v (real token: billing block + entitlement betas)"
    run_one "$v" "$REAL_TOKEN" || true
    RESULTS+=("$v|$(extract 1)")
  fi
done

python3 - "$REPORT" "$(printf '%s\n' "${RESULTS[@]}")" "$REAL_TOKEN" "$EXPECTED_ATTRIBUTION" <<'PY'
import sys, json, re, os
REPORT, data, has_real, EXPECTED = sys.argv[1], sys.argv[2], bool(sys.argv[3]), sys.argv[4]
# built-in reference (internal/config/config.go claudeCLIFingerprints, kept in sync)
builtin = {
    "2.1.241": ("0.112.1", "v26.3.0"), "2.1.240": ("0.112.1", "v26.3.0"),
    "2.1.239": ("0.112.1", "v26.3.0"), "2.1.238": ("0.112.1", "v26.3.0"),
    "2.1.237": ("0.112.1", "v26.3.0"), "2.1.236": ("0.112.1", "v26.3.0"),
    "2.1.226": ("0.94.0", "v26.3.0"),
}
entries = {}
for line in data.splitlines():
    if not line: continue
    v, rest = line.split("|", 1)
    entries.setdefault(v, []).append(json.loads(rest))
report, changes = {}, []
for v, snaps in entries.items():
    fake = snaps[0]
    e = {"cli_version": fake.get("cli_version"), "stainless_package": fake.get("stainless_package"),
         "stainless_runtime": fake.get("stainless_runtime"), "beta": fake.get("beta"),
         "billing": fake.get("billing", ""), "attribution": fake.get("attribution", "unknown")}
    if has_real and len(snaps) > 1:
        real = snaps[1]
        e["billing"] = real.get("billing", e["billing"])
        if real.get("attribution") != "unknown":
            e["attribution"] = real.get("attribution")
        if real.get("beta"):
            e["beta_real_token"] = real["beta"]
    report[v] = e
    exp = builtin.get(v)
    if exp and (e["stainless_package"] or e["stainless_runtime"]) and \
       (e["stainless_package"] != exp[0] or e["stainless_runtime"] != exp[1]):
        changes.append({"version": v, "kind": "tuple", "got": (e["stainless_package"], e["stainless_runtime"]), "want": exp})
    # attribution state drift: only actionable when the wire disagrees with the
    # pool's configured state AND we actually saw the billing block (not "unknown").
    if e["attribution"] != "unknown" and e["attribution"] != EXPECTED:
        changes.append({"version": v, "kind": "attribution_state", "got": e["attribution"], "want": EXPECTED, "billing": e["billing"]})
    if not exp:
        changes.append({"version": v, "kind": "new_version"})
states = [e["attribution"] for e in report.values()]
if any(s == "live" for s in states): attr = "live"
elif any(s == "plain" for s in states): attr = "plain"
else: attr = "unknown"
out = {"captured_at": int(__import__("time").time()), "window": sorted(report),
       "real_token_capture": bool(has_real), "expected_attribution": EXPECTED, "attribution": attr,
       "versions": report, "diff_vs_builtin": changes}
os.makedirs(os.path.dirname(REPORT), exist_ok=True)
with open(REPORT, "w") as f:
    json.dump(out, f, indent=2, sort_keys=True)
print(json.dumps({"drift": len(changes), "attribution": attr}))
PY
DRIFT_FLAG=$(python3 -c "import json;d=json.load(open('$REPORT'));print(len(d['diff_vs_builtin']),d['attribution'])" 2>/dev/null || echo "0 unknown")
DRIFT=$(echo "$DRIFT_FLAG" | awk '{print $1}')
ATTR=$(echo "$DRIFT_FLAG" | awk '{print $2}')
log "report: $REPORT (drift=$DRIFT attribution=$ATTR)"
if [[ -n "$SIGNING_KEY" && "$ATTR" == "live" ]]; then
  log "signing compatmanifest payload (attribution live)"
  python3 - "$MANIFEST_OUT" "$REPORT" <<'PY'
import sys, json, time
OUT, REPORT = sys.argv[1], sys.argv[2]
r = json.load(open(REPORT))
vs = sorted(r["versions"])
v = vs[-1]
p = {"schema_version": 1, "generation": int(time.time()), "issued_at": int(time.time()),
     "expires_at": int(time.time()) + 14*86400, "source": "signed_custom",
     "claude": {"cli_version": v,
                "node_version": r["versions"][v].get("stainless_runtime", ""),
                "stainless_version": r["versions"][v].get("stainless_package", ""),
                "attribution_suffix": "live"}}
json.dump(p, open(OUT, "w"), indent=2)
print("wrote", OUT)
PY
  SIG_BIN="$DATA/payload.sig"
  openssl pkeyutl -sign -inkey "$SIGNING_KEY" -rawin -in "$MANIFEST_OUT" -out "$SIG_BIN"
  python3 - "$MANIFEST_OUT" "$SIG_BIN" <<'PY'
import sys, json, base64
OUT, SIG = sys.argv[1], sys.argv[2]
env = {"payload": json.loads(open(OUT).read()), "signature": base64.b64encode(open(SIG, "rb").read()).decode()}
# envelope = {"payload": <raw json string>, "signature": base64}
json.dump({"payload": json.dumps(env["payload"]), "signature": env["signature"]}, open(OUT, "w"), indent=2)
print("signed manifest:", OUT)
PY
fi

# 1 = drift was found (tuple drift, a new version, or the attribution state changed
# vs the pool's configured state). "live" with the default EXPECTED_ATTRIBUTION=live
# is the normal, current state and is NOT a drift. Operator should review/re-sign.
if [[ "${DRIFT:-0}" != "0" ]]; then exit 1; fi
exit 0
