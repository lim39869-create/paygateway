#!/bin/bash
# 支付网关停止脚本（与 start.sh / .gateway.pid 配套；systemd 部署请用 systemctl stop）
cd "$(dirname "$0")"

PID_FILE="./.gateway.pid"

if [ ! -f "$PID_FILE" ]; then
    echo "无 pid 文件 $PID_FILE（可能未用 start.sh 启动，手动找进程用 pgrep -af 'paygateway/gateway'）"
    exit 0
fi

PID=$(cat "$PID_FILE" 2>/dev/null || echo "")
if [ -z "$PID" ]; then
    rm -f "$PID_FILE"
    echo "pid 文件为空，已清理"
    exit 0
fi

if ! kill -0 "$PID" 2>/dev/null; then
    rm -f "$PID_FILE"
    echo "进程 $PID 已不在，pid 文件已清理"
    exit 0
fi

# SIGTERM 优雅停止，等待最多 10s
kill "$PID" 2>/dev/null
for i in $(seq 1 50); do
    if ! kill -0 "$PID" 2>/dev/null; then
        break
    fi
    sleep 0.2
done

if kill -0 "$PID" 2>/dev/null; then
    echo "10s 后仍未退出，发送 SIGKILL"
    kill -9 "$PID" 2>/dev/null
    sleep 0.3
fi

rm -f "$PID_FILE"
echo "已停止 $PID"
