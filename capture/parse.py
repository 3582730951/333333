#!/usr/bin/env python3
"""Parse a captured pcap (+ TLS keylog) into a ground-truth fingerprint manifest.

Records, per upstream host: the TLS ClientHello fingerprint (JA3 + raw component
lists, enough to rebuild a uTLS spec or a curl_cffi ja3 string), the negotiated
HTTP version, and the exact request — header names/values in wire order plus the
body. Handles BOTH HTTP/1.1 (Claude Code → api.anthropic.com offers ALPN http/1.1)
and HTTP/2.
"""
import json
import subprocess
import sys
import hashlib

AGG = "\x1f"


def tshark(args, pcap=None):
    base = ["tshark", "-r", pcap or PCAP, "-o", f"tls.keylog_file:{KEYS}", "-Q",
            "-E", "separator=\t", "-E", f"aggregator={AGG}", "-E", "occurrence=a"]
    return subprocess.run(base + args, capture_output=True, text=True)


def fields(display_filter, fs, pcap=None):
    args = ["-Y", display_filter, "-T", "fields"]
    for f in fs:
        args += ["-e", f]
    out = tshark(args, pcap=pcap)
    rows = []
    for line in out.stdout.splitlines():
        if not line.strip():
            continue
        parts = line.split("\t")
        parts += [""] * (len(fs) - len(parts))
        rows.append([p.split(AGG) if p != "" else [] for p in parts])
    return rows


def hx2dec(h):
    h = h.strip()
    if h == "":
        return None
    try:
        return int(h, 16) if h.lower().startswith("0x") else int(h)
    except ValueError:
        return None


# GREASE values per RFC 8701 — excluded from JA3 (real Node/rustls have none, but
# be safe so the fingerprint is canonical).
GREASE = {0x0a0a, 0x1a1a, 0x2a2a, 0x3a3a, 0x4a4a, 0x5a5a, 0x6a6a, 0x7a7a,
          0x8a8a, 0x9a9a, 0xabab, 0xbaba, 0xcaca, 0xdada, 0xeaea, 0xfafa}


def ja3_of(version_dec, ciphers, exts, groups, ecpf):
    def clean(xs):
        return [x for x in xs if x is not None and x not in GREASE]
    s = "{},{},{},{},{}".format(
        version_dec or "",
        "-".join(str(x) for x in clean(ciphers)),
        "-".join(str(x) for x in clean(exts)),
        "-".join(str(x) for x in clean(groups)),
        "-".join(str(x) for x in clean(ecpf)),
    )
    return s, hashlib.md5(s.encode()).hexdigest()


def collect_tls(pcap=None):
    rows = fields("tls.handshake.type==1", [
        "tls.handshake.extensions_server_name",
        "tls.handshake.version",
        "tls.handshake.ciphersuite",
        "tls.handshake.extension.type",
        "tls.handshake.extensions_supported_group",
        "tls.handshake.extensions_ec_point_format",
        "tls.handshake.extensions_alpn_str",
        "tls.handshake.sig_hash_alg",
        "tls.handshake.extensions.supported_version",
    ], pcap=pcap)
    out = {}
    for r in rows:
        sni = (r[0] or [""])[0]
        if not sni:
            continue
        version = hx2dec((r[1] or ["0"])[0])
        ciphers = [hx2dec(x) for x in r[2]]
        exts = [hx2dec(x) for x in r[3]]
        groups = [hx2dec(x) for x in r[4]]
        ecpf = [hx2dec(x) for x in r[5]]
        alpn = r[6]
        sigalgs = [hx2dec(x) for x in r[7]]
        supver = [hx2dec(x) for x in r[8]]
        ja3, ja3h = ja3_of(version, ciphers, exts, groups, ecpf)
        if sni in out:
            continue  # first ClientHello per host is enough
        out[sni] = {
            "sni": sni, "ja3": ja3, "ja3_hash": ja3h,
            "tls_legacy_version": version,
            "cipher_suites": [x for x in ciphers if x not in GREASE],
            "cipher_suites_hex": ["0x%04x" % x for x in ciphers if x not in GREASE],
            "extensions": [x for x in exts if x not in GREASE],
            "supported_groups": [x for x in groups if x not in GREASE],
            "ec_point_formats": ecpf,
            "alpn": alpn,
            "signature_algorithms": ["0x%04x" % x for x in sigalgs if x is not None],
            "supported_versions": ["0x%04x" % x for x in supver if x is not None and x not in GREASE],
        }
    return out


