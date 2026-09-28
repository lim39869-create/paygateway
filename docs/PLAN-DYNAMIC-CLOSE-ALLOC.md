# 动态关单与均衡分配：设计与实现基线

> 本文档记录动态关单和资源均衡的设计基线，代码以实际实现为准。
> 关联：docs/DESIGN-FAIR-SCHEDULER.md（公平调度）、docs/MIANQIAN.md（设备自动化概览）、docs/RESILIENCE.md（对账兜底）
> 目标：**不漏单（不可商量底线）+ 高/低负载自适应 + 多设备负载均衡 + 性能/稳定性兼顾**
>
> 本文档覆盖两个模块的开发设计：
> - 模块一：WAIT_PAY 订单动态关单时间智能调度（含漏单路径修复）
> - 模块二：免签账户订单自动均衡分配（设备 × 账户两层）
> - 前置改造：WAIT_PAY 池从「全局单池」升级为「设备维度子池」

---

## 1. 现状代码事实（设计依据）

### 1.1 关单链路（两条路径，安全检查不一致 ⚠️）

**路径 A：`order.Service.CloseExpiredOrders`**（internal/order/service.go:821，main.go:738 每 60s 触发）

```go
for _, o := range expired {
    if ch, ok := channel.Lookup(o.Channel); ok && ch.ConfirmAbility() != channel.ConfirmByQuery {
        // 个人/监听通道：直接超时关单 —— 无离线检查、无最后复查 ⚠️
        if s.closeExpiredOrder(ctx, o, nil, "") { closed++ }
        continue
    }
    // 商家通道：关单前必查单 ...
}
```

**路径 B：`reconciler.Scheduler.confirmPersonal`**（internal/reconciler/scheduler.go:303）

```go
if s.personalCheck != nil {
    offline, reason := s.personalCheck(ctx, o)   // 有离线检查 ✅
    if offline {
        s.svc.EnqueueException(...)               // 进异常池，绝不直接关
        return
    }
}
// 监听端在线 + 无推送 + 未超复查阈值 → 才关单
```

**结论**：路径 A 对个人通道**直接关单**，绕过路径 B 的离线检查——**设备离线时用户已付款但未监听到，订单被硬关 = 钱收了单关了 = 漏单**。这是本设计首先要修的。

### 1.2 WAIT_PAY 池（internal/pool/waitpay_pool.go）

- 结构：`buckets[accountID][outTradeNo] *Entry` + `byNote` + `byAmount`，单 RWMutex，`atomic.Pointer[Snapshot]` 零锁快照
- `Entry.CloseAt = CreatedAt + closeWindow`（**固定 30min**，`NewWaitPayPool(closeWindowMinutes)` 传入）
- `Snapshot.Accounts[i] = {AccountID, Count, OldestAge, MinTTL}`（按账户聚合）
- `DeviceScheduler`（ledgermon/scheduler.go:170）每设备一个 goroutine，读**全局快照**再过滤本设备账户（`onDevice` 映射）
- 订单 → 账户 → 设备映射：订单只有 `AccountID`；设备绑定在 personal.db（`Device.AccountID` 主账户 + `DeviceAccountIDs` 附加账户）

### 1.3 选号链路（internal/channel/personal/personal.go:244）

```go
func (c *Channel) selectServableAccount(ctx) *account.Account {
    servable := c.servableAccountIDs(ctx)      // adb 在线设备的账户集（主+附加）
    return c.pool.SelectFrom(c.channelName(), "default", servable)  // 加权随机，无负载感知
}
```

- `AccountPool.SelectFrom`（account/pool.go:268）：`filterAvailable`（状态/日限额/冷却）→ priority 分组 → `weightedRandom`（Weight 加权）
- **没有任何负载维度**：不感知设备上有多少待确认订单，不感知账户池内积压

### 1.4 可用信号清单（无需新增即可用）

| 信号 | 来源 | 用途 |
|---|---|---|
| 设备 adb 在线 | `adbmgr.Online` 实时探测 | 关单前检查、分配硬过滤 |
| 每账户待确认数 | `WaitPayPool.Count(accountID)` | 账户负载 |
| 每账户池快照 | `Snapshot.Accounts[i]` | 负载/最久等待/MinTTL |
| 活跃账户数 | 池快照中 Count>0 的账户数 | 设备一轮耗时估算 |
| 账户状态/失败数/日限额 | `AccountPool`（normal/cooling/frozen/disabled） | 账户健康 |
| 单次 poll 成本 | 实测：支付宝≈4.3s、微信≈2.9s（dump 2.5s/OCR 0.7s） | 设备能力估算 |
| 调度器参数 | `BaseQuota=3, CapPct=0.6, DwellMax=3, UrgentTTL=8s` | 一轮耗时估算 |

