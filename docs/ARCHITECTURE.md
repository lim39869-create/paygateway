# PayGateway 架构设计（最终版）

> 目标：高性能、高稳定、资金零差错、**支付方式可插拔**的支付网关
> 通道底座：**gopay v1.5.119**（9 种通道现成：支付宝/微信/抖音/QQ/通联/拉卡拉/PayPal/Apple/扫呗）
> 未来扩展：链上支付（crypto）→ **预留扩展位**，不现在实现

---

## 1. 核心思想：三层各司其职，不重复造轮子

```
┌────────────────────────────────────────────────────────────┐
│ ① 协议层（对外接口，永不改变）                               │
│    epay: /submit.php  /api.php   ← New API 等易支付系       │
│    REST: /v1/orders              ← 未来其他项目（预留）      │
│    type=alipay|wxpay|paypal|usdt → 路由到对应通道            │
├────────────────────────────────────────────────────────────┤
│ ② 编排层（资金安全核心，与具体通道无关）                      │
│    订单状态机 / 金额校验 / 幂等 / 可靠回调(outbox) / 对账     │
│    → 只认统一接口 PaymentChannel，不认任何 SDK 细节          │
├────────────────────────────────────────────────────────────┤
│ ③ 通道层（可插拔，每个通道一个包）                           │
│    ┌─────────────── gopay 库（现成 9 通道）──────────────┐  │
│    │ alipay / wechat / paypal / douyin / qq / allinpay  │  │
│    │ lakala / saobei / apple                            │  │
│    └────────────────────────────────────────────────────┘  │
│    crypto/（★ 预留目录：未来链上支付，接口已留好）            │
└────────────────────────────────────────────────────────────┘
```

**职责划分：**
- **gopay** = 通道适配器工厂（把 9 种支付 SDK 的差异封装掉）
- **paygateway** = 协议翻译 + 资金安全编排（不重复实现支付 SDK，只做编排）
- **crypto** = 未来加一个目录，接口现成

---

## 2. 通道抽象（接口 + 注册表）

### 2.1 统一接口

```go
package channel

// PaymentChannel 所有支付通道必须实现的统一接口
type PaymentChannel interface {
    // Name 通道标识：alipay / wechat / paypal / usdt ...
    Name() string

    // Kind 通道类型：回调驱动 or 监听驱动（链上）
    Kind() ChannelKind // CallbackKind / ListenerKind

    // Modes 支持的支付模式
    Modes() []PayMode // Page / Wap / QRCode / Address

    // CreatePayment 下单：返回支付凭据（跳转链接/二维码/链上地址）
    CreatePayment(ctx context.Context, req *PayRequest) (*PayCredential, error)

    // VerifyNotify 验签回调（CallbackKind 用）
    VerifyNotify(ctx context.Context, raw []byte) (*NotifyResult, error)

    // QueryOrder 主动查单（对账 + 补偿兜底）
    QueryOrder(ctx context.Context, outTradeNo string) (*OrderStatus, error)

    // Listener 事件监听（ListenerKind 必须实现；CallbackKind 返回 nil）
    Listener() EventListener
}

type PayRequest struct {
    OutTradeNo string  // 网关订单号
    Subject    string  // 商品名
    AmountMinor int64  // 金额（最小单位，法币=分，链上=wei/satoshi）
    Currency   string  // CNY / USD / USDT
    NotifyURL  string
    ReturnURL  string
    PayMode    PayMode // 用户选择的支付模式
}

type PayCredential struct {
    PayURL     string // 跳转链接
    QRCode     string // 二维码内容
    Address    string // 链上收款地址（未来）
    ExpireAt   time.Time
}

type NotifyResult struct {
    Valid       bool   // 验签是否通过
    ChannelTxID string // 通道交易号（去重依据）
    Status      string // SUCCESS / CLOSED / FAILED
    AmountMinor int64  // 实付金额（必须与订单比对！）
}
```

### 2.2 注册表（配置驱动，零代码路由）

```go
// 注册：每个通道 init() 或显式调用
func Register(c PaymentChannel) { registry[c.Name()] = c }

// 路由：协议层只写一行
ch, ok := channel.Get(c.Query("type")) // "alipay" → 支付宝通道
if !ok { c.String(400, "不支持的支付方式") }
```

**新增一种支付方式 = 3 步：**
1. `internal/channel/wechat/wechat.go` — 实现 4 个方法（内部调 gopay 的微信 SDK）
2. `Register(...)` 一行注册
3. 配置 `.env` 加微信的密钥

