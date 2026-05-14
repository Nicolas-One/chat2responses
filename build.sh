#!/bin/bash
# =====================================================
# chat2responses 一键打包脚本
# 用法: ./build.sh [输出路径]
# 默认输出: ./chat2responses
# =====================================================
set -euo pipefail

cd "$(dirname "$0")"

BUILD_TIME=$(date '+%Y-%m-%d %H:%M:%S')
GO_VERSION=$(go version 2>/dev/null | awk '{print $3}')
COMMIT=$(git log --format=%h -1 2>/dev/null || echo "unknown")

echo "==================================="
echo " chat2responses 构建"
echo " 时间: $BUILD_TIME"
echo " Go:   ${GO_VERSION:-未安装}"
echo " 提交: $COMMIT"
echo "==================================="

# 检查 Go
if ! command -v go &>/dev/null; then
    echo "[错误] 未找到 go 命令，请安装 Go 1.16+"
    exit 1
fi

OUTPUT="${1:-chat2responses}"

echo
echo "[1/3] 清理旧构建..."
rm -f "$OUTPUT" "$OUTPUT".md5 2>/dev/null

echo "[2/3] 编译中..."
go build -o "$OUTPUT" \
    -ldflags="-s -w" \
    ./*.go

echo "[3/3] 校验..."
md5sum "$OUTPUT" | tee "$OUTPUT".md5

echo
echo "==================================="
echo " 完成"
echo " 二进制: $(pwd)/$OUTPUT"
echo " 大小:   $(ls -lh "$OUTPUT" | awk '{print $5}')"
echo " MD5:   $(cat "$OUTPUT".md5 | cut -d' ' -f1)"
echo "==================================="
