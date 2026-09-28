#!/bin/bash
# 支付网关启动脚本（生产环境裸跑场景，建议优先用 systemd：deploy/paygateway.service）
#
# 加载 .env（ACCOUNT_MASTER_KEY / GATEWAY_ADDR / 数据库路径等基础设施配置），
# 然后以编译好的二进制启动服务。带 pid 文件 + 已运行检查，避免重复启动。

cd "$(dirname "$0")"

PID_FILE="./.gateway.pid"

# 加载 .env
if [ -f .env ]; then
    set -a
    source .env
    set +a
fi

# 已运行检查：通过 pid 文件 + 进程存活双重验证
if [ -f "$PID_FILE" ]; then
    OLD_PID=$(cat "$PID_FILE" 2>/dev/null || echo "")
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
        echo "❌ gateway 已在运行（pid=$OLD_PID，pid 文件 $PID_FILE）" >&2
        echo "   如需停止：kill $OLD_PID && rm $PID_FILE" >&2
        echo "   如需重启：先停再启，或用 ./stop.sh" >&2
        exit 1
    else
        # pid 文件在但进程已死：清理残留
        rm -f "$PID_FILE"
    fi
fi

# 后台启动；保留 stderr 以便启动失败时能看到
nohup ./gateway > /dev/null 2>&1 &
PID=$!
echo "$PID" > "$PID_FILE"

# 等待短秒数验证进程未立刻退出（端口冲突 / 配置错误会快速 crash）
sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
    rm -f "$PID_FILE"
    echo "❌ gateway 启动后 1 秒内退出，请检查日志：tail -50 logs/paygateway.log" >&2
    exit 1
fi

echo "$PID"