→ 协议层、订单核心、存储、页面 **零改动**。

### 2.3 crypto 预留位（未来链上）

```go
// internal/channel/crypto/ 目前只放：
//   README.md  — 设计说明（HD钱包/确认数/重组处理）
//   stub.go    — 返回 "未启用" 的占位实现（注册表可查但拒绝下单）

// 未来实现时：
//   Kind() = ListenerKind
//   Listener() = 区块监听器（WebSocket + 轮询兜底）
//   CreatePayment() = HD 钱包派生一次性地址
//   QueryOrder() = 查链上确认数
```

注册表已经支持 `ListenerKind` 通道，**未来 crypto 落地 = 填实现，不动架构**。

---

## 3. 资金安全编排层（最不能错的部分）

```
下单:  协议层 → order.Create() → ① 事务写订单(CREATED) → ② 调通道
       → ③ 事务更新支付凭据 → ④ 返回页面

支付:  通道事件（回调/监听）
       → ① 验签 → ② 金额比对（实付==订单，不等→拒绝+告警）
       → ③ 幂等检查（channel_tx_id 唯一，重复→直接应答成功）
       → ④ 条件更新状态（WHERE status='WAIT_PAY'，失败→拒绝）
       → ⑤ 写 payment_events 流水 → ⑥ 写 outbox 回调任务

结算:  outbox worker → 回调接入方（失败重试 1m/5m/15m/1h → dead 告警）
       定时补偿：PAID 且未通知 > 30s → 补发
       定时对账：通道实付 vs 订单金额
```

**5 条铁律**（写入代码评审清单）：
1. 金额只用最小单位 int64，永不用 float
2. 状态变更必须 `WHERE status=当前态`（防回跳/防覆盖）
3. 实付金额 ≠ 订单金额 → 拒绝 + 告警
4. 同一 channel_tx_id 重复事件 → 幂等返回成功
5. 每笔资金事件落 payment_events，可追溯

---

## 4. 数据库设计（资金安全地基，最关键的组件）

### 4.1 选型决策

| 阶段 | 数据库 | 理由 |
|------|--------|------|
| **P1 开发/单机生产** | **SQLite**（`modernc.org/sqlite` 纯 Go，无 CGO） | 本机已有；单文件零运维；事务/行锁/唯一约束完整；交易量几百 TPS 足够 |
| **正式生产/多机** | PostgreSQL | 用户已声明基础设施有；行级锁 + 强一致 + 可扩展 |

> **关键**：`Store` 接口抽象，SQLite 与 PG 实现可无缝切换。**P1 直接上 SQLite，不做 JSON 过渡** —— 资金数据从一开始就落在真数据库里。

### 4.2 数据模型（SQLite 版，PG 同构）

```sql
-- 订单表：唯一约束防重复下单，条件更新防覆盖
CREATE TABLE orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    out_trade_no    TEXT NOT NULL UNIQUE,   -- 接入方订单号（幂等依据）
    gateway_no      TEXT NOT NULL UNIQUE,   -- 网关订单号
    channel         TEXT NOT NULL,          -- alipay/wechat/paypal/usdt
    channel_tx_id   TEXT,                   -- 通道交易号（回调去重）
    amount_minor    INTEGER NOT NULL,       -- 金额（最小单位：分）
    currency        TEXT NOT NULL DEFAULT 'CNY',
    status          TEXT NOT NULL,          -- CREATED/WAIT_PAY/PAID/...
    payer           TEXT,                   -- 付款方标识
    pay_url         TEXT,                   -- 支付链接/二维码/链上地址
    return_url      TEXT,
    notify_url      TEXT,
    created_at      TEXT NOT NULL,
    paid_at         TEXT,
    notified_at     TEXT,
    UNIQUE (channel, channel_tx_id)         -- 防回调重放
);
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_created ON orders(created_at);
CREATE INDEX idx_orders_channel ON orders(channel);

-- 交易流水：资金审计，只增不改
CREATE TABLE payment_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id        INTEGER NOT NULL,
    event_type      TEXT NOT NULL,          -- created/paid/closed/failed/refunded
    channel_tx_id   TEXT,
    amount_minor    INTEGER NOT NULL,
    raw_payload     TEXT,                   -- 原始回调/区块数据 JSON
    created_at      TEXT NOT NULL
);
CREATE INDEX idx_events_order ON payment_events(order_id);

-- 可靠回调投递（Outbox）
CREATE TABLE notify_tasks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id        INTEGER NOT NULL,
    payload         TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',  -- pending/success/dead
    retry_count     INTEGER NOT NULL DEFAULT 0,
    next_retry_at   TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE INDEX idx_notify_status ON notify_tasks(status, next_retry_at);

-- 回调投递日志
CREATE TABLE callback_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     INTEGER NOT NULL,
    http_status INTEGER,
    response    TEXT,
    latency_ms  INTEGER,
    created_at  TEXT NOT NULL
);

-- 应用日志（管理界面查询用，与文件日志双写）
CREATE TABLE app_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    level       TEXT NOT NULL,              -- debug/info/warn/error
    scope       TEXT,                       -- 模块：order/channel/notify/scheduler
    message     TEXT NOT NULL,
    trace_id    TEXT,                       -- 请求追踪 ID
    fields      TEXT,                       -- 结构化字段 JSON
    created_at  TEXT NOT NULL
);
CREATE INDEX idx_logs_level_time ON app_logs(level, created_at);
CREATE INDEX idx_logs_scope_time ON app_logs(scope, created_at);
CREATE INDEX idx_logs_trace ON app_logs(trace_id);
```