---

## 2. 设计原则（不可商量）

1. **不漏单**：设备离线/未监听时绝不关单；关单前必须「最后复查」；宁可不关转人工，绝不误关。
2. **动态窗口有物理锚点**：下限 = 用户支付操作时间 + 一轮完整确认（5min）；上限 = 用户体验红线（10min），超过转人工升级处理，不是无限挂起。
3. **按设备负载伸缩，非全局统一**：窗口由订单所在设备的实时负载决定。
4. **设备是物理瓶颈**：分配先看设备（最闲最健康），再看设备内账户（最闲最健康）；只惩罚短板，不叠加长处。
5. **超限显式处理**：设备超限 → 换设备；全部超限 → 拒绝下单 + 日志告警建议加设备。
6. **已付款用户秒级到账**：窗口只影响「未支付订单」何时关，不影响确认链路（notify/ledger 照常秒级入账）。

---

## 3. 前置改造：WAIT_PAY 池 → 设备维度子池

### 3.1 数据结构

```go
// internal/pool/waitpay_pool.go —— 总池改造成设备维度管理

type WaitPayPool struct {
    mu       sync.RWMutex
    devices  map[string]*devicePool      // deviceID → 设备子池（新增）
    // 账户→设备 解析（main.go 注入；解析不到 → 归入 "" 兜底桶，Reconcile 修复）
    deviceOf func(accountID int64) (deviceID string, ok bool)
    snap     atomic.Pointer[Snapshot]    // 全局快照（notifywatcher 的 Total>0 判断用，保持）
    closeWindow time.Duration            // 仅作兜底（设备信息缺失时）
    winCalc  func(deviceLoad int) time.Duration // 动态窗口计算器（模块二注入；nil=固定窗口）
}

type devicePool struct {
    buckets  map[int64]map[string]*Entry
    byNote   map[int64]map[string]string
    byAmount map[int64]map[int64]map[string]struct{}
    // 设备级聚合（每子池独立快照，调度器/分配器零锁读）
    stat     atomic.Pointer[DeviceStat]
}

type DeviceStat struct {
    DeviceID    string
    Total       int               // 该设备 WAIT_PAY 总数（设备负载）
    Accounts    []AccountStat     // 该设备内按账户聚合（与现 AccountStat 同构）
    ActiveAccts int               // 活跃账户数
    EstRound    time.Duration     // 估算一轮耗时 = ActiveAccts × 配额 × pollCost
    CloseWindow time.Duration     // 当前动态窗口（模块二算好放这）
    UpdatedAt   time.Time
}
```

### 3.2 操作接口（保持对外语义不变，内部按设备分池）

| 现有方法 | 改造后行为 |
|---|---|
| `Add(o)` / `Upsert(o)` | 解析 `deviceID = deviceOf(o.AccountID)` → 写入对应子池；CloseAt 用**该设备当前动态窗口**计算 |
| `Remove(outTradeNo)` | 遍历子池删除（防御性全表，语义同现在） |
| `Snapshot()` | 全局聚合（Total = Σ 设备 Total），**保持**（notifywatcher / 调度器兼容） |
| `Count(accountID)` | 不变（按账户计数） |
| `MatchByNote / MatchByAmount` | 不变（账户维度匹配，跨设备天然隔离） |
| `Rebuild / Reconcile / ReconcileLoop` | 遍历时按 deviceOf 分桶；Reconcile 兜底：解析不到设备的订单归入 `""` 桶并告警 |
| **新增** `DeviceSnapshot(deviceID) *DeviceStat` | 设备级快照（零锁），供 DeviceScheduler 与分配器使用 |

### 3.3 关键改造点

