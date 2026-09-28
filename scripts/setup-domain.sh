#!/bin/bash
# Cloudflare 固定域名 + 隧道一键初始化（全自动）
# 
# 功能：自动创建/确认（全部幂等）：
#   1. Cloudflare zone（paygateway.de5.net 全托管）
#   2. DNS CNAME 记录（proxied → 隧道）
#   3. hostname route（域名 → 隧道映射）
#   4. DNSHe 子域名（不存在时自动注册） + NS 委派给 Cloudflare
#   5. 等待 zone 激活
#   6. Universal SSL 证书（自动签发，toggle 触发）
#   7. HTTPS 公网验证
#
# 前置条件：
#   1. go build -o gateway ./cmd/gateway/ (编译产物)
#   2. .env 包含以下变量：
#      CF_EMAIL, CF_GLOBAL_KEY, CF_TUNNEL_HOSTNAME, CF_TUNNEL_CREDENTIALS
#      DNSHE_API_KEY, DNSHE_API_SECRET   (de5.net 免费子域名场景)
#
# 使用：
#   bash scripts/setup-domain.sh
#
# 换环境流程：复制 .env → go build → 本脚本 → ./start.sh
# 全新域名：把 CF_TUNNEL_HOSTNAME 换成新前缀（如 myapp.de5.net），脚本全自动注册+配置。

set -euo pipefail

cd "$(dirname "$0")/.."

# 加载 .env
if [ -f .env ]; then
    set -a
    source .env
    set +a
fi

: "${CF_EMAIL:?请设置 CF_EMAIL}"
: "${CF_GLOBAL_KEY:?请设置 CF_GLOBAL_KEY}"
: "${CF_TUNNEL_HOSTNAME:?请设置 CF_TUNNEL_HOSTNAME}"
: "${CF_TUNNEL_CREDENTIALS:?请设置 CF_TUNNEL_CREDENTIALS}"

# 从凭据中提取 TunnelID
TUNNEL_ID=$(echo "$CF_TUNNEL_CREDENTIALS" | base64 -d 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['TunnelID'])" 2>/dev/null || echo "")
if [ -z "$TUNNEL_ID" ]; then
    echo "❌ 无法从 CF_TUNNEL_CREDENTIALS 中提取 TunnelID，请确认格式正确"
    exit 1
fi

CF_AUTH=(-H "X-Auth-Email: $CF_EMAIL" -H "X-Auth-Key: $CF_GLOBAL_KEY" -H "Content-Type: application/json")
echo "=== Cloudflare 初始化: $CF_TUNNEL_HOSTNAME (隧道 $TUNNEL_ID) ==="

# 1. 确保 zone 存在
echo "--- 1/7 检查 zone: $CF_TUNNEL_HOSTNAME ---"
ZONE_ID=$(curl -sS -m 15 "https://api.cloudflare.com/client/v4/zones?name=$CF_TUNNEL_HOSTNAME" "${CF_AUTH[@]}" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['result'][0]['id'] if d.get('result') else '')" 2>/dev/null || echo "")
if [ -z "$ZONE_ID" ]; then
    echo "  zone 不存在，正在创建..."
    ZONE_ID=$(curl -sS -m 20 -X POST "https://api.cloudflare.com/client/v4/zones" \
        "${CF_AUTH[@]}" \
        -d "{\"name\":\"$CF_TUNNEL_HOSTNAME\",\"type\":\"full\"}" | \
        python3 -c "import sys,json; d=json.load(sys.stdin); print(d['result']['id'] if d.get('success') else '')" 2>/dev/null || echo "")
    if [ -z "$ZONE_ID" ]; then
        echo "❌ zone 创建失败（可能是 TLD 或重复），跳过"
    else
        echo "  ✅ zone 已创建 (ID: $ZONE_ID)"
    fi
else
    echo "  ✅ zone 已存在 (ID: $ZONE_ID)"
fi

if [ -z "$ZONE_ID" ]; then
    echo "❌ 无 zone ID，无法继续。请检查域名是否可被 Cloudflare 托管"
    exit 1
fi

