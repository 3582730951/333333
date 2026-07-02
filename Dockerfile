FROM golang:1.19-bullseye AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go test ./... && \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/codex-pool-server ./cmd/pool-server && \
    mkdir -p /out/gateway-bin && \
    for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
      os="${target%/*}"; arch="${target#*/}"; ext=""; \
      if [ "$os" = "windows" ]; then ext=".exe"; fi; \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "/out/gateway-bin/gateway-$os-$arch$ext" ./cmd/gateway; \
    done

FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates sqlite3 && rm -rf /var/lib/apt/lists/*
RUN useradd --system --home /var/lib/codex-pool --create-home --shell /usr/sbin/nologin codex-pool

COPY --from=build /out/codex-pool-server /usr/local/bin/codex-pool-server
COPY --from=build /out/gateway-bin /usr/local/lib/codex-pool/bin
COPY config.example.json /etc/codex-pool/config.json

USER codex-pool
WORKDIR /var/lib/codex-pool
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/codex-pool-server"]
CMD ["--config", "/etc/codex-pool/config.json"]
