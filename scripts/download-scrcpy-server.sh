#!/bin/bash
# 下载 scrcpy-server.jar（编译时嵌入，go generate 调用）
# 需要网络；下载失败时提示手动放置
set -e
DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="$DIR/internal/scrcpy/scrcpy-server.jar"

if [ -f "$TARGET" ] && [ -s "$TARGET" ]; then
    echo "scrcpy-server.jar 已存在，跳过下载"
    exit 0
fi

# scrcpy v4.1 release 的 server jar
URL="https://github.com/Genymobile/scrcpy/releases/download/v4.1/scrcpy-server-v4.1"

echo "下载 scrcpy-server v4.1 → $TARGET"
if curl -fsSL -o "$TARGET" "$URL" 2>/dev/null && [ -s "$TARGET" ]; then
    echo "✅ 下载完成 ($(du -h "$TARGET" | cut -f1))"
else
    rm -f "$TARGET"
    echo "❌ 下载失败，请手动下载:"
    echo "   $URL"
    echo "   放置到: $TARGET"
    exit 1
fi