# 2. 确保 DNS CNAME 记录（proxied=true）
echo "--- 2/7 检查 CNAME 记录 ---"
EXISTING=$(curl -sS -m 15 "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/dns_records?type=CNAME&name=$CF_TUNNEL_HOSTNAME" "${CF_AUTH[@]}" | \
    python3 -c "import sys,json; d=json.load(sys.stdin); r=d.get('result',[]); print(r[0]['id'] if r else '')" 2>/dev/null || echo "")
if [ -z "$EXISTING" ]; then
    echo "  CNAME 不存在，创建中..."
    curl -sS -m 15 -X POST "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/dns_records" \
        "${CF_AUTH[@]}" \
        -d "{\"type\":\"CNAME\",\"name\":\"@\",\"content\":\"$TUNNEL_ID.cfargotunnel.com\",\"proxied\":true}" > /dev/null
    echo "  ✅ CNAME 已创建 (proxied=true)"
else
    echo "  ✅ CNAME 已存在"
fi

# 3. 确保 hostname route
echo "--- 3/7 检查 hostname route ---"
ACT_ID=$(curl -sS -m 10 "https://api.cloudflare.com/client/v4/accounts?limit=1" "${CF_AUTH[@]}" | python3 -c "import sys,json; print(json.load(sys.stdin)['result'][0]['id'])" 2>/dev/null || echo "")
if [ -n "$ACT_ID" ]; then
    ROUTE=$(curl -sS -m 15 "https://api.cloudflare.com/client/v4/accounts/$ACT_ID/zerotrust/routes/hostname" "${CF_AUTH[@]}" | \
        python3 -c "import sys,json; d=json.load(sys.stdin); r=[x for x in d.get('result',[]) if x['hostname']=='$CF_TUNNEL_HOSTNAME']; print(r[0]['id'] if r else '')" 2>/dev/null || echo "")
    if [ -z "$ROUTE" ]; then
        echo "  hostname route 不存在，创建中..."
        curl -sS -m 15 -X POST "https://api.cloudflare.com/client/v4/accounts/$ACT_ID/zerotrust/routes/hostname" \
            "${CF_AUTH[@]}" \
            -d "{\"hostname\":\"$CF_TUNNEL_HOSTNAME\",\"tunnel_id\":\"$TUNNEL_ID\",\"tun_type\":\"cfd_tunnel\",\"comment\":\"setup-domain.sh\"}" > /dev/null
        echo "  ✅ hostname route 已创建"
    else
        echo "  ✅ hostname route 已存在"
    fi
fi