1. **`orderToEntry`**：`CloseAt` 从固定 `closeWindow` 改为 `winCalc(deviceLoad)`——设备负载在 Add 时点计算。
2. **`publishSnapshotLocked`**：先按设备分桶聚合，再汇总全局；同时发布每子池 `DeviceStat`。
3. **`DeviceScheduler`**：`plan()` 改为读 `pool.DeviceSnapshot(s.deviceID)`（不再全局快照过滤），活跃账户判断与配额计算**只在本设备子池内进行** → 多设备天然隔离、互不干扰。
4. **notifywatcher**：`Snapshot().Total > 0` 判断不变（全局快照保持兼容）。
5. **注入 `deviceOf`**：main.go 构造（从 personal.db 设备 + `DeviceAccountIDs` 建 accountID→deviceID 映射；映射在设备增删时刷新）。

---

## 4. 模块一：动态关单时间算法

### 4.1 窗口公式（物理锚点）

```
设备负载 L     = 该设备子池 WAIT_PAY 订单总数
活跃账户数 A   = 该设备子池内 Count>0 的账户数
单次 poll 成本 P = 通道加权实测值（支付宝 4.3s / 微信 2.9s，默认取 4s 保守）
一轮耗时 T     = A × BaseQuota(3) × P          ≈ 12s × A
排队系数       = 1.5（最坏队尾位置）
支付操作预留   = 60s（用户扫码→支付操作时间下限）

动态窗口 W = clamp( T × 1.5 + 60s,  MIN=5min,  MAX=10min )
CloseAt    = CreatedAt + W
```

**单调性（防误关）**：`CloseAt` 在订单生命周期内**只增不减**。负载升高 → 允许推后；负载降低 → 不提前（保守，避免已排队确认的订单被提前关掉）。实现：Add 时记录 `CloseAt`，`RefreshCloseAt`（Upsert 触发）仅在 `newCloseAt > oldCloseAt` 时更新。

### 4.2 窗口取值表

| 设备状态 | L / A | T | W |
|---|---|---|---|
| 空闲（1 账户 1 单） | A=1 | 12s | **5min（下限）** |
| 正常（2 账户各有单） | A=2 | 24s | **5min** |
| 高峰（4 账户多单） | A=4 | 48s | **5~6min** |
| 高负载（6 账户积压） | A=6 | 72s | **~7min** |
| 接近超限 | A≥8 | ≥96s | **8~10min** |
| 超限 | L ≥ C | — | **10min（上限）→ 换设备** |

### 4.3 漏单路径修复（最高优先级，先行落地）

**改造 `CloseExpiredOrders`（order/service.go:821）**：

```go
// 个人/监听通道：不再直接关单，改为「复查后关」——与 confirmPersonal 同语义
if ch.ConfirmAbility() != channel.ConfirmByQuery {
    // ① 设备离线检查（注入 personalCheck，与 reconciler 共用）
    if s.personalCheck != nil {
        offline, reason := s.personalCheck(ctx, o)
        if offline {
            s.EnqueueException(ctx, o, ExReasonListenOffline, "LISTEN_OFFLINE", "", ...,
                "监听端掉线，无法确认是否已付款，需人工核实： " + reason)
            continue   // 不关单
        }
    }
    // ② 关单前最后复查（新增）：检查池子是否还有该订单 + 复查次数上限
    if !s.poolStillHolds(ctx, o.OutTradeNo) { continue }   // 已入账/已移除，跳过
    // ③ 复查次数超限 → 进异常池（复用 confirmPersonal 的 QueryFailAlert 语义）
    // ④ 确认未支付 → 关单（走 closeExpiredOrder，含池同步）
}
```

> 语义统一：个人通道关单**只有一条安全路径**（设备在线 + 无新记录 + 未超复查上限）。`CloseExpiredOrders` 是 DB 扫描兜底（防 confirmSched 堆丢失），`confirmPersonal` 是事件驱动主路径，两者最终都收敛到同一安全检查。

**新增 `personalCheck` 注入**：main.go 组装（`reconciler.AsPersonalCheck(personalDB, adbMgr())`），order.Service 与 reconciler.Scheduler 共用同一检查函数，避免两处逻辑分叉。

### 4.4 关单扫描改造

- 扫描器（main.go:738）：`before` 从 `now - 固定30min` 改为 `now - MAX_WINDOW(10min) - 裕量(1min)`，候选集放宽到上限内所有 WAIT_PAY。
- 命中候选后：在池子中查该订单的 `CloseAt`（动态），`now < CloseAt` → 跳过；`now ≥ CloseAt` → 走 §4.3 复查流程。
- `ORDER_CLOSE_INTERVAL` 从 60s 收紧到 **30s**（窗口变小，扫描粒度需跟上）。