### 4.3 资金安全约束（写库必须遵守）

```go
// 状态变更必须条件更新（防回跳/防覆盖）
UPDATE orders SET status='PAID', paid_at=?, channel_tx_id=?
WHERE out_trade_no=? AND status='WAIT_PAY'
// 影响行数 = 0 → 状态非法，拒绝（幂等/防覆盖双保险）

// 金额校验
实付金额(amount_minor) == 订单金额 → 才允许 PAID
不等 → 记录 payment_events(event_type='amount_mismatch') + 告警 + 拒绝

// 回调去重
INSERT INTO orders(channel, channel_tx_id) ... ON CONFLICT DO NOTHING
```

---

## 5. 交易不可篡改设计（资金审计账本）

> 目标：**每一笔交易都不可篡改** —— 任何人（包括管理员/DBA）改任何一条记录都能被检测。
> 方案：**哈希链 + 数字签名 + 只增不改 + 每日快照锚定**（企业级审计账本标准做法）。

### 5.1 哈希链（账本防篡改核心）

```
genesis(固定) → entry1 → entry2 → entry3 → ... → entryN
                   ↑        ↑        ↑
            prev_hash  prev_hash  prev_hash   ← 每条都链接前一条

entry_hash(i) = SHA256( prev_hash(i-1) + payload_hash(i) )
payload_hash(i) = SHA256( 事件全部内容: 订单号+金额+状态+通道交易号+时间+原始数据 )
```

**篡改任意一条 → 它后面的所有 entry_hash 全部失配 → 校验必失败。**
**数据库整体被替换 → 与每日快照锚点比对 → 发现。**

### 5.2 数字签名（防伪造 + 来源验证）

- 独立**审计密钥对**（与支付签名密钥物理分离），私钥存 env/密钥文件
- 每条 entry 的 `entry_hash` 用私钥 **ECDSA 签名**
- 任何人想改数据必须重新签名 → 没有私钥就无法伪造合法记录
- 验签失败 = 数据被改过或来源可疑 → 立即告警

### 5.3 数据库层硬约束（SQLite/PG 双实现）

```sql
-- 账本表：只追加，禁止修改删除（SQLite 触发器强制）
CREATE TABLE audit_ledger (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    prev_hash    TEXT NOT NULL,        -- 前一哈希（链）
    payload_hash TEXT NOT NULL,        -- 本条事件内容哈希
    entry_hash   TEXT NOT NULL,        -- SHA256(prev+payload)
    signature    TEXT NOT NULL,        -- ECDSA(entry_hash)
    event_id     INTEGER,              -- 关联 payment_events.id
    created_at   TEXT NOT NULL
);

-- 禁止 UPDATE / DELETE（数据库层防篡改）
CREATE TRIGGER trg_ledger_no_update BEFORE UPDATE ON audit_ledger
BEGIN SELECT RAISE(ABORT, 'audit_ledger is append-only'); END;
CREATE TRIGGER trg_ledger_no_delete BEFORE DELETE ON audit_ledger
BEGIN SELECT RAISE(ABORT, 'audit_ledger is append-only'); END;
```

> PG 版：DB 账号只授予 INSERT/SELECT 权限（无 UPDATE/DELETE），双保险。

### 5.4 每日快照（外部锚定点）