# 4. DNSHe NS 委派（de5.net 免费子域名场景）
#    de5.net 的 DNS 在 DNSHe，需把 paygateway.de5.net 的 NS 委派给 Cloudflare。
#    若 DNSHE_API_KEY 已配置则自动处理；未配置则提示手动。
echo "--- 4/7 DNSHe NS 委派 ---"
if [ -n "${DNSHE_API_KEY:-}" ] && [ -n "${DNSHE_API_SECRET:-}" ]; then
    DNSHE_BASE="${DNSHE_API_BASE:-https://api005.dnshe.com}"
    DNSHE_AUTH=(-H "X-API-Key: $DNSHE_API_KEY" -H "X-API-Secret: $DNSHE_API_SECRET")
    # 提取子域前缀（paygateway.de5.net → paygateway）
    SUB_PREFIX="${CF_TUNNEL_HOSTNAME%%.*}"
    ROOT_DOMAIN="${CF_TUNNEL_HOSTNAME#*.}"
    echo "  子域: $SUB_PREFIX (根域: $ROOT_DOMAIN)"
    # 列出子域名，找 subdomain_id
    SUB_ID=$(curl -sS -m 15 "${DNSHE_AUTH[@]}" \
        "https://${DNSHE_BASE#https://}/index.php?m=domain_hub&endpoint=subdomains&action=list" 2>/dev/null | \
        python3 -c "import sys,json; d=json.load(sys.stdin); r=[x for x in d.get('subdomains',[]) if x['full_domain']=='$CF_TUNNEL_HOSTNAME']; print(r[0]['id'] if r else '')" 2>/dev/null || echo "")
    if [ -z "$SUB_ID" ]; then
        # 子域名不存在 → 自动注册（DNSHe API 支持）
        echo "  子域名 $CF_TUNNEL_HOSTNAME 不存在，自动注册..."
        SUB_ID=$(curl -sS -m 20 -X POST "${DNSHE_AUTH[@]}" \
            "https://${DNSHE_BASE#https://}/index.php?m=domain_hub&endpoint=subdomains&action=create" \
            -H "Content-Type: application/json" \
            -d "{\"subdomain\":\"$SUB_PREFIX\",\"domain\":\"$ROOT_DOMAIN\"}" 2>/dev/null | \
            python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('subdomain_id',''))" 2>/dev/null || echo "")
        if [ -n "$SUB_ID" ]; then
            echo "  ✅ 子域名已注册: $CF_TUNNEL_HOSTNAME (id=$SUB_ID)"
        else
            echo "  ❌ 子域名注册失败（配额可能已满，DNSHe 免费额度 3 个）"
        fi
    fi
    if [ -n "$SUB_ID" ]; then
        # 取 Cloudflare 分配的 NS（kevin/reza.ns.cloudflare.com）
        CF_NS=$(curl -sS -m 15 "https://api.cloudflare.com/client/v4/zones/$ZONE_ID" "${CF_AUTH[@]}" 2>/dev/null | \
            python3 -c "import sys,json; r=json.load(sys.stdin).get('result',{}); ns=r.get('name_servers',[]); print(' '.join(ns) if ns else '')" 2>/dev/null || echo "")
        if [ -z "$CF_NS" ]; then
            echo "  ⚠️ 无法获取 Cloudflare NS，跳过 DNSHe 自动委派"
        else
            echo "  Cloudflare NS: $CF_NS"
            # 检查当前 NS 记录是否已是 Cloudflare 的
            NS_CHECK=$(curl -sS -m 15 "${DNSHE_AUTH[@]}" \
                "https://${DNSHE_BASE#https://}/index.php?m=domain_hub&endpoint=dns_records&action=list&subdomain_id=$SUB_ID" 2>/dev/null | \
                python3 -c "
import sys,json
d=json.load(sys.stdin)
ns1,ns2='$CF_NS'.split()[:2]
ok=all(any(r.get('type')=='NS' and r.get('content')==ns for r in d.get('records',[])) for ns in (ns1,ns2))
print('ok' if ok else 'need')
" 2>/dev/null || echo "need")
            if [ "$NS_CHECK" = "ok" ]; then
                echo "  ✅ NS 委派已就绪 ($CF_NS)"
            else
                echo "  NS 委派缺失，自动配置中..."
                # 删除现有 CNAME/非 NS 记录（NS 委派与 CNAME 冲突）
                curl -sS -m 15 "${DNSHE_AUTH[@]}" \
                    "https://${DNSHE_BASE#https://}/index.php?m=domain_hub&endpoint=dns_records&action=list&subdomain_id=$SUB_ID" 2>/dev/null | \
                    python3 -c "
import sys,json
d=json.load(sys.stdin)
for r in d.get('records',[]):
    print(r.get('id'))
" 2>/dev/null | while read rid; do
                    curl -sS -m 15 -X POST "${DNSHE_AUTH[@]}" \
                        "https://${DNSHE_BASE#https://}/index.php?m=domain_hub&endpoint=dns_records&action=delete" \
                        -H "Content-Type: application/json" \
                        -d "{\"record_id\":\"$rid\"}" > /dev/null 2>&1
                done
                # 添加两条 NS 记录
                for ns in $CF_NS; do
                    curl -sS -m 20 -X POST "${DNSHE_AUTH[@]}" \
                        "https://${DNSHE_BASE#https://}/index.php?m=domain_hub&endpoint=dns_records&action=create" \
                        -H "Content-Type: application/json" \
                        -d "{\"subdomain_id\": $SUB_ID, \"type\": \"NS\", \"content\": \"$ns\", \"ttl\": 600}" > /dev/null 2>&1
                done
                echo "  ✅ NS 委派已配置: $CF_NS"
            fi
        fi
    else
        echo "  ⚠️ DNSHe 未找到子域名 $CF_TUNNEL_HOSTNAME，跳过（请确认已注册）"
    fi
else
    NS_HINT=$(curl -sS -m 10 "https://api.cloudflare.com/client/v4/zones/$ZONE_ID" "${CF_AUTH[@]}" 2>/dev/null | \
        python3 -c "import sys,json; print(' '.join(json.load(sys.stdin).get('result',{}).get('name_servers',[])))" 2>/dev/null || echo "")
    echo "  ⚠️ 未配置 DNSHE_API_KEY/SECRET，跳过自动委派"
    echo "  如需手动：dns.he.net → $CF_TUNNEL_HOSTNAME → NS 指向 $NS_HINT"
fi

# 5. 确保 DNS 委派传播 + zone active
echo "--- 5/7 等待 zone 激活 (最多 60s) ---"
for i in $(seq 1 12); do
    STATUS=$(curl -sS -m 10 "https://api.cloudflare.com/client/v4/zones/$ZONE_ID" "${CF_AUTH[@]}" | \
        python3 -c "import sys,json; print(json.load(sys.stdin)['result']['status'])" 2>/dev/null || echo "pending")
    if [ "$STATUS" = "active" ]; then
        echo "  ✅ zone 已激活"
        break
    fi
    sleep 5
done

# 6. 确保 Universal SSL
echo "--- 6/7 检查 Universal SSL ---"
CERT=$(curl -sS -m 15 "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/ssl/certificate_packs?status=all" "${CF_AUTH[@]}" | \
    python3 -c "import sys,json; d=json.load(sys.stdin); r=[x for x in d.get('result',[]) if x.get('type')=='universal' and x.get('status')=='active']; print('ok' if r else '')" 2>/dev/null || echo "")
if [ -z "$CERT" ]; then
    echo "  SSL 未签发，尝试 toggle 触发..."
    curl -sS -m 10 -X PATCH "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/ssl/universal/settings" \
        "${CF_AUTH[@]}" -d '{"enabled":false}' > /dev/null 2>&1
    sleep 10
    curl -sS -m 10 -X PATCH "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/ssl/universal/settings" \
        "${CF_AUTH[@]}" -d '{"enabled":true}' > /dev/null 2>&1
    echo "  ⏳ Universal SSL toggle 完成，等待签发（通常 1-5 分钟）..."
    for i in $(seq 1 30); do
        STATUS=$(curl -sS -m 10 "https://api.cloudflare.com/client/v4/zones/$ZONE_ID/ssl/certificate_packs?status=all" "${CF_AUTH[@]}" | \
            python3 -c "import sys,json; d=json.load(sys.stdin); r=[x for x in d.get('result',[]) if x.get('status')=='active']; print(r[0]['status'] if r else 'pending')" 2>/dev/null || echo "pending")
        if [ "$STATUS" = "active" ]; then
            echo "  ✅ Universal SSL 已签发!"
            break
        fi
        sleep 10
    done
else
    echo "  ✅ Universal SSL 已签发"
fi

# 7. 验证 HTTPS
echo "--- 7/7 验证 HTTPS ---"
sleep 5
HTTP_CODE=$(curl -sS -m 15 -o /dev/null -w "%{http_code}" "https://$CF_TUNNEL_HOSTNAME/health" 2>/dev/null || echo "000")
if [ "$HTTP_CODE" = "200" ]; then
    echo "  ✅ https://$CF_TUNNEL_HOSTNAME/health → HTTP 200"
    echo ""
    echo "============================================"
    echo " 初始化完成! 固定域名已就绪"
    echo "   URL: https://$CF_TUNNEL_HOSTNAME"
    echo "   隧道: $TUNNEL_ID"
    echo "============================================"
else
    echo "  ⚠️ HTTPS 返回 HTTP $HTTP_CODE，可能需等待 DNS 传播"
    echo "  隧道进程需手动确认: ps aux | grep cloudflared"
fi
