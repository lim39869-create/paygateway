<p align="center">
  <img src="assets/logo/paygateway-wordmark.svg" alt="PayGateway" width="424" height="96" />
</p>

<p align="center">可扩展、可验证、面向授权业务集成的 Go 支付网关</p>

<p align="center">
  <a href="https://github.com/lim39869-create/paygateway/actions/workflows/ci.yml"><img src="https://github.com/lim39869-create/paygateway/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go" alt="Go 1.25+" />
  <img src="https://img.shields.io/badge/License-MIT-blue" alt="MIT License" />
  <a href="https://goreportcard.com/report/github.com/lim39869-create/paygateway"><img src="https://goreportcard.com/badge/github.com/lim39869-create/paygateway" alt="Go Report Card" /></a>
</p>

# PayGateway 支付网关

一个面向学习和授权业务集成的 Go 支付网关参考实现，兼容易支付（epay）协议，支持插件化商家通道、订单编排、可验证账本、异常对账和弹性调度。仓库同时保留设备自动化相关的通用工程实现，便于研究设备访问、事件处理和故障恢复。

> 详细设计见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)（架构）、[`docs/MIANQIAN.md`](docs/MIANQIAN.md)（设备自动化概览）、[`docs/RESILIENCE.md`](docs/RESILIENCE.md)（弹性/对账）和 [`docs/PLAN-DYNAMIC-CLOSE-ALLOC.md`](docs/PLAN-DYNAMIC-CLOSE-ALLOC.md)（动态调度）。

## 项目定位

`paygateway` 不是一个只展示接口调用的 Demo，而是一个经过真实业务验证的支付与账务基础设施参考实现。项目重点不在于堆叠支付渠道数量，而在于把以下问题放进同一套可审查的工程模型中：

- 支付通道可插拔，核心订单流程不依赖单一渠道；
- 订单状态、回调、补账和异常处理具备明确的状态边界；
- 资金事件进入可验证的追加式账本，历史记录的静默修改可以被发现；
- 管理后台、日志、备份、告警和测试构成完整的运行闭环；
- 设备自动化适配器与订单、账本和异常池隔离，结果不明确时默认人工核查。

项目当前已验证支付宝、微信相关商家通道和设备事件适配能力。国际支付、数字资产支付及其他地区通道属于可扩展方向，尚不代表仓库已经提供对应的生产适配器。

## 品牌资源

Logo 和 favicon 位于 [`assets/logo/`](assets/logo/)，可用于项目主页、技术博客、后台界面和社区头像。标识的视觉语义是“插件化通道、统一编排和可验证账本链”。

## 生产参考部署