```sql
CREATE TABLE daily_snapshots (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    snapshot_date TEXT NOT NULL UNIQUE,
    last_hash     TEXT NOT NULL,   -- 当日最后 entry_hash（锚点）
    signature     TEXT NOT NULL,   -- 对锚点签名
    created_at    TEXT NOT NULL
);
```

- 每日 00:00 生成快照：当前链尾哈希 + 签名
- 可选增强：把快照哈希发布到外部（邮件/对象存储/链上）→ 防数据库整体替换
- **改任何历史数据 → 与快照锚点比对即发现**

### 5.5 完整性校验任务（scheduler 每日 + 手动）

```go
VerifyLedger() → 从 genesis（或上一快照）重算整条哈希链
               → 校验每条 ECDSA 签名
               → 比对 daily_snapshots 锚点
               → 全过 → INFO 记录"账本完整"
               → 任何失配 → ERROR + 管理后台红色告警（重大安全事件）
```

### 5.6 资金事件写入流程（唯一入口，防绕过）

```
通道回调/监听 → order服务校验通过
  → ① payment_events INSERT（业务库，只增）
  → ② audit_ledger.Append(事件内容)  ← 哈希链+签名（独立账本库 ledger.db）
  → ③ 账本写入失败 → 回滚订单状态 + 告警（资金事件必须有账本记录）
```

**所有资金变更只能走这一个入口**，不允许其他代码直接写事件表。
> 注：账本在独立数据库中，无法与业务库同事务；因此采用"业务库先记事件 → 账本库追加 → 失败则回滚订单状态并告警"的顺序，确保资金事件必有账本记录。

### 5.7 管理后台联动

| 功能 | 说明 |
|------|------|
| 账本状态 | 显示最后校验时间/结果/链长 |
| 手动校验 | 按钮触发全链校验 |
| 篡改告警 | 校验失败 → 顶部红色横幅 + 告警日志 |
| 事件详情 | 每条事件可看 payload_hash / signature / 原始数据 |

### 5.8 独立账本数据库（ledger.db，安全隔离）

> **架构升级**：账本从业务库中独立出来，单独一个 SQLite 文件，与订单/日志/回调完全隔离。

```
paygateway.db（业务库）          ledger.db（账本库 🛡️）
├── orders                     ├── audit_ledger
├── payment_events             └── daily_snapshots
├── notify_tasks
├── callback_logs
├── app_logs
└── settings
```

**隔离带来的安全提升：**

| 防护层 | 机制 | 效果 |
|--------|------|------|
| 文件隔离 | 账本独立 `ledger.db` | 删业务库不影响账本，审计线索永存 |
| 文件权限 | `chmod 0600` | 仅文件所有者可读写，其他用户/进程无法访问 |
| 触发器 | 两个表均有 UPDATE/DELETE 触发器 | 通过 SQL 无法修改/删除任何账本数据 |
| WAL 模式 | `PRAGMA journal_mode=WAL` | 崩溃安全，写入不丢数据 |
| FULL 同步 | `PRAGMA synchronous=FULL` | 每次写入等待落盘，防断电丢数据 |
| 连接隔离 | `SetMaxOpenConns(1)` | 单连接串行写，避免并发写入冲突 |

**实现方式：**

```go
// store.LedgerDB 独立账本存储（internal/store/ledgerdb.go）
ledgerDB, err := store.NewLedgerDB("ledger.db")  // 0600 + WAL + 触发器
lg, err := ledger.New(ledgerDB, cfg.LedgerPrivKey) // 账本只依赖独立库
// admin 同时注入业务库 + 账本库
adminSrv := admin.New(st, ledgerDB, ...)
```

### 5.9 管理后台 API 与前端

**账本相关 API：**

| 端点 | 说明 |
|------|------|
| `GET /api/ledger` | 账本条目列表（哈希链可视化） |
| `POST /api/ledger/verify` | 手动触发全链校验（哈希链 + 签名 + 快照锚点） |
| `GET /api/ledger/snapshots` | 每日快照列表 |
| `POST /api/ledger/snapshot` | 手动创建当日快照 |
| `GET /api/ledger/public-key` | 返回 ECDSA P256 公钥（供第三方验证） |
| `GET /api/ledger/export` | 导出账本 + 快照为 CSV（审计报告） |

**前端功能（Vue3 Ledger 页面）：**

