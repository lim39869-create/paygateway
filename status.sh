#!/bin/bash
# 支付网关状态脚本（与 start.sh / .gateway.pid 配套；systemd 部署请用 systemctl status）
cd "$(dirname "$0")"

PID_FILE="./.gateway.pid"

if [ -f "$PID_FILE" ]; then
    PID=$(cat "$PID_FILE" 2>/dev/null || echo "")
    if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
        STARTED=$(ps -o lstart= -p "$PID" 2>/dev/null | sed 's/^ *//')
        ELAPSED=$(ps -o etime= -p "$PID" 2>/dev/null | sed 's/^ *//')
        echo "✅ 运行中 pid=$PID 启动于 $STARTED 已运行 $ELAPSED"
        echo "   监听端口：$(ss -lntp 2>/dev/null | grep -E "pid=$PID" | awk '{print $4}' | tr '\n' ' ')"
        exit 0
    fi
fi

echo "⚠️  未运行（pid 文件缺失或进程已死）"
echo "   通过 pgrep 兜底检查：$(pgrep -af 'paygateway/gateway' | head -1 || echo '无')"
exit 1
