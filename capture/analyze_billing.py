#!/usr/bin/env python3
"""Analyze the Claude Code x-anthropic-billing-header from captured traffic.

Run real Claude Code through the existing MITM capture (see capture.sh / mitm_addon.py)
so out_v2/requests.jsonl contains genuine api.anthropic.com /v1/messages requests, then:

    python3 analyze_billing.py [requests.jsonl ...]

For each request that carries an `x-anthropic-billing-header:` system block it extracts
`cc_version=<ver>.<buildHash>; cc_entrypoint=<ep>; cch=<cch>;` and then:

  1. tabulates buildHash per (version, entrypoint, identity-line) — flagging whether
     buildHash is truly constant per version (our assumption in cloak.claudeBuildHashByVersion);
  2. brute-forces the buildHash FORMULA over (messageText, version) across hash funcs,
     salts, and char-index tuples — needs ≥2 DISTINCT versions to be meaningful;
  3. brute-forces the per-request cch over candidate content slices × hashes ×
     JSON serializations × head/tail, and detects "identical body, different cch"
     (⇒ cch carries a nonce and is not a pure content hash);
  4. prints Go lines to paste into cloak.claudeBuildHashByVersion.

Findings (live capture of real claude-cli 2.1.160 via mock_relay.py, out_mock):
  - buildHash (the 3-hex cc_version suffix) AND cch (5-hex) are BOTH per-request content
    fingerprints: they change every request ("say hi"->cc_version 2.1.160.f01/cch c511f,
    "what is 2+2"->.268/8c02b, ...). out_v2 only LOOKED version-constant because capture.sh
    always sends "say hi". So a version->buildHash table is WRONG; they are not reproducible
    from the wire (obfuscated client-side; other_cpa's salted-index formula is for an older
    build and does not match). pool_server therefore emits fresh random hex of the right shape.
  - cc_entrypoint tracks the UA suffix: sdk-cli for `claude -p`, cli for interactive.
  - metadata.user_id is a JSON STRING: {"device_id":"<64hex>","account_uuid":"","session_id":"<uuid>"}.
Re-run with more samples / RAW bytes (mitm_addon now records body_b64) to attempt a crack;
this tool brute-forces formulas and flags the per-request/nonce nature.
"""
import sys, os, json, re, hashlib, itertools, copy, base64

BILLING_RE = re.compile(r'cc_version=([0-9.]+)\.([0-9a-f]{3});\s*cc_entrypoint=([^;]+);\s*cch=([0-9a-f]+);')
HASHES = {"sha256": hashlib.sha256, "sha1": hashlib.sha1, "md5": hashlib.md5}
SALTS = ["", "59cf53e54c78"]  # other_cpa's salt + none


def load(paths):
    rows = []
    for p in paths:
        if not os.path.exists(p):
            print(f"  (skip, not found: {p})", file=sys.stderr); continue
        with open(p) as f:
            for line in f:
                line = line.strip()
                if line:
                    try: rows.append(json.loads(line))
                    except Exception: pass
    return rows


def first_text(sysv, skip_billing=True):
    if isinstance(sysv, str):
        return sysv
    if isinstance(sysv, list):
        for b in sysv:
            if isinstance(b, dict) and b.get("type") == "text":
                t = b.get("text", "")
                if skip_billing and t.lstrip().startswith("x-anthropic-billing-header:"):
                    continue
                return t
    return ""


def samples_from(rows):
    out = []
    for r in rows:
        body = r.get("body_json") or {}
        sysv = body.get("system")
        text0 = ""
        if isinstance(sysv, list) and sysv and isinstance(sysv[0], dict):
            text0 = sysv[0].get("text", "")
        elif isinstance(sysv, str):
            text0 = sysv
        m = BILLING_RE.search(text0)
        if not m:
            continue
        rec = {
            "ver": m.group(1), "buildhash": m.group(2), "entrypoint": m.group(3), "cch": m.group(4),
            "msg": first_text(sysv), "body": body,
        }
        b64 = r.get("body_b64")
        if b64:
            try:
                raw_bytes = base64.b64decode(b64)
                rec["raw_bytes"] = raw_bytes
                rec["raw"] = raw_bytes  # hashable key for the nonce check
            except Exception:
                pass
        out.append(rec)
    return out


def crack_buildhash(samples):
    pairs = {}  # (msg, ver) -> buildhash, deduped
    for s in samples:
        pairs[(s["msg"], s["ver"])] = s["buildhash"]
    distinct_versions = {v for (_, v) in pairs}
    print(f"  distinct (msg,version) pairs: {len(pairs)}  distinct versions: {len(distinct_versions)}")
    if len(distinct_versions) < 2:
        print("  -> need ≥2 versions to crack the buildHash formula (capture more).")
        return
    items = list(pairs.items())
    maxlen = min(len(m) for (m, _), _ in items if m) if items else 0
    hits = []
    for hn, hf in HASHES.items():
        for salt in SALTS:
            templates = {
                "salt+ver": lambda m, v: salt + v,
                "salt+msg+ver": lambda m, v: salt + m + v,
                "msg+ver": lambda m, v: m + v,
                "ver+msg": lambda m, v: v + m,
            }
            for tn, tf in templates.items():
                for sl in ("head", "tail"):
                    ok = all((hf(tf(m, v).encode()).hexdigest()[:3] if sl == "head"
                              else hf(tf(m, v).encode()).hexdigest()[-3:]) == bh
                             for (m, v), bh in items)
                    if ok: hits.append((hn, salt, tn, sl))
            # salted char-index triples over messageText
            for combo in itertools.combinations(range(min(maxlen, 48)), 3):
                ok = True
                for (m, v), bh in items:
                    pick = "".join(m[i] for i in combo)
                    if hf((salt + pick + v).encode()).hexdigest()[:3] != bh:
                        ok = False; break
                if ok: hits.append((hn, salt, f"idx{combo}", "head"))
    print(f"  buildHash formula matches: {hits[:8] if hits else 'NONE (formula unknown; rely on the table)'}")


