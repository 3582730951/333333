# Fingerprint capture harness — installs the REAL Claude Code + Codex CLIs and
# captures their exact wire fingerprint. Two complementary methods:
#  1. Passive tcpdump + TLS keylog (tshark) for clients we can observe directly.
#  2. An intercepting mitmproxy with a CA the clients trust — REQUIRED for the Rust
#     `codex_cli_rs` binary, which ignores SSLKEYLOGFILE/NODE_OPTIONS and speaks a
#     WebSocket (wss://api.openai.com/v1/responses) the plain HTTP recorder can't
#     accept. mitmproxy decrypts the HTTP requests AND the WS upgrade + frames; the
#     client→proxy TLS hop is still the client's REAL ClientHello, so tcpdump on
#     that hop yields the genuine JA3.
FROM node:22-bookworm

ENV DEBIAN_FRONTEND=noninteractive
RUN echo "wireshark-common wireshark-common/install-setuid boolean false" | debconf-set-selections \
 && apt-get update \
 && apt-get install -y --no-install-recommends tshark tcpdump ca-certificates jq mitmproxy \
 && rm -rf /var/lib/apt/lists/*

# Generate the mitmproxy CA (first run creates ~/.mitmproxy/*) and install it into
# the system trust store so the clients (and OpenSSL/rustls-native-certs) trust it.
RUN (timeout 5 mitmdump -p 8889 >/dev/null 2>&1 || true) \
 && cp /root/.mitmproxy/mitmproxy-ca-cert.pem /usr/local/share/ca-certificates/mitmproxy.crt \
 && update-ca-certificates

# The real shipping clients (latest). Pinning to @latest means the captured
# fingerprint always reflects what end users actually run.
RUN npm install -g @anthropic-ai/claude-code@latest @openai/codex@latest \
 && (claude --version || true) && (codex --version || true)

WORKDIR /cap
COPY keylog.js capture.sh parse.py recorder.py mitm_addon.py /cap/
RUN chmod +x /cap/capture.sh
ENTRYPOINT ["/cap/capture.sh"]
