FROM node:22.23.1-trixie-slim AS web

WORKDIR /src/web-spa
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates fonts-liberation libasound2 libatk-bridge2.0-0 \
      libatk1.0-0 libatspi2.0-0 libcairo2 libcups2 libdbus-1-3 \
      libgbm1 libglib2.0-0 libgtk-3-0 libnspr4 libnss3 \
      libpango-1.0-0 libpangocairo-1.0-0 libx11-6 libx11-xcb1 \
      libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3 \
      libxkbcommon0 libxrandr2 libxrender1 && \
    rm -rf /var/lib/apt/lists/*
RUN apt-get update && apt-get install -y --no-install-recommends unzip && \
    rm -rf /var/lib/apt/lists/*
COPY web-spa/package.json web-spa/package-lock.json ./
RUN npm ci
COPY web-spa/ ./
COPY docs/CONSOLE_ASSET_POLICY.md /src/docs/CONSOLE_ASSET_POLICY.md
RUN mkdir -p /src/internal/console && npm run verify

FROM node:22.23.1-trixie-slim AS registrar-node

WORKDIR /opt/registrar-node
COPY workers/node-registrar/package.json workers/node-registrar/package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund
COPY workers/node-registrar/index.js workers/node-registrar/sbom.cdx.json ./
RUN node --check index.js && \
    chown -R root:root /opt/registrar-node && \
    chmod -R u=rwX,g=rX,o= /opt/registrar-node

FROM golang:1.25.12-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/console/dist /src/internal/console/dist
RUN CGO_ENABLED=1 go test ./... && \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/codex-pool-server ./cmd/pool-server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/codex-pool-handoff ./cmd/pool-handoff && \
    mkdir -p /out/gateway-bin && \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
      os="${target%/*}"; arch="${target#*/}"; ext=""; \
      if [ "$os" = "windows" ]; then ext=".exe"; fi; \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "/out/gateway-bin/gateway-$os-$arch$ext" ./cmd/gateway; \
    done