### 4.5 设备超限处理（配合模块三）

```
设备能力 C = (MAX_WINDOW - 支付操作预留) / (单次poll × 每账户最低配额1) × 排队修正
           = (600s - 60s) / (4s × 1) ≈ 135 单（理论值，可按实测 poll 成本校准）

L ≥ C×80%   → 分配器降权该设备（模块三）
L ≥ C       → 分配器跳过该设备（换设备处理）
全部设备超限 → 下单返回「设备负载超限，暂无可服务设备」+ 日志 ERROR「设备负载超限，建议增加收款设备」
```

---

## 5. 模块二：免签账户订单自动均衡分配

### 5.1 两层分配模型

```
下单(个人通道 CreatePayment)
  → 第一层：选设备（最闲 + 最健康）
  → 第二层：该设备内选账户（最闲 + 最健康）
  → 现有 filterAvailable 安全过滤始终保留（状态/日限额/冷却）
```

### 5.2 第一层：设备评分（物理瓶颈优先）

```go
// internal/channel/personal/ 新增 deviceSelector（或 internal/pool 提供负载查询后在此实现）
type DeviceCandidate struct {
    DeviceID string
    Serial   string
    Load     int            // 设备子池 WAIT_PAY 总数（DeviceSnapshot().Total）
    Health   float64        // 最近 N 次 poll 成功率（埋点，见 §6）
}

score_device = Health × 1 / (Load + 1)

// 规则：
//  - 设备 adb 离线 → 排除（现有 servableAccountIDs 已过滤）
//  - Load ≥ C（超限）→ 排除（换设备）
//  - 其余按 score_device 降序选最优；得分并列 → 按 Load 升序；再并列 → 确定性（DeviceID 字典序）
```

**设备健康度埋点**（新增，§6.2）：`ledgermon.DeviceActor` 每次 PollCmd 执行记录成败，滑动窗口最近 20 次，`Health = 成功数/20`。设备无历史（<5 次）→ `Health = 0.9` 中性（不因无数据饿死新设备）。

### 5.3 第二层：账户评分（设备内）

```go
score_account = Health × 1 / (Count + 1)

// Health（账户健康，只罚短板）：
//   - 状态 normal=1（cooling/frozen/disabled 已由 filterAvailable 排除）
//   - 日限额余量：1 - DailyUsed/DailyLimit（DailyLimit=0 视为 1）
//   - 失败折扣：FailCount>=3 → ×0.2；FailCount>=1 → ×0.5（复用现 ReportResult 计数）
// Count = WaitPayPool.Count(accountID)（账户负载）
```

### 5.4 与现有代码的落点

改造 `selectServableAccount`（personal.go:244）：

```go
func (c *Channel) selectServableAccount(ctx) *account.Account {
    // ① 现有：adb 在线设备 + 账户集（硬过滤，保留）
    devices, acctIDs := c.servableDevicesAndAccounts(ctx)   // 从 servableAccountIDs 拆出设备维度
    // ② 设备超限过滤（Load ≥ C → 剔除设备及其账户）
    devices = filterOverloaded(devices)
    if len(devices) == 0 {
        logger.Error("personal", "设备负载超限，建议增加收款设备", ...)
        return nil
    }
    // ③ 第一层：选设备（score_device 最高）
    dev := pickDevice(devices)
    // ④ 第二层：该设备内选账户（score_account 最高，替代 weightedRandom）
    return c.pool.SelectFromWeighted(c.channelName(), "default", acctIDsOf(dev),
        func(id int64) float64 { return accountScore(c.pool, id, c.loadOf, c.healthOf) })
}
```

> `SelectFrom` 保留（管理后台/其他调用不受影响），新增 `SelectFromWeighted`（带评分函数参数，替换 weightedRandom 为 argmax 评分）。备注码分配器**不动**（全局唯一 + TTL 5min，均衡分配不改变码的唯一性；byNote 索引按 accountID 隔离，同码跨账户天然不冲突）。

### 5.5 振荡防护

- 评分分母 `Load+1` 天然带惯性：分配后目标负载上升 → 分数下降 → 下次转移，无需额外冷却。
- 设备健康度滑动窗口（20 次）平滑抖动，不因单次 poll 失败跳变。
- 多设备间负载在 `C×80%` 阈值内自然均衡，不会在两台设备间反复横跳（每单只分配一次，分配后负载立即反映）。

---

