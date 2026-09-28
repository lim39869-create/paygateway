# 支付网关容灾与对账体系设计 v3（主动查账 · 事件驱动 · 三级兜底 · 人工异常池）

> 版本：v3.0（2026-08-09）
> 设计约束：
> - 临时隧道是防攻击特性，不引入固定域名；回调丢失是常态，主动查账是核心保障。
> - 兜底分**三级**：① 自动查单补账（ConfirmScheduler）→ ② 退避重试（查单/通知）→ ③ **人工异常处理池**（自动化兜不住时的最终兜底）。
> - 所有资金写入（含人工处理）只走幂等入口（SettlePayment / CloseOrder），不绕过状态机、不手改账本。
> - 高性能：事件驱动 + timer heap，不轮询；不出错：幂等 + 关单前查单 + 冲突进差错。

---

## 一、设计哲学

1. **回调是优化，查单是保障** —— 快路径（回调入账）与慢路径（查单补账）殊途同归，都进幂等 `SettlePayment`。
2. **数据经过网关，就有信息返回** —— 每个环节可自我验证，异常即触发兜底。
3. **三级兜底**：自动补偿 → 退避重试 → **人工异常池**。自动化兜不住才进人工，人工操作也走同一幂等入口。

---

## 二、每环节"正常路径 vs 兜底"

| 环节 | 正常 | 异常→兜底触发 |
|------|------|----------------|
| ① 上游→网关（下单） | 验签→建单→返回凭据 | 验签/参数失败→拒绝并记录（无资金影响） |
| ② 用户→支付宝（支付） | 回调→入账（快路径） | **回调丢失→ConfirmScheduler 到期→查单补账（慢路径）** |
| ③ 网关→上游（通知） | 通知 200→标记 | 失败→outbox 重试 31h→dead→人工重发 |
| ④ 日终对账 | 一致 | 不一致→差错单→人工 |
| ⑤ **任何环节异常不明** | — | **进人工异常池**（兜底链终点） |

---

## 三、核心模块：ConfirmScheduler（订单确认调度器）

内存 min-heap（按 nextCheckAt 排序），事件驱动，**非轮询**。

```
OnOrderCreated → push(now + T1 60s)
OnCallbackReceived → remove + SettlePayment（幂等）
到期 Confirm(tradeNo)：
  ├─ 已 SUCCESS → remove
  ├─ 仍 WAIT 未到关单时间 → QueryOrder 查单
  │     ├─ TRADE_SUCCESS → SettlePayment 补账 → 通知 → remove
  │     ├─ 未支付 → push(+30s~5m 自适应)
  │     └─ 查单失败 → push(+退避)，连续失败 N 次 → 进异常池（query_failed）
  └─ 到关单时间 → 最后查一次：已支付补账；未支付 CloseOrder；查单失败 → 推迟关单 + 进异常池
```

冲突（查单金额≠订单金额、状态矛盾）→ **不自动入账** → 进异常池（amount_mismatch / state_conflict）。

---

## 四、人工异常处理池（Manual Exception Pool）★新增核心

### 4.1 定位
**自动化兜底失败后的最终兜底**：任何"不按正常流程"的订单（回调全丢且查单不明、金额矛盾、关单存疑、通知 dead 无法重发）自动进入本池。人工核对证据 → 按流程填写必要信息 → 提交后经幂等入口恢复为正常订单，**订单永久标记"人工处理"**，全程审计。

### 4.2 数据模型（新表 + orders 标记）

```sql
-- 异常订单池（只增不改，resolved 后可 reopen，不可删除）
CREATE TABLE exception_orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    out_trade_no    TEXT NOT NULL UNIQUE,      -- 关联订单
    pid             TEXT, upstream_name TEXT, channel TEXT,
    amount_minor    INTEGER,                   -- 网关记录金额（分）
    order_status    TEXT,                      -- 当前订单状态
    channel_status  TEXT,                      -- 最近查单结果（TRADE_SUCCESS/CLOSED/QUERY_FAILED/UNKNOWN）
    channel_tx_id   TEXT,                      -- 查单拿到的通道交易号
    paid_amount     INTEGER,                   -- 人工确认的实际支付金额（默认=amount_minor）
    payer           TEXT,                      -- 人工确认的付款方
    reason          TEXT,                      -- query_failed / amount_mismatch / state_conflict / close_uncertain / notify_dead / manual
    phase           TEXT DEFAULT 'pending',    -- pending / processing / resolved
    resolved_action TEXT,                      -- settle / close / noop / refund
    evidence        TEXT,                      -- 证据时间线 JSON（自动收集，不可手改）
    suggestion      TEXT,                      -- 系统建议动作（依据最近查单结果）
    handler         TEXT, handler_note TEXT,
    resolved_at     TEXT, updated_at TEXT,
    created_at      TEXT
);

-- orders 表新增人工处理标记（列表/详情可见，审计可追溯）
ALTER TABLE orders ADD COLUMN exception_id  INTEGER DEFAULT 0;    -- 关联异常池
ALTER TABLE orders ADD COLUMN handled_by    TEXT DEFAULT '';      -- 处理人
ALTER TABLE orders ADD COLUMN handled_action TEXT DEFAULT '';     -- manual_settle / manual_close / noop
ALTER TABLE orders ADD COLUMN handled_at    TEXT DEFAULT '';
ALTER TABLE orders ADD COLUMN handled_note  TEXT DEFAULT '';

-- 操作审计（谁在何时对哪个异常单做了什么）
CREATE TABLE exception_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    exception_id INTEGER, action TEXT, operator TEXT, detail TEXT, created_at TEXT
);
```