- 📋 **账本条目**：哈希链可视化（每条含 prev_hash / entry_hash / 签名）
- 🔍 **校验账本**：一键全链校验，弹窗展示结果（通过/失败 + 错误明细 + 快照锚点状态）
- 📸 **快照管理**：卡片列表 + 详情弹窗 + **锚点核对**（对比快照链尾与当前链尾）
- 🔑 **账本公钥**：展示 PEM 公钥，可复制供外部审计验证
- ⬇️ **导出 CSV**：一键下载完整审计报告（含所有条目 + 快照）
- 🔗 **事件跳转**：点击事件 ID 跳转订单页搜索

**测试覆盖（internal/ledger/ledger_test.go）：**

| 测试 | 验证点 |
|------|--------|
| TestAppendAndVerify | 追加 + 全链校验通过 |
| TestHashChainLinked | 哈希链正确连接 |
| TestTamperDetected | 篡改条目 → 校验失败 |
| TestTamperSignatureDetected | 篡改内容 → 签名校验失败 |
| TestSnapshotCreateAndVerify | 快照创建 + 锚点匹配 |
| TestSnapshotAnchorMismatch | 数据库替换 → 锚点不匹配 |
| TestSignatureUnique | 防重放（同内容不同事件不同签名） |

---

## 6. 日志系统设计（健壮 + 可查询）

### 5.1 双写架构：文件（排障）+ 数据库（查询）

```
应用 → slog(结构化) → ① 文件轮转日志（/var/log/paygateway/*.log）
                   → ② 异步批量写 app_logs 表（管理界面查询）
```

### 5.2 技术选型

| 项 | 方案 |
|----|------|
| 日志库 | Go 标准库 **slog**（结构化，JSON 输出） |
| 轮转 | `gopkg.in/natefinch/lumberjack.v2`（按大小/天数切割，防磁盘爆） |
| 写库 | 异步批量（攒 50 条或 1s flush），避免日志拖慢交易主链路 |
| 级别 | debug / info / warn / error（env 可配） |
| 追踪 | 每个 HTTP 请求生成 `trace_id`（中间件），贯穿 下单→回调→通知 全链路 |

### 5.3 关键审计日志（钱相关的必须全记）

```go
// 资金事件：writeEvent(orderID, "paid", amountMinor, rawPayload)
//  - created / paid / closed / failed / amount_mismatch / refunded
// 回调事件：writeEvent(orderID, "notify_attempt", ..., httpStatus)
// 安全事件：writeEvent(orderID, "verify_failed", ..., 原因)  ← 验签失败必须告警
```

---

## 7. Web 管理后台设计

### 6.1 形态：同进程嵌入式（单二进制部署）

- 同一 Go 进程，路由组 `/admin/*`（端口可独立配置 `ADMIN_ADDR`）
- 内置 HTML/JS/CSS（`embed` 打包），无前端构建链
- **登录鉴权**：管理 token（`ADMIN_TOKEN` env）+ 可选 Basic Auth；生产必须启用
- **接口全部走 `/admin/api/*`**，返回 JSON

### 6.2 功能清单

| 页面 | 功能 |
|------|------|
| **仪表盘** | 今日订单量/交易额/成功率/待通知数/告警数（卡片 + 简易图表） |
| **订单管理** | 列表（状态/通道/金额/时间筛选，分页）→ 详情（状态机时间线 + 支付凭据 + 原始回调） |
| **交易流水** | payment_events 查询（按订单/事件类型/时间） |
| **日志查看** | app_logs 查询（级别/模块/trace_id 过滤，实时 tail） |
| **回调管理** | notify_tasks 列表；**手动补发**按钮（dead 任务重投）；重试状态 |
| **通道管理** | 已注册通道列表 + 启用/停用开关（配置驱动） |
| **对账报告** | 定时对账结果（订单 vs 通道实付） |
| **账本校验** | 哈希链完整性校验 + 篡改告警（红色横幅） |
| **系统状态** | 运行时长/goroutine/内存/最近错误 |

### 6.3 页面路由规划

```
GET  /admin/login                 # 登录页
GET  /admin/                      # 仪表盘（SPA，JS 拉 /admin/api/*）
GET  /admin/orders                # 订单页
GET  /admin/logs                  # 日志页
GET  /admin/notify                # 回调页
GET  /admin/api/dashboard         # JSON API
GET  /admin/api/orders?status=&channel=&page=
GET  /admin/api/orders/:no        # 订单详情（含事件流）
GET  /admin/api/logs?level=&scope=&trace=&page=
GET  /admin/api/notify/tasks?status=
POST /admin/api/notify/:id/retry  # 手动补发
GET  /admin/api/channels          # 通道状态
GET  /admin/api/health            # 系统状态
```