`paygateway` 当前用于作者运营的 [FlashXAPI](https://flashxapi.com) 服务，承载真实的充值、订单、余额和账务流程。FlashXAPI 是作者自己的生产参考部署，不是独立第三方测评，也不是使用本项目的前置条件。

这意味着仓库中的订单、回调、账本和异常处理设计并非只在示例测试中存在，而是在实际运行环境中持续验证。生产部署中的用户数据、支付凭证、渠道信息和运行日志不会随仓库公开。

需要模型 API 的开发者可以单独了解 FlashXAPI；只希望研究或自部署支付网关的用户可以完全忽略该服务，直接使用本项目的开源代码。

## 账务审计模型

项目的账本使用哈希链、ECDSA 签名、独立存储和数据库层约束，目标是让资金事件可追溯，并在历史记录被静默修改或删除时提供检测依据。

这里的“防篡改”是技术语义：正常业务流程下账本只追加，历史记录的异常变化可被检测。它不等同于绝对不可攻击，也不替代会计凭证、税务申报、司法鉴定或专业审计意见。签名密钥、运行主机、备份和访问权限仍需由部署者独立保护。

## 为什么持续维护需要收入

本项目由个人维护，代码免费开源，不设置付费墙。持续维护、测试、生产运行和问题响应都需要时间与基础设施成本。作者通过自己运营的 FlashXAPI 服务获得维护收入，而不是向开源用户募捐或对源码设置获取门槛。

FlashXAPI 的生产使用关系会在项目发布、博客文章和技术实践记录中保持透明。服务可用性、模型能力和价格属于独立的服务承诺，不应被理解为本仓库对任何第三方渠道的保证。

## 社区与实践记录

项目后续会通过配套博客发布架构设计、支付异常复盘、账务审计实践和版本更新记录；社区频道用于维护公告、问题交流和使用反馈。公开内容会优先讨论可复用的工程问题，不公开生产密钥、真实订单、用户请求、渠道来源或未经授权的账户操作细节。

- **Telegram 频道**：[@paygtway](https://t.me/paygtway) — 项目公告、版本更新和实践文章
- **Telegram 群组**：[加入群组](https://t.me/+TyBkhTb5HT0xNDFl) — 使用交流、问题反馈和社区讨论

如果你在实际部署中发现回调、账务、并发、备份或文档问题，欢迎通过 GitHub Issue 提供最小复现。请先阅读 [`SECURITY.md`](SECURITY.md)，不要在公开 Issue 中提交密钥、支付凭证、个人数据或生产地址。

---

## ✨ 功能特性

### 商家通道（官方 API）
- **支付宝**：PC/手机网站支付、当面付扫码（gopay v1.5.119）
- **微信支付**：Native/JSAPI（预留扩展）
- 多商户（上游池）：每上游独立 `pid + 密钥`，下单/回调按 pid 路由验签
- 回调验签（RSA2 / HMAC-SHA256）、查单补账、超时关单

### 设备自动化实验适配器（可选）
- 设备访问、事件读取、状态机、调度和健康检查采用独立包封装
- 事件经来源认证、时间窗口、金额、状态和幂等校验后才可影响订单
- 设备离线、解析失败和状态冲突进入异常池，不自动确认资金结果
- 具体平台适配和授权边界见 [`docs/MIANQIAN.md`](docs/MIANQIAN.md)；文档不提供第三方账户操作或绕过安全控制的步骤

### 基础设施
- **账户池**：多账户轮询调度、日限额、连续失败熔断、手动/自动冻结，密钥 AES-256-GCM 加密入库
- **审计账本**：哈希链 + ECDSA 签名（`ledger.db`），每日快照，防篡改可验证
- **异常池**：金额不匹配 / 监听端离线 / 状态冲突 → 人工核查，绝不自动入账
- **不漏单保护**：个人通道关单前必查池内动态 `CloseAt` + 监听端离线检查——离线时订单进异常池人工核实（`LISTEN_OFFLINE`），绝不直接关单；商家通道关单前必查单（已支付→补账，查单失败→跳过）
- **管理后台**：Vue3 + Element Plus 单页（embed 进二进制），仪表盘/通道/订单/账户/上游/对账/账本/日志
- **告警**：Bark / Webhook（掉线、账本异常、风控）

---

## 🚀 快速开始

### 0. 两分钟最小体验（本地跑通，不接真实支付）

```bash
git clone https://github.com/lim39869-create/paygateway.git && cd paygateway
cp .env.example .env
# 生成必备主密钥并覆盖示例值
sed -i "s/^ACCOUNT_MASTER_KEY=.*/ACCOUNT_MASTER_KEY=$(openssl rand -hex 32)/" .env
go build -o gateway ./cmd/gateway/ && ./gateway
# 打开 http://127.0.0.1:8081 → 首次访问自动进入管理员注册页
```

只启动了网关和后台，订单/账本/后台全部可用；真实收款需要另行配置支付通道密钥（见下文第 4 步起）。

### 1. 编译

```bash
# Go 1.25+；隧道功能运行时需要系统 cloudflared
go build -o gateway ./cmd/gateway/

# 可选：重新构建前端（需要 node 18+）
cd web/admin && npm install && npm run build
cd ../.. && go build -o gateway ./cmd/gateway/
```

### 2. 配置 `.env`

```bash
cp .env.example .env && vim .env
```

`ACCOUNT_MASTER_KEY` 必须替换（`openssl rand -hex 32` 生成）。管理员账号不再配置在 .env——首次启动访问管理后台会自动进入注册页，凭据 bcrypt 哈希存主数据库 settings 表。

| 变量 | 必填 | 默认 | 说明 |
|------|------|------|------|
| `GATEWAY_ADDR` | 是 | `:8080` | 网关监听地址（易支付 /mq、套码 /qr 等） |
| `ADMIN_ADDR` | 否 | `:8081` | 管理后台监听（Vue3 SPA + REST + WS） |
| `JWT_SECRET` | 建议 | 空 | 留空 = 每次重启随机生成（重启后全员被踢下线）；管理员凭据存主数据库 settings 表（bcrypt），首次启动自动引导注册 |
| `ACCOUNT_MASTER_KEY` | 必填 | 空 | 账户 / 上游密钥加密主密钥。**缺失则服务拒绝启动**；首次用 `openssl rand -hex 32` 生成；**永不能改**（改 = accounts.db / upstreams.db 全部密文无法解密） |
| `LEDGER_PRIVATE_KEY` | 建议 | 空 | 账本 ECDSA 私钥（PEM）；留空 = 首次启动自动生成并持久化到 settings；更改则旧账本条目验签失败 |
| `QR_TOKEN_SECRET` | 个人免签建议 | 空 | 套码 token 签名密钥；留空 = 首次启动自动生成并持久化到 settings；更改则旧套码失效 |
| `ORDER_DB` | 否 | `data/paygateway.db` | 业务库路径（订单/异常池/回调队列/settings） |
| `LOG_FILE` | 否 | `logs/paygateway.log` | 日志文件（slog JSON + lumberjack：50MB×10 份×30 天 压缩） |
| `LOG_LEVEL` | 否 | `info` | `debug` / `info` / `warn` / `error` |
| `ORDER_CLOSE_MINUTES` | 否 | `30` | **商家通道**未支付订单自动关闭分钟数（0=不自动关）；商家走官方 API 可查单，固定窗口即可 |
| `ORDER_CLOSE_MIN` | 否 | `5` | 个人免签**动态关单窗口下限**（分钟）；0 或 ≥MAX → 退化为静态窗口 |
| `ORDER_CLOSE_MAX` | 否 | `10` | 个人免签**动态关单窗口上限**（分钟）；超过转人工异常池升级处理，不无限挂起 |
| `ORDER_CLOSE_INTERVAL` | 否 | `30` | 关单扫描间隔秒数（动态窗口下建议 30s 起） |
| `DEVICE_LOAD_CAP` | 否 | `0` | 单设备 WAIT_PAY 订单承载上限（0=按公式 `(MAX-60s)/poll成本` 自动算≈135）；超限设备被均衡分配跳过 |
| `DEVICE_OVERLOAD_RATIO` | 否 | `0.8` | 设备降权阈值（占 CAP 比例）：负载进入缓冲带后分配降权，避免临界抖动 |
| `DEVICE_POLL_COST_MS` | 否 | `4000` | 单次 poll 成本估算（ms），动态窗口与设备能力计算用 |
| `EPAY_PUBLIC_BASE_URL` | 否 | 自动 | 网关对外地址（用于通知注册）；同机 `http://127.0.0.1:8080`，公网填隧道 / 域名 |
| `PERSONAL_BASE_URL` | 否 | =EPAY | 个人免签监听端可达地址 |
| `ALIPAY_NOTIFY_URL` / `WECHAT_NOTIFY_URL` | 否 | `http://localhost:8080/...` | 通道回调地址，一般无需手填（隧道会覆盖） |
| `PERSONAL_MAX_AMOUNT` | 否 | `50000` | 个人免签单笔限额（分；50000 = 500 元；0 = 不限） |
| `PERSONAL_BIG_AMOUNT` | 否 | `20000` | 大额告警阈值（分；超此触发 Bark / Webhook） |
| `PERSONAL_MAX_PER_MIN` | 否 | `30` | 单设备每分钟收款推送上限（防监听端异常刷消息） |
| `ALERT_BARK_URL` | 否 | 空 | Bark 推送 URL（`https://api.day.app/<key>`），空 = 禁用 |
| `ALERT_WEBHOOK_URL` | 否 | 空 | 通用 Webhook URL（POST JSON：掉线 / 账本异常 / 风控文案检测） |
| `PUBLIC_RATE_MAX` | 否 | `60` | 公网接口滑动窗口内单 IP 最大请求数（仅覆盖 `/submit.php` `/api.php` `/qr/refresh` `/mq/:token`），0 = 禁用 |
| `PUBLIC_RATE_WINDOW_SEC` | 否 | `60` | 上一项的滑动窗口秒数，0 = 禁用 |

> ⚠️ **密钥三件套（`ACCOUNT_MASTER_KEY` / `LEDGER_PRIVATE_KEY` / `QR_TOKEN_SECRET`）更换/丢失的影响各不同，详见 `.env.example` 头部说明**。账户 / 上游密钥一律走管理后台，不在 `.env` 里。

### 3. 启动

**推荐：systemd 守护**

```bash
sudo cp deploy/paygateway.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now paygateway
journalctl -u paygateway -f        # 实时日志
sudo systemctl {status,restart,stop} paygateway
```

**裸跑：项目脚本（pid 文件 + 防双启）**

```bash
./start.sh          # 后台启动，写 ./.gateway.pid，重复运行会拒绝
./status.sh         # 查看运行状态
./stop.sh           # 优雅停止（SIGTERM → 10s 后 SIGKILL）
./run.sh            # 前台调试运行（go run，不读 .gateway.pid）
tail -f logs/paygateway.log
```

> 启动后看 `logs/paygateway.log` 的 `=== 启动配置自检 ===` 段：`0 错误` 才算就绪。

### 4. 配置账户（首次必做）

打开 `http://127.0.0.1:8081` 登录：

1. **上游对接** → 新增上游（pid + 密钥），否则下单会被拒绝
2. **账户池** → 按需新增：
   - 商家通道：填 AppID + 私钥（`alipay` / `wechat`）
   - 设备事件适配器：仅在获得授权的测试环境中配置

---

## 📡 下单 API（epay 协议兼容）

```bash
# 参数（MD5 签名，排除 sign/sign_type/pay_type）
curl "http://127.0.0.1:8080/submit.php?pid=1001&type=alipay&out_trade_no=TEST001&name=商品&money=10.00&notify_url=https://your.com/notify&return_url=https://your.com/return&sign=MD5签名&sign_type=MD5"
```

- `type`：`alipay`（个人优先→商家回退）、`wxpay`、`personal_alipay`（强制个人）
- 返回：支付页（个人免签=套码二维码+备注码提示；商家=官方二维码/跳转）
- 异步通知：`trade_status=TRADE_SUCCESS` + MD5 签名
- 查单：`/api.php?pid=..&out_trade_no=..&sign=..`

---

## 🔒 安全设计

- 回调验签（商家 RSA2 / 个人 HMAC-SHA256）+ 时间戳防重放（±5min）
- 金额校验（回调金额 ≠ 订单金额 → 拒绝入账 + 异常池）
- 状态机条件更新（SQL `WHERE status=?` 互斥，防并发双写）
- 账本哈希链 + ECDSA 签名，篡改可检测
- 敏感密钥 AES-256-GCM 加密入库，仅内存明文
- JWT 管理后台鉴权（随机密钥，禁可预测）
- **后台任务 panic 自恢复**：全部后台 goroutine（关单扫描/收款码维护/账本日任务/隧道/告警等）带 `defer recover`，单点异常不拖垮进程，panic + 栈记录日志便于定位

---

## 🧪 测试

```bash
go vet ./...
go test ./...
go test -race ./internal/...          # 全包并发安全验证

# 授权设备端到端测试（需要自行准备测试设备）
bash scripts/e2e_usb_realdevice.sh
```

覆盖：设备池隔离、动态调度、离线安全降级、通道窗口、均衡分配、设备健康埋点、事件匹配和账本哈希链。

---

## 📁 目录结构

```
cmd/gateway/          主程序（路由/启动/配置/自检/动态关单与均衡分配装配）
internal/
  account/            账户池（调度/加密/风控/加权分配）
  adbmgr/             adb 设备管理 / 配置下发
  admin/              管理后台 API + embed 前端
  alert/              告警客户端（Bark/Webhook）
  channel/            支付通道抽象 + 实现（alipay/wechat/personal）
  epay/               epay 协议层（签名/参数）
  ledger/             审计账本（哈希链/快照）
  ledgermon/          ADB 监听编排（设备健康埋点/公平调度/账本识别）
  model/              领域模型
  notify/             通知发送 worker
  notifywatcher/      通知栏事件驱动监听（比账本快 ~15s）
  order/              订单编排（状态机/幂等/结算/安全关单）
  personal/           个人免签（备注码/签名/风控）
  pool/               WAIT_PAY 设备维度池（动态关单窗口/负载快照/CloseAt）
  ratelimit/          公网接口 IP 限流
  reconciler/         确认调度器/对账/异常池
  scrcpy/             真机投屏（adb forward）
  store/              SQLite 存储层
  tunnel/             Cloudflare 隧道
  upstream/           上游池（多商户）
  web/                支付页面生成
deploy/               systemd unit + 每日 SQLite 备份脚本
scripts/              运维 / E2E 脚本
web/admin/            管理后台前端源码（Vue3）
docs/                 设计文档
.env.example          配置示例（cp 为 .env 修改）
start.sh              裸跑启动（带 pid 文件 / 防双启）
stop.sh / status.sh   裸跑对应停止 / 状态查询
```

---

## 📚 相关文档

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — 系统架构（协议/编排/通道三层）
- [`docs/PLAN-DYNAMIC-CLOSE-ALLOC.md`](docs/PLAN-DYNAMIC-CLOSE-ALLOC.md) — 动态关单窗口 + 设备/账户均衡分配设计与实现基线
- [`docs/MIANQIAN.md`](docs/MIANQIAN.md) — 设备自动化与个人通道的学习概览
- [`docs/RESILIENCE.md`](docs/RESILIENCE.md) — 弹性与对账（崩溃恢复/异常池/每日快照）
- [`docs/OPERATIONS.md`](docs/OPERATIONS.md) — 运维手册（升级/回滚/备份恢复/密钥轮换/常见故障/健康检查）
- [`docs/DESIGN-DEVICE-ACTOR.md`](docs/DESIGN-DEVICE-ACTOR.md) — ADB 设备操作串行化（DeviceActor 模型）
- [`docs/DESIGN-FAIR-SCHEDULER.md`](docs/DESIGN-FAIR-SCHEDULER.md) — 单设备多账户公平时间占比调度

## 📌 注意

- 本项目仅用于学习、授权测试和合法的商户支付集成。部署者必须自行确认支付合规、平台条款、隐私保护和数据安全要求。
- 设备自动化适配器不是第三方平台授权的替代品，不得用于未经授权访问账户、绕过安全控制或其他违法活动。
- 账本提供防篡改检测和可追溯性，不等同于法律、税务或司法意义上的审计结论。