def collect_h1():
    """HTTP/1.1 requests: header lines in wire order + body."""
    rows = fields("http.request", [
        "http.host", "http.request.method", "http.request.uri",
        "http.request.line", "http.file_data", "http.content_type"])
    out = {}
    for r in rows:
        host = (r[0] or [""])[0]
        method = (r[1] or [""])[0]
        uri = (r[2] or [""])[0]
        lines = r[3]
        body_hex = (r[4] or [""])[0]
        headers = []
        for ln in lines:
            ln = ln.rstrip("\r\n")
            if ":" in ln:
                k, v = ln.split(":", 1)
                headers.append([k.strip(), v.strip()])
        body = None
        if body_hex:
            try:
                body = bytes.fromhex(body_hex.replace(":", "")).decode("utf-8", "replace")
            except ValueError:
                body = None
        bj = None
        if body:
            try:
                bj = json.loads(body)
            except Exception:
                pass
        out.setdefault(host, []).append({
            "http_version": "1.1", "method": method, "uri": uri,
            "headers": headers,
            "body_json": bj, "body_text": None if bj else (body[:3000] if body else None),
        })
    return out


def collect_h2():
    rows = fields('http2.type==1 && http2.header.name==":method"', [
        "tcp.stream", "http2.header.name", "http2.header.value"])
    # map stream→authority via the headers themselves
    out = {}
    for r in rows:
        names, vals = r[1], r[2]
        headers = [[names[i], vals[i] if i < len(vals) else ""] for i in range(len(names))]
        host = next((v for n, v in headers if n == ":authority"), "")
        method = next((v for n, v in headers if n == ":method"), "")
        path = next((v for n, v in headers if n == ":path"), "")
        pseudo = [n for n, _ in headers if n.startswith(":")]
        out.setdefault(host, []).append({
            "http_version": "2", "method": method, "uri": path,
            "pseudo_header_order": pseudo, "headers": headers,
            "body_json": None, "body_text": None,
        })
    return out


def main():
    manifest = {"targets": {}}
    tls = collect_tls()
    h1 = collect_h1()
    h2 = collect_h2()
    hosts = set(tls) | set(h1) | set(h2)
    for host in sorted(hosts):
        t = {"tls": tls.get(host), "requests": []}
        t["requests"].extend(h1.get(host, []))
        t["requests"].extend(h2.get(host, []))
        if t["requests"]:
            t["http_version"] = t["requests"][0]["http_version"]
        manifest["targets"][host] = t

    # Merge the local-recorder requests (the authoritative decrypted request shape
    # for clients we can't passively decrypt, e.g. the compiled Claude Code binary)
    # plus the mitmproxy-captured HTTP requests and WebSocket handshake/frames (the
    # only way to see the Rust codex_cli_rs WS traffic). Keyed by label → canonical
    # upstream host so each lands next to the JA3.
    label_host = {"anthropic": "api.anthropic.com", "openai": "api.openai.com",
                  "chatgpt": "chatgpt.com"}
    if REQFILE:
        try:
            lines = open(REQFILE).read().splitlines()
        except OSError:
            lines = []
        for ln in lines:
            if not ln.strip():
                continue
            rec = json.loads(ln)
            host = label_host.get(rec.get("label"), rec.get("label", "unknown"))
            t = manifest["targets"].setdefault(host, {"tls": tls.get(host), "requests": []})
            kind = rec.get("kind", "http")
            if kind == "ws_handshake":
                t.setdefault("websocket_handshakes", []).append({
                    "request_line": rec.get("request_line"),
                    "method": rec.get("method"),
                    "path": rec.get("path"),
                    "headers": rec.get("headers", []),
                })
            elif kind == "ws_msg":
                t.setdefault("websocket_messages", []).append({
                    "path": rec.get("path"),
                    "direction": rec.get("direction"),
                    "opcode": rec.get("opcode"),
                    "text": rec.get("text"),
                })
            else:
                t.setdefault("recorded_requests", []).append({
                    "request_line": rec.get("request_line"),
                    "method": rec.get("method"),
                    "path": rec.get("path"),
                    "headers": rec.get("headers", []),
                    "body_json": rec.get("body_json"),
                    "body_text": rec.get("body_text"),
                })

    # The client→proxy ClientHello on the mitm hop is the client's REAL handshake
    # (SNI = the real target), so its JA3 is authentic even though the bytes were
    # proxied. Record it as a cross-check next to the direct-hop JA3.
    if MPCAP:
        for host, hop in collect_tls(pcap=MPCAP).items():
            t = manifest["targets"].setdefault(host, {"tls": None, "requests": []})
            t["tls_client_hop"] = hop
            if not t.get("tls"):
                t["tls"] = hop

    with open(OUT, "w") as f:
        json.dump(manifest, f, indent=2)

    for host in sorted(manifest["targets"]):
        t = manifest["targets"][host]
        tl = t.get("tls") or {}
        print(f"  [{host}] http/{t.get('http_version','?')} reqs={len(t.get('requests', []))} "
              f"recorded={len(t.get('recorded_requests', []))} "
              f"ws_hs={len(t.get('websocket_handshakes', []))} ws_msg={len(t.get('websocket_messages', []))} "
              f"ja3={tl.get('ja3_hash','-')} alpn={tl.get('alpn','-')}")
    print(f"  manifest → {OUT}")


if __name__ == "__main__":
    PCAP, KEYS, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
    REQFILE = sys.argv[4] if len(sys.argv) > 4 else None
    MPCAP = sys.argv[5] if len(sys.argv) > 5 else None
    main()