---

## 8. 目录结构（最终目标态）

```
paygateway/
├── cmd/gateway/main.go          # 入口：组装 config/store/registry/router
├── internal/
│   ├── config/                  # .env 加载 + 通道密钥管理
│   ├── model/                   # Order / 状态常量 / 金额类型
│   ├── logger/                  # slog 封装：文件轮转 + 异步写库 + trace_id
│   ├── epay/                    # 易支付协议（submit/api/notify 签名）
│   ├── rest/                    # REST API（预留）
│   ├── order/                   # 编排层：状态机/幂等/金额校验/对账
│   ├── channel/
│   │   ├── registry.go          # 注册表 + Get()
│   │   ├── interface.go         # PaymentChannel / PayRequest / ...
│   │   ├── alipay/              # 支付宝（gopay）——现有代码迁入
│   │   ├── wechat/              # 微信（gopay）——未来
│   │   ├── paypal/              # PayPal（gopay）——未来
│   │   └── crypto/              # ★ 预留：stub + README（未来链上）
│   ├── store/                   # Store 接口 + sqlite(默认)/pg 实现
│   │   ├── store.go             # Store 接口 + 数据模型
│   │   ├── sqlite.go            # 业务库 SQLite 实现（订单/事件/日志）
│   │   ├── sqlite_rest.go       # 业务库查询实现
│   │   └── ledgerdb.go          # ★ 独立账本库（ledger.db，0600 权限 + 触发器）
│   ├── notify/                  # outbox worker + 重试
│   ├── ledger/                  # ★ 资金审计账本（哈希链+签名+校验）
│   ├── scheduler/               # 关单/补偿/对账/账本校验定时任务
│   ├── admin/                   # ★ 管理后台：路由 + 页面(embed) + API
│   ├── web/                     # 二维码页/中转页渲染
│   └── metrics/                 # Prometheus（预留）
├── migrations/                  # SQL 迁移（sqlite/pg）
├── docs/ARCHITECTURE.md         # 本文档
└── go.mod
```

---

## 9. 实施计划（保持现有功能不回归）

| 阶段 | 内容 | 验收 |
|------|------|------|
| **P1 基础重建** | 目录分层 + Store 接口 + **SQLite 落地** + 支付宝迁入 `channel/alipay` + 日志系统（文件+DB） + **audit_ledger 哈希链** | 现有沙箱链路回归 ✅；订单/日志/账本入库 ✅ |
| **P2 管理后台** | `/admin` 页面（仪表盘/订单/日志/回调）+ API + token 鉴权 | 浏览器可查订单、日志、交易流水 |
| **P3 资金安全** | 状态机条件更新 + 金额校验 + 幂等 + outbox 回调重试 + **账本完整性校验任务** + **独立账本库 ledger.db** + **账本管理前端** | 模拟重复/篡改回调不产生错误；篡改一条记录 → 校验失败 ✅；账本独立库 + 0600 权限 + 触发器 ✅；前端校验/快照/公钥/导出 ✅ |
| **P4 通道扩展** | 用 gopay 接微信（验证"3 步加通道"流程） | 微信沙箱链路跑通 |
| **P5 crypto 预留** | crypto stub + README + 注册表支持 ListenerKind | `type=usdt` 返回"未启用" |
| **P6 生产化** | 按你们基础设施切 PG + 监控 + 对账 + 部署脚本 | 生产验收 |

---

## 10. 为什么这是最佳实践

1. **不重复造轮子**：9 种支付 SDK 由 gopay 维护，paygateway 只做编排 → 通道越多，收益越大
2. **加支付方式像插 U 盘**：新通道 = 新包 + 注册 + 配置，核心零改动
3. **协议层稳定**：易支付/REST 接口永不变 → 现有上游（New API）不受影响
4. **资金安全集中**：所有通道都过同一套状态机/金额校验/幂等 → 只维护一套安全逻辑
5. **链上预留**：接口已含 ListenerKind + Address 凭据 → 未来 crypto 落地不动架构
6. **存储可换**：SQLite(单机起步) → PG(生产)，`Store` 接口平滑切换
7. **可观测性内置**：日志双写（文件+DB）+ 管理后台 → 出问题能查、能补、能对账
8. **交易不可篡改**：哈希链 + 签名 + 只增不改 + 每日快照 → 任何篡改必被发现