## 6. 新增埋点（轻量，均为内存滑动窗口，不落库）

| 埋点 | 位置 | 实现 |
|---|---|---|
| 设备 poll 成功率 | `ledgermon.DeviceActor`（PollCmd 执行处） | 环形缓冲 20 次，`RecordPoll(ok bool)`；`PollHealth() float64` |
| 设备一轮实际耗时 | `DeviceScheduler` 窗口结束 | `RecordRound(dur)`；滑动平均（用于校准 pollCost 而非写死 4s） |
| 通知栏快路径命中 | `notifywatcher.Handle` 成功匹配时 | 每设备计数器（10min 窗口），供设备健康加成（可选，v0.1 不接） |
| 设备超限事件 | 分配器拒绝时 | 日志 ERROR + 可选 Bark/Webhook（复用 alert.Client） |

---

## 7. 实施步骤（顺序即依赖）

| 步骤 | 内容 | 涉及文件 | 验收 |
|---|---|---|---|
| **S1 漏单路径修复** | 个人通道关单统一安全检查（personalCheck 注入 + 关单前复查） | order/service.go、reconciler/、main.go | 设备离线时订单绝不关单（单测 + 模拟） |
| **S2 池子设备维度改造** | deviceOf 注入、devicePool 分池、DeviceSnapshot、动态窗口计算器挂载 | pool/waitpay_pool.go、ledgermon/scheduler.go、main.go | 多设备各自独立快照；调度器只读本设备池 |
| **S3 动态关单** | winCalc 实现（公式 §4.1）、CloseAt 单调更新、扫描器改造（30s/10min 窗口） | pool/、order/service.go、main.go | 窗口随负载在 5~10min 内变化；回归测试 |
| **S4 均衡分配** | 设备评分 + 账户评分 + SelectFromWeighted、设备超限过滤 | personal.go、account/pool.go、新增 deviceSelector | 多设备订单分布均衡；超限设备不再接单 |
| **S5 埋点** | 设备 poll 成功率 / 一轮耗时滑动统计 | ledgermon/ | 健康度随真实 poll 结果变化 |
| **S6 全量验证** | 编译 + vet + 单测 + 资金链路测试 + 真机验证 | — | 见 §8 |

---

## 8. 验收标准

### 模块一（动态关单）
1. [ ] 设备离线时，个人通道 WAIT_PAY 订单**不关闭**（转异常池人工核实），无漏单
2. [ ] 高峰（4+ 活跃账户）订单窗口自动放宽至 ≥6min；空闲（1 账户）收敛至 5min 下限
3. [ ] 窗口恒定在 [5min, 10min] 内，不无限低/无限高
4. [ ] 设备负载 ≥ C 时新订单不再路由该设备；全部超限时下单拒绝 + 日志告警
5. [ ] 已付款订单在任何负载下都秒级入账（窗口不影响确认链路）
6. [ ] 回归：`go build ./... && go vet ./... && go test ./... -count=1` 全绿

### 模块二（均衡分配）
1. [ ] 多设备下订单按设备负载均衡分布，不出现单设备积压
2. [ ] 某设备超限/离线 → 流量自动转移至健康设备
3. [ ] 设备内多账户按账户负载 + 日限额余量分配，限额快满账户自动降权
4. [ ] 备注码全局唯一性在均衡分配下不退化（byNote 账户隔离验证）
5. [ ] 分配失败（无可用设备/账户）返回明确错误，不留死单

---

## 9. 风险与兜底

| 风险 | 等级 | 兜底 |
|---|---|---|
| 动态窗口计算器 bug 导致窗口过短 | 高 | 下限 clamp 5min 硬保护 + 单调只增 + 关单前复查三重防线 |
| 设备→账户映射漂移（订单在池中但设备已删） | 中 | `""` 兜底桶 + Reconcile 修复 + 告警 |
| poll 成本估算偏差（真机实测 vs 4s 默认） | 中 | §6 一轮实际耗时埋点校准，env 可配 `DEVICE_POLL_COST_MS` |
| 均衡分配振荡 | 低 | Load+1 分母天然惯性 + 健康度滑动窗口 |
| 全设备超限误拒单 | 低 | 超限阈值 C×80% 降权先于 C 硬拒绝，有缓冲带 |
| 关单扫描与 confirmSched 双路径竞态 | 中 | 两者收敛同一安全检查 + UpdateOrderStatus 条件更新（WHERE status=WAIT_PAY）幂等 |

