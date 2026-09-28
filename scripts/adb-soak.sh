#!/bin/bash
# adb 设备稳定性压测（平板入库前验证，模拟 paygateway 运行时调用面）
#
# 用法: ./adb-soak.sh <serial> [轮数] [间隔秒]
#   serial: WiFi adb 为 host:5555（容器/裸跑同协议同路径）；USB 直连为纯设备号
#
# 每轮覆盖 adbmgr 实际用到的操作: get-state（连接口径）→ shell → wm size
# → screencap → uiautomator dump+cat，结尾输出成功率与平均时延对比。
set -u
SERIAL=${1:?'需要 serial 参数（host:5555 或 USB 设备号）'}
N=${2:-10}
IV=${3:-3}

ok_state=0; ok_shell=0; ok_size=0; ok_shot=0; ok_dump=0
sum_state=0; sum_shell=0; sum_size=0; sum_shot=0; sum_dump=0

ms() { echo $(( ($2 - $1) / 1000000 )); }  # 起止纳秒 → 毫秒

for i in $(seq 1 "$N"); do
    t0=$(date +%s%N)
    case "$SERIAL" in
        *:*) adb connect "$SERIAL" >/dev/null 2>&1 ;;
    esac
    st=$(adb -s "$SERIAL" get-state 2>/dev/null)
    d=$(ms "$t0" "$(date +%s%N)")
    if [ "$st" != "device" ]; then
        echo "[$i/$N] ❌ 设备不可达（connect+state ${d}ms）"
        sleep "$IV"; continue
    fi
    ok_state=$((ok_state+1)); sum_state=$((sum_state+d))

    t0=$(date +%s%N); out=$(adb -s "$SERIAL" shell echo soak-ok 2>/dev/null)
    d=$(ms "$t0" "$(date +%s%N)"); sum_shell=$((sum_shell+d))
    [ "$out" = "soak-ok" ] && ok_shell=$((ok_shell+1))

    t0=$(date +%s%N); sz=$(adb -s "$SERIAL" shell wm size 2>/dev/null | grep -o '[0-9]\+x[0-9]\+' | tail -1)
    d=$(ms "$t0" "$(date +%s%N)")
    [ -n "$sz" ] && { ok_size=$((ok_size+1)); sum_size=$((sum_size+d)); }

    t0=$(date +%s%N); adb -s "$SERIAL" exec-out screencap -p > /tmp/opencode/soak_shot.png 2>/dev/null
    d=$(ms "$t0" "$(date +%s%N)")
    [ -s /tmp/opencode/soak_shot.png ] && { ok_shot=$((ok_shot+1)); sum_shot=$((sum_shot+d)); }

    t0=$(date +%s%N)
    adb -s "$SERIAL" shell uiautomator dump /sdcard/soak_ui.xml >/dev/null 2>&1
    adb -s "$SERIAL" shell cat /sdcard/soak_ui.xml > /tmp/opencode/soak_ui.xml 2>/dev/null
    d=$(ms "$t0" "$(date +%s%N)")
    grep -q '<' /tmp/opencode/soak_ui.xml && { ok_dump=$((ok_dump+1)); sum_dump=$((sum_dump+d)); }

    
    echo "[$i/$N] ✅ 分辨率=${sz:-未取到}  shell=${d}ms  dump+读取=${d}ms  截图成功=${ok_shot}  dump成功=${ok_dump}"
    sleep "$IV"
done

echo "============ 汇总（共 $N 轮，覆盖 paygateway 实际调用面）============"
[ "$ok_size" -gt 0 ] && printf "wm size       : %d/%d  平均 %dms\n" "$ok_size" "$N" $((sum_size/ok_size))
[ "$ok_shot" -gt 0 ] && printf "screencap     : %d/%d  平均 %dms\n" "$ok_shot" "$N" $((sum_shot/ok_shot))
[ "$ok_dump" -gt 0 ] && printf "ui-dump       : %d/%d  平均 %dms\n" "$ok_dump" "$N" $((sum_dump/ok_dump))
if [ "$ok_state" -gt 0 ]; then
    avg_state=$((sum_state/ok_state)); avg_shell=$((sum_shell/ok_state))
    printf "connect/state : %d/%d  平均 %dms\n" "$ok_state" "$N" "$avg_state"
    printf "shell         : %d/%d  平均 %dms\n" "$ok_shell" "$N" "$avg_shell"
fi
echo "（时延由设备性能与 WiFi 决定——同一台设备在容器内与宿主机跑一遍结果应一致，" \
     "即可证明网络桥接无额外损耗）"
