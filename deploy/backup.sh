#!/bin/bash
# PayGateway 每日数据库备份（RESILIENCE.md P0-3）
#
# 功能：
#   - 备份全部 SQLite 库（paygateway / ledger / accounts / personal / upstreams）
#   - 用 sqlite3 .backup 命令在线备份（WAL 模式安全，无需停服）
#   - 保留最近 N 份（默认 14 天），旧备份自动清理
#
# 使用：
#   手动：    bash deploy/backup.sh
#   crontab： 0 2 * * * /opt/paygateway/deploy/backup.sh >> /var/log/paygateway-backup.log 2>&1
#   systemd timer（推荐）：见 deploy/paygateway.service 同目录的 deploy/README 或 docs/OPERATIONS.md "备份"
#
# 注意：请先确认系统装有 sqlite3 命令行工具（apt install sqlite3）

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DATA_DIR="$PROJECT_DIR/data"
BACKUP_DIR="${PAYGWAY_BACKUP_DIR:-$DATA_DIR/backups}"
KEEP_DAYS="${PAYGWAY_BACKUP_KEEP_DAYS:-14}"

# 需要备份的数据库文件（相对 data/）
DBS=(paygateway.db ledger.db accounts.db personal.db upstreams.db)

mkdir -p "$BACKUP_DIR"
STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_DIR_TS="$BACKUP_DIR/$STAMP"
mkdir -p "$BACKUP_DIR_TS"

FAILED=0
for db in "${DBS[@]}"; do
  SRC="$DATA_DIR/$db"
  if [ ! -f "$SRC" ]; then
    echo "[skip] $db 不存在"
    continue
  fi
  DST="$BACKUP_DIR_TS/$db"
  # 在线备份：SQLite 官方推荐方式，WAL 模式下安全（非简单 cp）
  if sqlite3 "$SRC" ".backup '$DST'" 2>/dev/null; then
    # 设置权限与源一致（含敏感账户库）
    chmod 600 "$DST"
    echo "[ok]   $db ($(du -h "$DST" | cut -f1))"
  else
    echo "[FAIL] $db 备份失败（sqlite3 是否安装？）"
    FAILED=1
  fi
done

# 清理过期备份（按日期目录保留 KEEP_DAYS 天）
find "$BACKUP_DIR" -maxdepth 1 -type d -name "20*" -mtime +"$KEEP_DAYS" -exec rm -rf {} \; 2>/dev/null || true

echo "=== 备份完成: $BACKUP_DIR_TS（保留 ${KEEP_DAYS} 天） ==="
exit $FAILED