---

## 10. 配置项（.env 新增）

| 变量 | 默认 | 说明 |
|---|---|---|
| `ORDER_CLOSE_MIN` | `5` | 动态关单窗口下限（分钟） |
| `ORDER_CLOSE_MAX` | `10` | 动态关单窗口上限（分钟） |
| `ORDER_CLOSE_INTERVAL` | `30` | 关单扫描间隔（秒，从 60 收紧） |
| `DEVICE_POLL_COST_MS` | `4000` | 单次 poll 成本估算（ms，埋点校准后可不配） |
| `DEVICE_LOAD_CAP` | `0` | 设备订单承载上限（0=按公式自动算） |
| `DEVICE_OVERLOAD_RATIO` | `0.8` | 设备降权阈值（占 C 的比例） |
| `DEVICE_HEALTH_WINDOW` | `20` | 设备健康度滑动窗口（次） |

---

## 11. 生产上线前代码审查修复记录（2026-08-31）

Oracle 只读审查发现的阻断级与中等问题,均已修复并验证:

| # | 级别 | 问题 | 修复 |
|---|------|------|------|
| P0 | 🔴 崩溃 | `waitpay_pool.go` Add 在锁外调 `orderToEntry`→`windowFor` 遍历 `dp.buckets`,与并发写操作形成 map 竞态(可致 fatal crash);`-race` 未捕获因并发测试未设 winCalc | `Add` 内 `orderToEntry` 移入写锁;并发测试注入 winCalc+deviceOf 以覆盖该路径 |
| P1 | 🔴 功能 | 扫描器候选集 `max(OrderCloseMinutes, dynMax+1min)` 使动态窗口完全失效:默认 30min>11min 时个人订单 5-10min 到期不进候选集 | 候选集改 `min(OrderCloseMinutes, dynMin)`,个人订单 age≥dynMin 起进入,由池内 `CloseAtOf` 精确过滤 |
| P2-1 | 🟡 资金 | `CloseExpiredOrders` 个人分支在线直接关单,绕过 `confirmPersonal` 的 LISTEN_TIMEOUT 复查保护(监听端僵死时误关已付款订单) | 在线且已有 LISTEN_TIMEOUT 异常记录 → 进异常池人工核实,不直接关单 |
| P2-2 | 🟡 资金 | `Add` 覆盖未删旧设备桶:设备映射变更后同订单残留双设备子池,负载虚高 | `unindexLocked(old)` 后显式 delete 旧桶 + 清理空桶/`acctDev` |
| P2-3 | 🟡 一致 | `balanceSelectAccount` 直读 `cfg.OrderCloseMax` 绕过配置退化逻辑(静态窗口下超限过滤仍生效) | `deviceLoadCapOf` 内重复校验 MIN/MAX 合法性,非法 → 返回 0(不启用过滤) |
| P2-4 | 🟡 性能 | 扫描器对每个到期订单逐次 `adb Online()` 探测,无设备级缓存 | `CloseExpiredOrders` 内扫描级设备在线缓存(仅缓存已绑定设备,单次扫描内复用) |

**验证**:`go build`/`go vet`/14 包测试全绿;`go test -race ./internal/pool/ ./internal/order/ ./internal/ledgermon/` 全绿;新增/强化单测:`TestWaitPayPool_ConcurrentStorm`(winCalc 路径)、`TestCloseExpiredOrders_PersonalOnline_StalledListener`、`TestCloseExpiredOrders_Merchant_RespectsFixedWindow`/`OverFixedWindow`。

**记录为已知限制(非阻断,后续加固)**:
- ~~全代码库 0 处 `recover()`~~ **[已修复 2026-08-31]**:8 个后台 goroutine 已加 `defer recover`(关单扫描器/收款码清理+续期/账本日任务/Cloudflare Tunnel/设备加锁/视频转发/告警发送),main.go 统一 `bgRecover` helper 记录 panic+栈;同时修复 notifywatcher 测试闭包变量的 DATA RACE(`orderDriven` 改 atomic.Bool)
- `SetWindowCalc` 原地修改已发布 Entry 的 CloseAt:仅启动时调用一次,无并发读者,实际无害
- 动态窗口公式 `clamp(active×12s×1.5+60s, 5min, 10min)` 依赖 `DEVICE_POLL_COST_MS=4000` 估算,真机实测可校准