def ser(obj, sep, asc):
    return json.dumps(obj, separators=sep, ensure_ascii=asc)


def cch_candidates(body):
    sys_nb = [b for b in body.get("system", []) if not (isinstance(b, dict) and str(b.get("text", "")).startswith("x-anthropic-billing-header:"))]
    full_nb = copy.deepcopy(body); full_nb["system"] = sys_nb
    return {
        "full_no_billing": full_nb,
        "sys_no_billing": sys_nb,
        "messages": body.get("messages"),
        "sys+messages+tools": {"system": sys_nb, "messages": body.get("messages"), "tools": body.get("tools")},
    }


def crack_cch(samples):
    have_raw = sum(1 for s in samples if s.get("raw"))
    print(f"  samples with exact raw bytes (body_b64): {have_raw}/{len(samples)}"
          + ("" if have_raw == len(samples) else "  <-- recapture with the updated mitm_addon for a byte-exact crack"))
    # nonce check: identical content payload but different cch (prefer exact bytes)
    by_content = {}
    for s in samples:
        key = s["raw"] if s.get("raw") else ser(cch_candidates(s["body"])["full_no_billing"], (",", ":"), False)
        by_content.setdefault(key, set()).add(s["cch"])
    if any(len(v) > 1 for v in by_content.values()):
        print("  -> IDENTICAL body produced DIFFERENT cch: cch carries a nonce/counter, "
              "NOT a pure content hash. A fresh random 5-hex per request is faithful.")
        return
    hits = []
    # If we have exact bytes, also try hashing the raw body directly (with the billing
    # header line stripped / its cch blanked) — the most likely real definition.
    if have_raw == len(samples):
        for hn, hf in HASHES.items():
            for transform, fn in {
                "raw_as_is": lambda b: b,
                "raw_strip_billing_line": lambda b: re.sub(rb'x-anthropic-billing-header:[^"]*', b'', b),
                "raw_cch_blank": lambda b: re.sub(rb'cch=[0-9a-f]+;', b'cch=;', b),
                "raw_cch_zero": lambda b: re.sub(rb'cch=[0-9a-f]+;', b'cch=00000;', b),
            }.items():
                for sl in ("head", "tail"):
                    ok = True
                    for s in samples:
                        d = hf(fn(s["raw_bytes"])).hexdigest()
                        got = d[:5] if sl == "head" else d[-5:]
                        if got != s["cch"]: ok = False; break
                    if ok: hits.append(("raw:" + transform, hn, sl))
    names = list(cch_candidates(samples[0]["body"]).keys())
    for name in names:
        for hn, hf in HASHES.items():
            for sep in [(",", ":"), (", ", ": ")]:
                for asc in (False, True):
                    for sl in ("head", "tail"):
                        ok = True
                        for s in samples:
                            c = cch_candidates(s["body"])[name]
                            if c is None: ok = False; break
                            data = c.encode() if isinstance(c, str) else ser(c, sep, asc).encode()
                            d = hf(data).hexdigest()
                            got = d[:5] if sl == "head" else d[-5:]
                            if got != s["cch"]: ok = False; break
                        if ok: hits.append((name, hn, sep, asc, sl))
    print(f"  cch formula matches: {hits[:6] if hits else 'NONE (try recapturing with raw bytes, or cch has a nonce)'}")


def main():
    paths = sys.argv[1:] or [os.path.join(os.path.dirname(__file__), "out_v2", "requests.jsonl")]
    print(f"reading: {paths}")
    samples = samples_from(load(paths))
    print(f"\nbilling-header requests found: {len(samples)}")
    if not samples:
        print("none — run Claude Code through the MITM capture first (capture.sh).")
        return
    # 1) table view
    table = {}
    for s in samples:
        table.setdefault(s["ver"], set()).add(s["buildhash"])
        print(f"  ver={s['ver']} buildHash={s['buildhash']} entrypoint={s['entrypoint']} cch={s['cch']}")
    print("\n[buildHash per version]")
    for ver, hs in sorted(table.items()):
        flag = "" if len(hs) == 1 else "  <-- NON-CONSTANT (depends on more than version!)"
        print(f"  {ver}: {sorted(hs)}{flag}")
    print("\n[buildHash formula crack]")
    crack_buildhash(samples)
    print("\n[cch crack]")
    crack_cch(samples)
    print("\n[paste into cloak.claudeBuildHashByVersion]")
    for ver, hs in sorted(table.items()):
        if len(hs) == 1:
            print(f'\t"{ver}": "{next(iter(hs))}",')


if __name__ == "__main__":
    main()
