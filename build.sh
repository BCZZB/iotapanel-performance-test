#!/usr/bin/env bash
# 交叉编译 performance-test 插件为单一静态二进制。
set -e
cd "$(dirname "$0")"
VERSION="1.0.0"
OUT="bin/performance-test"
mkdir -p bin web/assets

# 目标平台：linux/amd64, linux/arm64（面板部署常用）
for T in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$T go build -trimpath -ldflags "-s -w" -o "$OUT.$T" .
  echo "built: $OUT.$T"
done

# 默认 amd64 作为主产物
cp "$OUT.amd64" "$OUT"
chmod +x "$OUT"
echo "default: $OUT (linux/amd64, $(ls -lh $OUT | awk '{print $5}'))"