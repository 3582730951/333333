FROM golang:1.19-bullseye AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go test ./... && \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/codex-pool-server ./cmd/pool-server

FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates sqlite3 && rm -rf /var/lib/apt/lists/*
RUN useradd --system --home /var/lib/codex-pool --create-home --shell /usr/sbin/nologin codex-pool

COPY --from=build /out/codex-pool-server /usr/local/bin/codex-pool-server
COPY config.example.json /etc/codex-pool/config.json

USER codex-pool
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/codex-pool-server"]
CMD ["--config", "/etc/codex-pool/config.json"]
