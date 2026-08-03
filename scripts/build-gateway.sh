#!/bin/bash
# 预编译所有平台的网关二进制

set -e

echo "🔨 预编译网关二进制..."

# 创建输出目录
mkdir -p bin

# 编译目标平台
PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
)

for PLATFORM in "${PLATFORMS[@]}"; do
  IFS='/' read -r GOOS GOARCH <<< "$PLATFORM"
  OUTPUT="bin/gateway-${GOOS}-${GOARCH}"

  if [ "$GOOS" = "windows" ]; then
    OUTPUT="${OUTPUT}.exe"
  fi

  echo "  ✓ 编译 ${GOOS}/${GOARCH}..."
  env GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build -o "$OUTPUT" ./cmd/gateway
done

echo "✅ 编译完成！二进制文件："
ls -lh bin/gateway-*
