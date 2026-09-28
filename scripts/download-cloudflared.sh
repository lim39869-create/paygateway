#!/bin/bash
# download-cloudflared.sh — 下载 cloudflared 二进制到 internal/tunnel/ 供 //go:embed 使用
#
# 用法:
#   go generate ./internal/tunnel/          # 自动调用
#   bash scripts/download-cloudflared.sh    # 手动调用
#
# 下载策略（自动回退）:
#   1. GitHub 直连
#   2. GITHUB_MIRROR 环境变量指定的镜像（如有设置）
#   3. 内置镜像列表（ghproxy.cc / ghproxy.net / gh.api.99988866.xyz）
#
# 支持平台: linux amd64/arm64, darwin amd64/arm64

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TARGET_DIR="$PROJECT_DIR/internal/tunnel"
TARGET_FILE="$TARGET_DIR/cloudflared"

# 如果二进制已存在且可执行，跳过下载
if [ -x "$TARGET_FILE" ]; then
	echo "✓ cloudflared 已存在，跳过下载"
	exit 0
fi

OS="${GOOS:-$(go env GOOS)}"
ARCH="${GOARCH:-$(go env GOARCH)}"

case "$OS/$ARCH" in
linux/amd64)  REL_PATH="cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64"  ;;
linux/arm64)  REL_PATH="cloudflare/cloudflared/releases/latest/download/cloudflared-linux-arm64"  ;;
darwin/arm64) REL_PATH="cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-arm64" ;;
darwin/amd64) REL_PATH="cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-amd64" ;;
*)
	echo "❌ 不支持的平台: $OS/$ARCH"
	exit 1
	;;
esac

# 构建下载 URL 列表：环境变量镜像优先，内置镜像回退
URLS=()
if [ -n "${GITHUB_MIRROR:-}" ]; then
	URLS+=("${GITHUB_MIRROR%/}/https://github.com/$REL_PATH")
fi
URLS+=(
	"https://ghproxy.cc/https://github.com/$REL_PATH"
	"https://ghproxy.net/https://github.com/$REL_PATH"
	"https://gh.api.99988866.xyz/https://github.com/$REL_PATH"
	"https://github.com/$REL_PATH"
)

echo "⬇️  正在下载 cloudflared ($OS/$ARCH) ..."

for url in "${URLS[@]}"; do
	echo "  尝试: $url"
	set +e
	# 镜像站常见 SSL 证书问题，统一用 -k；直连 GitHub 不用 -k
	if echo "$url" | grep -q "github.com/$REL_PATH$" && [ -z "${GITHUB_MIRROR:-}" ]; then
		curl -sSL --fail --connect-timeout 10 --max-time 120 "$url" -o "$TARGET_FILE" 2>/dev/null
	else
		curl -sSL --fail --connect-timeout 10 --max-time 120 -k "$url" -o "$TARGET_FILE" 2>/dev/null
	fi
	rc=$?
	set -e
	if [ $rc -eq 0 ] && [ -s "$TARGET_FILE" ]; then
		chmod +x "$TARGET_FILE"
		echo "✓ 下载成功: $TARGET_FILE ($(du -h "$TARGET_FILE" | cut -f1))"
		exit 0
	fi
	echo "  ✗ 失败，换下一个..."
done

echo "❌ 所有下载源均失败，请手动下载:"
echo "   https://github.com/$REL_PATH"
echo "   放到 $TARGET_FILE 并 chmod +x"
exit 1
