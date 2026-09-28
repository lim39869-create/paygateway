#!/bin/bash
# 支付网关启动脚本
# 自动读取 .env 配置，从密钥文件加载密钥

cd "$(dirname "$0")"

# 加载 .env
set -a
source .env
set +a

# 从密钥文件加载私钥和公钥
if [ -f "$ALIPAY_PRIVATE_KEY_FILE" ]; then
    export ALIPAY_PRIVATE_KEY=$(cat "$ALIPAY_PRIVATE_KEY_FILE" | tr -d '\n' | tr -d ' ')
    echo "✅ 已加载应用私钥"
fi

if [ -f "$ALIPAY_PUBLIC_KEY_FILE" ]; then
    export ALIPAY_PUBLIC_KEY=$(cat "$ALIPAY_PUBLIC_KEY_FILE" | tr -d '\n' | tr -d ' ')
    echo "✅ 已加载支付宝公钥"
fi

echo "🚀 启动支付网关..."
echo "   监听: ${GATEWAY_ADDR:-:8080}"
echo "   数据库: ${ORDER_DB:-data/paygateway.db}"

# 自动下载并嵌入 cloudflared 二进制（如已存在则跳过）
echo ""
go generate ./internal/tunnel/

echo ""
go run ./cmd/gateway/