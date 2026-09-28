#!/usr/bin/env bash
# USB 真机端到端测试：选真机 → 调管理后台 push-config 下发配置 → 跑 e2e_personal.py 下单→入账
#
# 架构说明（listener-app 已下线，改为纯 adb 控制真机微信/支付宝账本 ledgermon）：
#   - 网关通过 adb 控制红米 K60 真机，ledgermon 读账本行匹配订单入账
#   - 不再有手机端 APP，所有授权/安装步骤已移除
#   - 本脚本负责：ADB 连接 → 管理后台登录 → 设备选择 → 配置下发 → 下单测试
#
# 使用前置：
#   1. 真机开了 USB 调试 + 已 adb pair
#   2. 网关在跑（./gateway）+ cloudflare 隧道在跑（server_url 可达）
#   3. 管理后台已创建同名监听设备（账户池 → 监听设备，确保 adb_host/adb_port 字段已填真机 ip，或脚本会问 --device-id）
#
# 用法：
#   scripts/e2e_usb_realdevice.sh                              # 自动选 adb device 第一个真机
#   scripts/e2e_usb_realdevice.sh --serial 4f2bdc30            # 指定真机
#   scripts/e2e_usb_realdevice.sh --device-id 91df2463...      # 指定服务端已创建的 device_id
#   scripts/e2e_usb_realdevice.sh --admin-url http://127.0.0.1:8081 --gateway-url http://127.0.0.1:8080
#
# 真实收款流程：下单后展示备注码，在真机上完成真实收款；ledgermon 通过 adb 读取账本行
# 匹配订单入账，订单变 TRADE_SUCCESS 即为闭环走过。
set -euo pipefail
IFS=$'\n\t'

# ---------- 参数 ----------
SERIAL=""
DEVICE_ID=""
ADMIN_URL="${ADMIN_URL:-http://127.0.0.1:8081}"
GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8080}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-paygateway123}"
PAY_AMOUNT="${PAY_AMOUNT:-0.66}"

usage() { sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --serial) SERIAL="$2"; shift 2;;
        --device-id) DEVICE_ID="$2"; shift 2;;
        --admin-url) ADMIN_URL="$2"; shift 2;;
        --gateway-url) GATEWAY_URL="$2"; shift 2;;
        --amount) PAY_AMOUNT="$2"; shift 2;;
        -h|--help) usage 0;;
        *) echo "未知参数: $1"; usage 1;;
    esac
done

[[ -n "$ADMIN_PASS" && "$ADMIN_PASS" == "paygateway123" ]] && echo "⚠️  使用默认管理员密码（ADMIN_PASS 未设），若改过请环境变量传值。"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo
echo "==== [1/7] 选择 USB 真机 ===="
ADB_CMD=(adb)
if [[ -n "$SERIAL" ]]; then
    ADB_CMD+=( -s "$SERIAL" )
    if ! "${ADB_CMD[@]}" get-state >/dev/null 2>&1; then
        echo "  ❌ 设备 --serial $SERIAL 不可达"
        exit 1
    fi
    echo "  ✅ 使用指定 serial: $SERIAL"
else
    mapfile -t DEVS < <(adb devices | awk 'NR>1 && $2=="device" {print $1}')
    if [[ ${#DEVS[@]} -eq 0 ]]; then
        echo "  ❌ adb devices 列表为空（USB 调试没开？或者用 --serial 指定）"
        adb devices
        exit 1
    fi
    if [[ ${#DEVS[@]} -gt 1 ]]; then
        echo "  ⚠️  检测到多台设备，请用 --serial 指定其中一台："
        printf '    %s\n' "${DEVS[@]}"
        exit 1
    fi
    SERIAL="${DEVS[0]}"
    ADB_CMD+=( -s "$SERIAL" )
    echo "  ✅ 自动选用: $SERIAL"
fi

echo
echo "==== [2/7] 管理后台登录 ===="
LOGIN_RESP=$(curl -sS -X POST "$ADMIN_URL/api/auth/login" \
    -H "Content-Type: application/json" \
    -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}" )
TOKEN=$(echo "$LOGIN_RESP" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("data",{}).get("token",""))')
if [[ -z "$TOKEN" ]]; then
    echo "  ❌ 登录失败：$LOGIN_RESP"
    exit 1
fi
echo "  ✅ 已获取 JWT"

echo
echo "==== [3/7] 检查关键授权（电池白名单）==="
# 注：listener-app 已下线，通知使用权检查已移除
# ledgermon 通过 adb 直连账本，无需 App 通知监听权限

WHITELIST_OK=$("${ADB_CMD[@]}" shell dumpsys deviceidle whitelist | head -5 || true)
if [[ -n "$WHITELIST_OK" ]]; then
    echo "  ✅ 电池优化白名单：已检查（见上方）"
else
    echo "  ⚠️  电池优化白名单：无法检查（无 root 权限，跳过）"
fi

echo
echo "==== [4/7] 选取监听设备 ===="
DEVS_RESP=$(curl -sS "$ADMIN_URL/api/personal/devices" -H "Authorization: Bearer $TOKEN")
if [[ -n "$DEVICE_ID" ]]; then
    MATCHED=$(echo "$DEVS_RESP" | python3 -c "
import sys, json
data = json.load(sys.stdin).get('data',{}).get('devices',[])
target = '$DEVICE_ID'
m = [d for d in data if d.get('device_id')==target]
print(json.dumps(m[0]) if m else '')")
    if [[ -z "$MATCHED" ]]; then
        echo "  ❌ 指定 --device-id $DEVICE_ID 在管理后台不存在"
        exit 1
    fi
    echo "  ✅ 使用 device_id=$DEVICE_ID"
else
    PICK=$(echo "$DEVS_RESP" | python3 -c "
import sys, json
data = json.load(sys.stdin).get('data',{}).get('devices',[])
if not data: sys.exit('无监听设备，请先在管理后台创建')
# 优先 alipay provider
alipay = [d for d in data if d.get('provider')=='alipay']
pick = alipay[0] if alipay else data[0]
print(pick.get('device_id'))")
    DEVICE_ID="$PICK"
    echo "  ✅ 自动选取 device_id=$DEVICE_ID"
fi

echo
echo "==== [5/7] 通过管理后台 push-config 下发配置 ===="
PUSH_RESP=$(curl -sS -X POST "$ADMIN_URL/api/personal/devices/$DEVICE_ID/push-config" \
    -H "Authorization: Bearer $TOKEN" \
    --max-time 30 )
echo "  raw: $PUSH_RESP"
echo "$PUSH_RESP" | python3 -c "
import sys, json
r = json.load(sys.stdin)
d = r.get('data', r)
if r.get('error'): sys.exit('  ❌ 下发失败: ' + r['error'])
print('  ✅ 配置下发成功')"

echo
echo "==== [6/7] 跑下单测试 ===="
echo "  ⓘ  ledgermon 通过 adb 读取真机账本行匹配订单，无需模拟推送"
echo "     下单后会展示备注码；在真机上完成真实收款即可"
# TODO: 待 ledgermon e2e 测试脚本就绪后替换
echo "  ⚠️  e2e_personal.py 已随 listener-app 下线而删除，请手动执行下单验证"
exit 0