FROM debian:13.6-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates chromium curl fonts-liberation python3 python3-venv sqlite3 xvfb && \
    rm -rf /var/lib/apt/lists/*
RUN useradd --system --home /var/lib/codex-pool --create-home --shell /usr/sbin/nologin codex-pool

RUN mkdir -p /usr/local/lib/codex-pool/releases/docker \
      /usr/local/lib/codex-pool/releases/docker/registrar-python \
      /var/lib/codex-pool/run /var/lib/codex-pool/data/spool \
      /var/lib/codex-pool/data/journal /var/lib/codex-pool/data/diagnostics \
      /var/lib/codex-pool/data/tmp/browser /var/lib/codex-pool/data/run \
      /var/lib/codex-pool/data/keys /var/lib/codex-pool/data/core-state && \
    chmod 0700 /var/lib/codex-pool /var/lib/codex-pool/run /var/lib/codex-pool/data \
      /var/lib/codex-pool/data/spool /var/lib/codex-pool/data/journal \
      /var/lib/codex-pool/data/diagnostics /var/lib/codex-pool/data/tmp \
      /var/lib/codex-pool/data/tmp/browser /var/lib/codex-pool/data/run \
      /var/lib/codex-pool/data/keys /var/lib/codex-pool/data/core-state && \
    chown -R codex-pool:codex-pool /var/lib/codex-pool
COPY --from=build /out/codex-pool-server /usr/local/lib/codex-pool/releases/docker/codex-pool-server
COPY --from=build /out/codex-pool-handoff /usr/local/bin/codex-pool-handoff
COPY --from=build /out/gateway-bin /usr/local/lib/codex-pool/bin
COPY --from=registrar-node /usr/local/bin/node /usr/local/bin/node
COPY --from=registrar-node /opt/registrar-node /usr/local/lib/codex-pool/releases/docker/registrar-node
COPY services/codex_register/requirements.txt /usr/local/lib/codex-pool/releases/docker/registrar-python/requirements.txt
RUN python3 -m venv /usr/local/lib/codex-pool/releases/docker/registrar-python-venv && \
    /usr/local/lib/codex-pool/releases/docker/registrar-python-venv/bin/pip \
      install --disable-pip-version-check \
      --requirement /usr/local/lib/codex-pool/releases/docker/registrar-python/requirements.txt && \
    /usr/local/lib/codex-pool/releases/docker/registrar-python-venv/bin/pip check
COPY services/codex_register/protocol_register.py \
     services/codex_register/browser_register.py \
     services/codex_register/reg_v3.py \
     services/codex_register/phone_verify.py \
     /usr/local/lib/codex-pool/releases/docker/registrar-python/
COPY config.example.json /etc/codex-pool/config.json
COPY super-instruct /usr/local/lib/codex-pool/releases/docker/super-instruct
COPY deploy/docker-entrypoint.sh /usr/local/bin/codex-pool-entrypoint
RUN PYTHONDONTWRITEBYTECODE=1 \
      /usr/local/lib/codex-pool/releases/docker/registrar-python-venv/bin/python -c \
      'import ast, pathlib; root=pathlib.Path("/usr/local/lib/codex-pool/releases/docker/registrar-python"); [ast.parse(path.read_text(encoding="utf-8"), filename=str(path)) for path in root.glob("*.py")]' && \
    PYTHONDONTWRITEBYTECODE=1 \
      /usr/local/lib/codex-pool/releases/docker/registrar-python-venv/bin/python -c \
      'import curl_cffi, playwright, playwright_stealth, requests, urllib3' && \
    chown -R root:codex-pool \
      /usr/local/lib/codex-pool/releases/docker/registrar-node \
      /usr/local/lib/codex-pool/releases/docker/registrar-python \
      /usr/local/lib/codex-pool/releases/docker/registrar-python-venv \
      /usr/local/lib/codex-pool/releases/docker/super-instruct && \
    chmod -R u=rwX,g=rX,o= \
      /usr/local/lib/codex-pool/releases/docker/registrar-node \
      /usr/local/lib/codex-pool/releases/docker/registrar-python \
      /usr/local/lib/codex-pool/releases/docker/registrar-python-venv \
      /usr/local/lib/codex-pool/releases/docker/super-instruct && \
    chmod 0755 /usr/local/bin/codex-pool-entrypoint

USER codex-pool
WORKDIR /var/lib/codex-pool
ENV CODEX_POOL_DATA_DIR=/var/lib/codex-pool/data
ENV CODEX_REG_NODE=/usr/local/bin/node
ENV CODEX_REG_NODE_DIR=/usr/local/lib/codex-pool/releases/docker/registrar-node
ENV CODEX_REG_PYTHON=/usr/local/lib/codex-pool/releases/docker/registrar-python-venv/bin/python
ENV CODEX_REG_PROTOCOL_SCRIPT=/usr/local/lib/codex-pool/releases/docker/registrar-python/protocol_register.py
ENV CODEX_REG_SCRIPT=/usr/local/lib/codex-pool/releases/docker/registrar-python/browser_register.py
ENV CODEX_POOL_SUPER_INSTRUCT_DIR=/usr/local/lib/codex-pool/releases/docker/super-instruct/codex-skills
ENV CODEX_REG_V3_SCRIPT=/usr/local/lib/codex-pool/releases/docker/registrar-python/reg_v3.py
ENV CODEX_REG_CHROME=/usr/bin/chromium
ENV CHROME_PATH=/usr/bin/chromium
VOLUME ["/var/lib/codex-pool"]
EXPOSE 8787
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 CMD curl --noproxy '*' -fsS http://127.0.0.1:8787/readyz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/codex-pool-entrypoint"]
CMD ["--config", "/etc/codex-pool/config.json"]