### 4.3 自动进池触发点（ConfirmScheduler / notify 集成）

| 触发 | reason | 进入时自动收集的证据 |
|------|--------|----------------------|
| 查单连续失败 ≥ `query_fail_alert`（5 次） | query_failed | 下单/回调/通知记录 + 每次查单结果时间线 |
| 查单金额 ≠ 订单金额 | amount_mismatch | 查单快照（金额、交易号、时间） |
| 回调与查单状态矛盾 | state_conflict | 两方原始数据 |
| 关单时查单失败（存疑） | close_uncertain | 查单错误信息 |
| 通知 dead 且重发仍失败 | notify_dead | 通知任务重试历史 |
| 管理后台手动加入 | manual | 人工填写的说明 |

进池时同时**暂停 ConfirmScheduler 对该订单的自动动作**（避免人工处理与自动查单打架），订单转入"待人工"。

### 4.4 人工处理流程（管理后台「异常订单」页）

```
① 列表（筛选 phase/reason/上游/日期）→ 打开详情
② 详情：基本信息 + 证据时间线（自动收集，只读）+ 系统建议动作
   （可点「再次查单」刷新证据：调用通道查单，把最新结果并入时间线）
③ 填写处理表单：
     动作：settle（确认入账）/ close（确认未支付，关单）/ noop（无需处理）
     实际支付金额（默认=订单金额，可改）
     通道交易号（可从支付宝账单人工核对后填入）
     付款方（可选）
     备注
④ 提交 → 后端校验（金额>0、订单未处理过、动作合法、表单与证据不冲突）
      → 执行：
        settle → Service.SettlePayment(幂等，参数含人工金额/交易号/付款方/来源=manual)
                  → 成功自动创建通知任务 → 通知上游到账
        close  → Service.CloseOrder(幂等，单笔)
        noop   → 仅记录
      → 成功：orders 写 handled_* 标记 + exception_orders 置 resolved + exception_logs 记审计
⑤ 该订单在列表/详情永久显示「⚠️ 人工处理」徽标（含处理人/时间/备注/动作）
```

### 4.5 防呆与安全

| 项 | 设计 |
|----|------|
| 并发防重 | 处理时 `phase=pending→processing` 行锁，同一订单不可并发处理；resolved 后不可再处理（可管理员 reopen） |
| 幂等入口 | settle/close 一律走 Service 幂等方法，**绝不直接 UPDATE 订单状态/金额** |
| 金额校验 | 人工金额必须 >0 且为整数分；与查单金额不一致时强制二次确认（输入框要求重复输入） |
| 证据只读 | evidence 自动收集，人工不可编辑（防掩盖）；人工信息只写在 paid_amount/payer/handler_note |
| 权限 | 异常池操作走管理后台认证；敏感动作（settle）可加二次确认口令（可配置） |
| 审计 | exception_logs 全程留痕：谁、何时、看了什么、填了什么、做了什么 |
| 账本 | settle 走 SettlePayment 自动过账本 hash 链；人工无法绕账本 |

### 4.6 与上游通知不到的联动
人工 settle 成功后自动创建通知任务 → 若上游仍不可达 → outbox 重试 31h → dead → 此时订单已 SUCCESS，**dead 通知单独成清单**（"上游未确认"），可在异常池关联查看/重发；上游也可主动调 `/api.php` 查单（网关数据正确，拉取是上游的事）。

---

## 五、分阶段落地

| 阶段 | 内容 | 说明 |
|------|------|------|
| **P0-1** | 通知重试增强（31h 窗口）+ dead 管理 API | 修复"付钱未到账" |
| **P0-2** | ConfirmScheduler 最小版（timer heap + 查单补账 + 关单前查单） | 回调丢失自动兜底 |
| **P0-3** | systemd 守护（gateway+new-api）+ 每日 sqlite 备份 | 进程/数据安全 |
| **P1-1** | **人工异常处理池**（表 + 进池触发 + 处理流程 + 管理后台页） | 最终兜底 + 人工台 |
| **P1-2** | 启动配置自检（沙箱/正式、密钥、回调地址） | is_prod 错配早暴露 |
| **P1-3** | 告警（Bark/Webhook：查单连续失败、验签失败率、dead、账本失败、**进池事件**） | 可观测 |
| **P2-1** | 日终对账报告（网关 vs 通道 vs 上游） | 资金最终核对 |

> **存量订单 116**：P0-2 落地后由 ConfirmScheduler 自然发现；若查单亦失败（极端），P1-1 人工池接管——人工从支付宝账单核对后 settle 恢复，标记人工处理。

---

## 六、模块与现有代码对应

| 新组件 | 复用 | 新增 |
|--------|------|------|
| `reconciler.ConfirmScheduler` | `Channel.QueryOrder`、`Service.SettlePayment`、`CloseExpiredOrders` | timer heap、事件接口 |
| `reconciler.ExceptionPool` | admin 框架、审计日志模式、SettlePayment/CloseOrder | exception 表、进池触发、处理流程 API |
| notify 增强 | `notify.Worker`、`notify_tasks` | 重试参数、dead 管理 API |
| admin 页面 | admin 框架 | 异常订单池 Vue 页（证据时间线 + 处理表单） |
| systemd + 备份 | gateway_autostart.sh | service 单元、备份脚本 |

**核心承诺**：所有资金写入（自动或人工）只走 `SettlePayment` / `CloseOrder` 等幂等方法；人工处理同样过账本 hash 链、同样触发通知、同样留审计；订单永久可追溯"是否人工处理过、谁处理的、依据什么"。
