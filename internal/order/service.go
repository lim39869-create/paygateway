// Package order 订单编排层：状态机 / 幂等 / 金额校验 / 审计账本
package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/alert"
	"paygateway/internal/channel"
	"paygateway/internal/epay"
	"paygateway/internal/ledger"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/store"
	"paygateway/internal/upstream"
)

// Service 订单服务
type Service struct {
	store        store.Store
	ledger       *ledger.Ledger
	accountPool  *account.AccountPool   // 账户池（日志/展示账户名称）
	upstreamPool *upstream.UpstreamPool // 上游池（多商户：按 pid 路由验签/签名）
	alert        *alert.Client          // 告警（RESILIENCE P1-3，nil=禁用）

	// 可选：个人通道备注码释放回调（main.go 注入，订单终态时释放备注码，避免 TTL 惰性回收）
	noteCodeReleaser func(channelName, noteCode string)

	// WaitPayHooks WAIT_PAY 内存订单池钩子（main.go 注入，可选）：
	// 个人免签通道订单进入/离开 WAIT_PAY 状态时通知池（调度器数据源），保证池与 DB 同步。
	// 回调在状态流转成功后调用（幂等），不参与资金判定——池只是索引，DB 是真相。
	waitPay *WaitPayHooks

	// 个人通道监听端检查（main.go 注入，与 reconciler.Scheduler.SetPersonalCheck 共用同一函数）：
	// 个人/监听通道关单前调用，监听端离线 → 进异常池人工核实，绝不误关已付款订单。
	personalCheck PersonalCheckFunc

	// 关单窗口（ORDER_CLOSE_MINUTES，0=未开启自动关单）：复用订单时判断是否已超窗
	closeWindow time.Duration
}

// WaitPayHooks WAIT_PAY 池同步钩子（注入后非 nil 才生效）
type WaitPayHooks struct {
	// Add 订单进入 WAIT_PAY 且已绑定账户（下单/复活后调用，幂等覆盖）
	Add func(o *model.Order)
	// Upsert 订单凭据刷新/换通道（NoteCode/AccountID 可能变化，覆盖更新）
	Upsert func(o *model.Order)
	// Remove 订单离开 WAIT_PAY（入账/关单后调用，幂等）
	Remove func(outTradeNo string)
	// CloseAtOf 查询订单的动态关单时间（pool 注入，可选）：返回 (closeAt, found)。
	// 动态关单窗口下，扫描器用该时间判断是否真正到期——未到期则跳过（窗口由设备负载决定）。
	CloseAtOf func(outTradeNo string) (time.Time, bool)
}

// PersonalCheckFunc 个人通道监听端检查（ConfirmByManual 通道关单前调用）。
// 返回 (offline, reason)：offline=true 表示监听端离线/异常，订单进异常池人工核实（用户可能已付款但未监听到）。
type PersonalCheckFunc func(ctx context.Context, o *model.Order) (offline bool, reason string)

// SetPersonalCheck 注入个人通道监听端检查（与 reconciler.Scheduler 共用，防止两条关单路径安全检查分叉）。
// nil 安全（不注入 = 不检查直接关，仅测试/无设备环境）。
func (s *Service) SetPersonalCheck(fn PersonalCheckFunc) { s.personalCheck = fn }

// SetWaitPayHooks 注入 WAIT_PAY 池钩子（main.go 调用；nil 安全）
func (s *Service) SetWaitPayHooks(h *WaitPayHooks) { s.waitPay = h }

// SetAlert 注入告警客户端（可选）
func (s *Service) SetAlert(a *alert.Client) { s.alert = a }

// SetCloseWindow 注入关单窗口（分钟），复用订单超窗判定用（0=不启用超窗刷新）
func (s *Service) SetCloseWindow(minutes int) {
	if minutes > 0 {
		s.closeWindow = time.Duration(minutes) * time.Minute
	}
}

// isExpiredWindow 订单是否已超出关单窗口（WAIT_PAY 但 CreatedAt 超窗，关单清扫器尚未执行）
func (s *Service) isExpiredWindow(o *model.Order) bool {
	return s.closeWindow > 0 && !o.CreatedAt.IsZero() && time.Since(o.CreatedAt) >= s.closeWindow
}

// SetNoteCodeReleaser 注入个人通道备注码释放回调（可选）
// 回调在订单终态（SUCCESS/CLOSED）时调用，避免备注码仅靠 TTL 惰性回收
func (s *Service) SetNoteCodeReleaser(fn func(channelName, noteCode string)) {
	s.noteCodeReleaser = fn
}

// releaseNoteCode 订单终态时释放备注码（仅 personal.* 通道生效）
func (s *Service) releaseNoteCode(o *model.Order) {
	if s.noteCodeReleaser != nil && o.NoteCode != "" && strings.HasPrefix(o.Channel, "personal.") {
		s.noteCodeReleaser(o.Channel, o.NoteCode)
	}
}

// New 创建订单服务
func New(st store.Store, lg *ledger.Ledger, pool *account.AccountPool, upPool *upstream.UpstreamPool) *Service {
	return &Service{store: st, ledger: lg, accountPool: pool, upstreamPool: upPool}
}

// signKey 按订单 pid 返回签名密钥（多商户路由，严格模式）
// pid 未登记或已禁用 → 返回空（回调签名无效，防止未登记上游的订单入账，防御）
func (s *Service) signKey(pid string) string {
	if s.upstreamPool == nil || pid == "" {
		return ""
	}
	if u := s.upstreamPool.ByPid(pid); u != nil && u.Status == upstream.StatusEnabled && u.Key != "" {
		return u.Key
	}
	return ""
}

// upstreamName 返回订单归属的上游名称（展示用）
func (s *Service) upstreamName(pid string) string {
	if s.upstreamPool == nil || pid == "" {
		return ""
	}
	if u := s.upstreamPool.ByPid(pid); u != nil {
		return u.Name
	}
	return ""
}

// accountName 返回账户池中账户名称（用于日志/展示；查不到返回空）
func (s *Service) accountName(id int64) string {
	if s.accountPool == nil || id <= 0 {
		return ""
	}
	if acct := s.accountPool.GetByID(id); acct != nil {
		return acct.Name
	}
	return ""
}

// queryOrder 历史遍历查单；新调用应传订单所属通道名以避免跨通道错账。
func (s *Service) queryOrder(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error) {
	return s.queryOrderFor(ctx, outTradeNo, "")
}

var ErrNotQueryable = errors.New("订单所属通道无法主动查单")

func (s *Service) queryOrderFor(ctx context.Context, outTradeNo, channelName string) (*channel.OrderStatus, error) {
	if channelName != "" {
		ch, ok := channel.Lookup(channelName)
		if !ok {
			return nil, fmt.Errorf("通道未注册: %s", channelName)
		}
		if ch.ConfirmAbility() != channel.ConfirmByQuery {
			return nil, ErrNotQueryable
		}
		if !channel.IsEnabled(channelName) {
			return nil, fmt.Errorf("通道已禁用: %s", channelName)
		}
		return ch.QueryOrder(ctx, outTradeNo)
	}
	var lastErr error
	for _, name := range channel.Names() {
		ch, ok := channel.Get(name)
		if !ok || ch.ConfirmAbility() != channel.ConfirmByQuery {
			continue
		}
		st, err := ch.QueryOrder(ctx, outTradeNo)
		if err != nil {
			lastErr = err
			continue
		}
		return st, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无可查询通道")
	}
	return nil, lastErr
}

// CreateOrderResult 下单结果
type CreateOrderResult struct {
	Order    *model.Order
	Cred     *channel.PayCredential
	Reused   bool // 是否复用了已有订单（幂等）
	Existing *model.Order
}

// CreateOrder 下单（幂等：同一订单号重复请求复用原订单，不重复调通道）
func (s *Service) CreateOrder(ctx context.Context, req *CreateRequest) (*CreateOrderResult, error) {
	// 金额解析（字符串元 → 最小单位分）：前置解析，复用路径需以本次下单金额为最新标准
	amountMinor, err := model.ParseMoneyToMinor(req.Money)
	if err != nil {
		return nil, fmt.Errorf("金额非法: %w", err)
	}

	// 1. 幂等检查
	if existing, err := s.store.GetOrder(ctx, req.OutTradeNo); err != nil {
		return nil, err
	} else if existing != nil {
		logger.Warn("order", "订单已存在，复用", "out_trade_no", req.OutTradeNo, "status", existing.Status)
		return s.reuseOrCreateError(ctx, existing, req.Type, amountMinor)
	}

	// 1.5 买家级串行：同一买家环境已有待支付订单 → 复用原订单（防海量刷单/错配）
	// 双维度检查：buyer_key(IP+UA+device+buyer_id) 或 fp_key(浏览器指纹 cookie) 命中均拦截。
	// 换 IP 攻击：buyer_key 变但 fp_key(cookie) 不变 → 仍被拦截。
	if req.BuyerKey != "" && strings.HasPrefix(req.Type, "personal.") {
		var pending []*model.Order
		var err error
		if req.FPKey != "" {
			pending, err = s.store.ListWaitPayByBuyerOrFP(ctx, req.BuyerKey, req.FPKey)
		} else {
			pending, err = s.store.ListWaitPayByBuyer(ctx, req.BuyerKey)
		}
		if err != nil {
			logger.Error("order", "买家待支付订单查询失败", "err", err, "buyer_key", req.BuyerKey)
		} else if len(pending) > 0 {
			o := pending[0]
			bk := req.BuyerKey
			if len(bk) > 12 {
				bk = bk[:12]
			}
			if o.OutTradeNo != req.OutTradeNo {
				// 不同订单号：上游订单号与金额为不可变主键，无法复用 OLD。
				// 关闭 OLD（CAS/幂等，释放备注码+移出池）。并发时仅 CAS 胜出者创建 NEW，
				// 落败者复用获胜新单或返回安全冲突错误，绝不产生第二笔活跃 WAIT_PAY。
				if !o.SessionExpiresAt.IsZero() && !time.Now().Before(o.SessionExpiresAt) {
					return nil, fmt.Errorf("订单 %s 的支付会话已过期，请重新发起充值", o.OutTradeNo)
				}
				req.SessionExpiresAt = s.sessionExpiryOf(o)
				logger.Warn("order", "买家已有待支付订单，替换旧单创建新单", "buyer_key", bk, "pending", o.OutTradeNo, "new", req.OutTradeNo)
				reuse, create, err := s.replacePendingOrder(ctx, o, req.OutTradeNo, amountMinor)
				if err != nil {
					return nil, err
				}
				if !create {
					if reuse != nil {
						// 并发替换者已创建获胜新单：复用之，不再重复创建。
						logger.Warn("order", "买家替换并发，复用获胜新单", "buyer_key", bk, "new", req.OutTradeNo, "winner", reuse.OutTradeNo)
						return &CreateOrderResult{Order: reuse, Reused: true, Existing: reuse}, nil
					}
					// 并发结算胜出（用户已付 OLD）：禁止再创建同买家新单，避免重复收款。
					return nil, fmt.Errorf("订单 %s 已支付，请勿重复下单", o.OutTradeNo)
				}
			} else {
				// 同订单号：幂等复用（金额不可变，见 reuseOrCreateError 的金额校验）
				if o.PayURL != "" || o.QrCode != "" {
					return s.reuseOrCreateError(ctx, o, req.Type, amountMinor)
				}
				return nil, fmt.Errorf("您有未支付订单 %s，请先完成支付", o.OutTradeNo)
			}
		}
	}

	// 2. 获取通道
	ch, ok := channel.Get(req.Type)
	if !ok {
		return nil, channel.ErrUnsupported
	}

	// 4. 创建订单（数据库唯一约束兜底幂等）
	// 记录上游标识：pid 来自下单参数（多商户路由），用于订单归属 + 回调签名
	now := time.Now()
	sessionExpiresAt := req.SessionExpiresAt
	if sessionExpiresAt.IsZero() && s.closeWindow > 0 {
		sessionExpiresAt = now.Add(s.closeWindow)
	}
	o := &model.Order{
		OutTradeNo:       req.OutTradeNo,
		GatewayNo:        fmt.Sprintf("GW%d", now.UnixNano()),
		Channel:          req.Type,
		Pid:              req.RawParams["pid"],
		Attach:           req.RawParams["attach"],
		AmountMinor:      amountMinor,
		Currency:         "CNY",
		Subject:          req.Name,
		Status:           model.StatusWaitPay,
		NotifyURL:        req.NotifyURL,
		ReturnURL:        req.ReturnURL,
		BuyerKey:         req.BuyerKey,
		FPKey:            req.FPKey,
		CreatedAt:        now,
		SessionExpiresAt: sessionExpiresAt,
	}
	o.UpstreamName = s.upstreamName(o.Pid)
	if err := s.store.CreateOrder(ctx, o); err != nil {
		// 并发同号：另一请求刚插入同一 out_trade_no（唯一约束冲突）→ 转复用路径，
		// 不让商户收到裸 DB 错误
		if existing, gerr := s.store.GetOrder(ctx, req.OutTradeNo); gerr == nil && existing != nil {
			return s.reuseOrCreateError(ctx, existing, req.Type, amountMinor)
		}
		return nil, fmt.Errorf("创建订单失败: %w", err)
	}
	s.recordEvent(ctx, o, "created", "", amountMinor, req.rawJSON())

	// 5. 调通道下单
	payReq := &channel.PayRequest{
		OutTradeNo:  o.OutTradeNo,
		GatewayNo:   o.GatewayNo,
		Subject:     o.Subject,
		AmountMinor: o.AmountMinor,
		Currency:    o.Currency,
		NotifyURL:   "", // 通道 notify 由网关统一配置
		ReturnURL:   o.ReturnURL,
		PayMode:     channel.PayMode(req.PayMode),
		Device:      req.Device,
	}
	cred, err := ch.CreatePayment(ctx, payReq)
	if err != nil {
		logger.Error("order", "通道下单失败", "err", err, "out_trade_no", o.OutTradeNo)
		// 清理无凭据订单：留着会让同号重试撞"已处理"死墙（订单号被永久烧掉）。
		// 删除后商户可用原号重试；created 事件随行删除，失败事实在日志与告警中留痕。
		if _, derr := s.store.DeleteOrder(ctx, o.OutTradeNo); derr != nil {
			logger.Error("order", "清理失败订单未遂（该单号将无法重用，需人工处理）",
				"err", derr, "out_trade_no", o.OutTradeNo)
		}
		return nil, err
	}

	// 6. 保存支付凭据（条件更新，WAIT_BUYER_PAY -> WAIT_BUYER_PAY 幂等）
	// 使用后台上下文，避免 HTTP 客户端断开导致数据库操作被取消
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	if cred.PayURL != "" {
		o.PayURL = cred.PayURL
	}
	if cred.QRCode != "" {
		o.QrCode = cred.QRCode
	}
	if cred.AccountID > 0 {
		o.AccountID = cred.AccountID
	}
	if _, err := s.store.UpdateOrderStatus(dbCtx, o.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		if cred.PayURL != "" {
			upd.PayURL = cred.PayURL
		}
		if cred.QRCode != "" {
			upd.QrCode = cred.QRCode
		}
		if cred.AccountID > 0 {
			upd.AccountID = cred.AccountID
		}
		// 个人免签通道：持久化备注码 + 套码 token（匹配/跳转用）
		if cred.NoteCode != "" {
			upd.NoteCode = cred.NoteCode
		}
		if cred.QrToken != "" {
			upd.QrToken = cred.QrToken
		}
		if cred.DeviceID != "" {
			upd.DeviceID = cred.DeviceID
		}
	}); err != nil {
		return nil, err
	}
	// 读回最新（含凭据）
	latest, _ := s.store.GetOrder(dbCtx, o.OutTradeNo)
	if latest != nil {
		o = latest
	}

	logger.Info("order", "下单成功 ✅",
		"out_trade_no", o.OutTradeNo,
		"pid", o.Pid,
		"upstream", o.UpstreamName,
		"channel", req.Type,
		"amount", amountMinor,
		"mode", cred.Mode,
		"account_id", cred.AccountID,
		"account", s.accountName(cred.AccountID))

	// WAIT_PAY 池同步：个人免签订单已绑定账户 + 凭据，进入调度数据源
	s.notifyWaitPayAdd(o)

	return &CreateOrderResult{Order: o, Cred: cred}, nil
}

// reuseOrCreateError 已存在订单的统一处置：
//   - 待支付订单（WAIT_PAY）且有凭据：
//   - 请求通道 ≠ 订单通道（均为个人免签）→ 自动迁移订单到新通道账户（保留原过期时间）
//   - 否则 → 复用原订单（保留原过期时间）
//   - 已关闭订单（TRADE_CLOSED）同号重提 → 复活重新计时（重新走一个支付生命周期）
//   - 其他终态（TRADE_SUCCESS 等）→ 报"已处理"
//
// 僵尸订单防护语义（只针对 WAIT_PAY）：
//   - WAIT_PAY 订单占资源（买家串行锁/备注码/账户绑定/倒计时），
//     复用/切换均不重置 CreatedAt，到期必关 → 无法无限续命占资源
//   - TRADE_CLOSED 已关闭不占资源，同号复活只是重新支付，无危害
func (s *Service) reuseOrCreateError(ctx context.Context, existing *model.Order, reqType string, amountMinor int64) (*CreateOrderResult, error) {
	hasCred := existing.PayURL != "" || existing.QrCode != ""
	if !hasCred {
		return nil, fmt.Errorf("订单 %s 已处理（状态: %s）", existing.OutTradeNo, existing.Status)
	}
	// 金额不可变：订单号+金额均为上游不可变主键。同号重提若金额不同 → 拒单，
	// 绝不覆盖已存在订单金额，也绝不以新金额刷新凭据（否则回调按订单金额校验会永不入账/错账）。
	// 不同订单号的替换路径见 replacePendingOrder（CreateOrder 买家守卫调用）。
	if amountMinor > 0 && amountMinor != existing.AmountMinor {
		return nil, fmt.Errorf("订单 %s 金额不可变：原金额=%d 新金额=%d（订单号已存在，请使用新订单号提交）",
			existing.OutTradeNo, existing.AmountMinor, amountMinor)
	}
	// 已关闭订单同号重提 → 复活（上游同号重试是易支付协议标准行为）
	if existing.Status == model.StatusClosed {
		revived, err := s.ReviveOrder(ctx, existing, reqType)
		if err != nil {
			logger.Error("order", "已关闭订单复活失败", "err", err, "out_trade_no", existing.OutTradeNo, "to", reqType)
			return nil, fmt.Errorf("订单 %s 已过期，重新下单失败: %w", existing.OutTradeNo, err)
		}
		return &CreateOrderResult{Order: revived, Reused: true, Existing: revived}, nil
	}
	if existing.Status != model.StatusWaitPay {
		return nil, fmt.Errorf("订单 %s 已处理（状态: %s）", existing.OutTradeNo, existing.Status)
	}
	// 自动切换支付方式：上游换了支付模式的入口进来（同号不同 type），订单整体迁移到新通道
	if reqType != "" && reqType != existing.Channel {
		if strings.HasPrefix(existing.Channel, "personal.") && strings.HasPrefix(reqType, "personal.") {
			switched, err := s.SwitchChannel(ctx, existing, reqType)
			if err != nil {
				logger.Warn("order", "自动切换支付方式失败，退回复用原订单", "err", err, "out_trade_no", existing.OutTradeNo, "from", existing.Channel, "to", reqType)
			} else {
				return &CreateOrderResult{Order: switched, Reused: true, Existing: switched}, nil
			}
		}
	}
	// 复用前超窗检查：WAIT_PAY 但已超过关单窗口（关单清扫器还没跑）→ 刷新凭据并重置倒计时，
	// 避免复用出的支付页立即过期跳回过期页（不违反僵尸防护：只对"已超窗"订单重新计时）。
	if s.isExpiredWindow(existing) {
		refreshed, err := s.RefreshPayment(ctx, existing)
		if err != nil {
			logger.Warn("order", "复用超窗订单刷新失败，按原单返回", "err", err, "out_trade_no", existing.OutTradeNo, "amount", existing.AmountMinor)
		} else {
			return &CreateOrderResult{Order: refreshed, Reused: true, Existing: refreshed}, nil
		}
	}
	return &CreateOrderResult{Order: existing, Reused: true, Existing: existing}, nil
}

// replacePendingOrder 买家已有 WAIT_PAY 待支付订单(OLD)，本次 request 用了不同 out_trade_no(NEW)。
// OLD 的订单号+金额是上游不可变主键，无法复用 → 关闭 OLD 释放资源，再由调用方为 NEW 走正常下单。
//
// 并发安全：多个同买家请求可能同时试图替换同一个 OLD。用 UpdateOrderStatus 的 CAS 语义仲裁——
// 仅 CAS 胜出者（replaceOrder 原子关单+写 replaced 事件）创建 NEW；CAS 落败者不得再创建自己的 NEW，
// 否则同一买家会出现多笔 WAIT_PAY。落败者依据 OLD 的终态与 replaced 事件分派：
//
//   - OLD=SUCCESS（并发结算胜出）→ create=false，禁止创建新单（防重复收款）。
//   - OLD=CLOSED 且带 replaced 事件（被并发替换者关闭）→ create=false，若替换者的 NEW 已存在则复用，
//     否则返回可安全重试的冲突错误；绝不重复创建 NEW。
//   - OLD=CLOSED 且无 replaced 事件（仅被清扫器/超时关闭）→ create=true，正常为新单走创建流程。
//
// 返回 (reuse, create, err)：create=false 时若 reuse!=nil 则调用方应复用该订单，否则调用方不得创建 NEW。
func (s *Service) replacePendingOrder(ctx context.Context, old *model.Order, newOutTradeNo string, newAmountMinor int64) (*model.Order, bool, error) {
	payload := fmt.Sprintf(`{"old_out_trade_no":%q,"old_amount":%d,"new_out_trade_no":%q,"new_amount":%d,"reason":"buyer_replaced","time":%q}`,
		old.OutTradeNo, old.AmountMinor, newOutTradeNo, newAmountMinor, time.Now().Format(time.RFC3339))
	closed, err := s.store.ReplaceOrder(ctx, old.OutTradeNo, old.ID, old.AmountMinor, payload)
	if err != nil {
		return nil, false, err
	}
	if closed {
		// 赢得替换：释放备注码 + 移出 WAIT_PAY 池（各一次），随后调用方为 NEW 走正常下单。
		s.releaseNoteCode(old)
		s.notifyWaitPayRemove(old)
		logger.Warn("order", "买家待支付订单已被新订单替换关闭", "old", old.OutTradeNo, "new", newOutTradeNo, "new_amount", newAmountMinor)
		return nil, true, nil
	}

	latest, gerr := s.store.GetOrder(ctx, old.OutTradeNo)
	if gerr == nil && latest != nil && latest.Status == model.StatusSuccess {
		logger.Error("order", "买家待支付并发结算胜出，取消新单创建（防重复收款）", "old", old.OutTradeNo, "new", newOutTradeNo)
		return nil, false, nil
	}

	// 非结算：OLD 带 replaced 事件 ⇒ 被并发替换者关闭，不得再创建自己的 NEW。
	if s.hasReplacedRecently(ctx, old) {
		return s.reuseWinnerNew(ctx, old, newOutTradeNo)
	}

	// OLD 仅被清扫器/超时关闭（无 replaced 事件）→ 安全为本次请求创建全新 NEW。
	return nil, true, nil
}

// hasReplacedRecently 判断 OLD 是否刚被「并发替换者」关闭：存在近窗口内的 replaced 事件。
// 近窗口过滤掉「被替换后同号复活、又被清扫器关闭」的罕见历史场景，避免误判为并发替换。
func (s *Service) hasReplacedRecently(ctx context.Context, old *model.Order) bool {
	evs, err := s.store.ListEvents(ctx, old.ID, 20)
	if err != nil {
		return false
	}
	cutoff := time.Now().Add(-30 * time.Second)
	for _, e := range evs {
		if e.EventType == "replaced" && e.CreatedAt.After(cutoff) {
			return true
		}
	}
	return false
}

// reuseWinnerNew 并发替换者已关闭 OLD：返回其创建的 NEW 供复用，若尚不可见则返回可安全重试的冲突错误。
func (s *Service) reuseWinnerNew(ctx context.Context, old *model.Order, newOutTradeNo string) (*model.Order, bool, error) {
	pending, err := s.store.ListWaitPayByBuyerOrFP(ctx, old.BuyerKey, old.FPKey)
	if err != nil {
		return nil, false, err
	}
	for _, p := range pending {
		if p.OutTradeNo != old.OutTradeNo {
			logger.Warn("order", "买家替换并发，复用获胜新单而非重复创建", "old", old.OutTradeNo, "new", newOutTradeNo, "winner", p.OutTradeNo)
			return p, false, nil
		}
	}
	return nil, false, fmt.Errorf("订单 %s 正在被并发替换为新单，请稍后重试", old.OutTradeNo)
}

// CreateRequest 下单请求（易支付协议参数）
type CreateRequest struct {
	OutTradeNo       string
	Type             string // alipay / wxpay / paypal / usdt
	Name             string
	Money            string // 元（字符串）
	NotifyURL        string
	ReturnURL        string
	PayMode          string    // page / wap / qrcode（pay_type）
	Device           string    // pc / mobile
	BuyerKey         string    // 下单环境唯一标识（IP+UA+设备指纹 hash，买家级串行/防刷单）
	FPKey            string    // 浏览器指纹标识（cookie pg_fp，换 IP 场景串行兜底）
	SessionExpiresAt time.Time // 内部替换继承的支付会话硬截止时间
	RawParams        map[string]string
}

func (s *Service) sessionExpiryOf(o *model.Order) time.Time {
	if o == nil {
		return time.Time{}
	}
	return o.SessionExpiresAt
}

// RefreshPayment 刷新支付凭据（二维码刷新）：保持原订单号和上游关联不变，重新调通道生成新二维码/备注码。
//
// 易支付协议中 out_trade_no 是上游的业务主键，刷新二维码不允许改单号，
// 否则回调带上新单号（如 xxx-R2）时上游找不到订单 → 回调入账失败。
func (s *Service) RefreshPayment(ctx context.Context, o *model.Order) (*model.Order, error) {
	if o == nil || o.OutTradeNo == "" {
		return nil, fmt.Errorf("订单为空")
	}
	if o.Status != model.StatusWaitPay {
		return nil, fmt.Errorf("订单当前状态(%s)不可刷新", o.Status)
	}
	ch, ok := channel.Get(o.Channel)
	if !ok {
		return nil, channel.ErrUnsupported
	}
	// 释放旧备注码（避免 5 分钟 TTL 惰性回收期间码被占用）
	if strings.HasPrefix(o.Channel, "personal.") {
		s.releaseNoteCode(o)
	}
	// 重新调通道生成支付凭据（用原订单号）
	payReq := &channel.PayRequest{
		OutTradeNo:  o.OutTradeNo,
		GatewayNo:   o.GatewayNo,
		Subject:     o.Subject,
		AmountMinor: o.AmountMinor,
		Currency:    o.Currency,
		NotifyURL:   "",
		ReturnURL:   o.ReturnURL,
		PayMode:     channel.ModeQRCode,
		Device:      "pc",
	}
	cred, err := ch.CreatePayment(ctx, payReq)
	if err != nil {
		logger.Error("order", "刷新支付凭据失败", "err", err, "out_trade_no", o.OutTradeNo)
		return nil, err
	}
	// 条件更新订单凭据（WAIT_BUYER_PAY -> WAIT_BUYER_PAY，幂等）
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	if _, err := s.store.UpdateOrderStatus(dbCtx, o.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		// 刷新二维码 = 用户重新支付：重置倒计时给完整支付窗口。
		// 不违反僵尸防护：订单始终 WAIT_PAY 未复活已关闭订单；
		// 且刷新是用户主动点击（每点一次重新计时），与"同号重提自动续命"语义不同。
		upd.CreatedAt = time.Now()
		if cred.PayURL != "" {
			upd.PayURL = cred.PayURL
		}
		if cred.QRCode != "" {
			upd.QrCode = cred.QRCode
		}
		if cred.AccountID > 0 {
			upd.AccountID = cred.AccountID
		}
		if cred.NoteCode != "" {
			upd.NoteCode = cred.NoteCode
		}
		if cred.QrToken != "" {
			upd.QrToken = cred.QrToken
		}
		if cred.DeviceID != "" {
			upd.DeviceID = cred.DeviceID
		}
	}); err != nil {
		return nil, err
	}
	// 读回最新订单（含新凭据）
	latest, _ := s.store.GetOrder(dbCtx, o.OutTradeNo)
	if latest != nil {
		o = latest
	}
	logger.Info("order", "支付凭据已刷新 ✅",
		"out_trade_no", o.OutTradeNo, "channel", o.Channel,
		"pid", o.Pid, "amount", o.AmountMinor)
	// WAIT_PAY 池同步：凭据/备注码已更新，覆盖池条目
	s.notifyWaitPayUpsert(o)
	return o, nil
}

// SwitchChannel 切换支付方式（个人免签通道间迁移订单）
//
// 用户下单后想换支付方式（如支付宝→微信）：保持订单号和金额不变，
// 重新调目标通道选账户生成凭据，订单整体迁移到目标通道的可用账户。
// 仅允许未过期（WAIT_PAY）订单切换；保留原 CreatedAt（过期时间复用，
// 防换支付方式无限刷新倒计时/复活 → 僵尸订单刷单漏洞）。
// TRADE_CLOSED 订单是终态，任何重提都报"已处理"（不允许复活）。
//
// 幂等性：目标通道与当前一致时直接返回原订单（不重复调通道）。
func (s *Service) SwitchChannel(ctx context.Context, o *model.Order, newChannel string) (*model.Order, error) {
	if o == nil || o.OutTradeNo == "" {
		return nil, fmt.Errorf("订单为空")
	}
	if o.Channel == newChannel {
		return o, nil
	}
	if !strings.HasPrefix(o.Channel, "personal.") || !strings.HasPrefix(newChannel, "personal.") {
		return nil, fmt.Errorf("仅支持个人免签通道间切换支付方式")
	}
	if o.Status != model.StatusWaitPay {
		return nil, fmt.Errorf("订单当前状态(%s)不可切换支付方式（仅待支付订单可切换，过期订单不可复活）", o.Status)
	}
	ch, ok := channel.Get(newChannel)
	if !ok {
		return nil, channel.ErrUnsupported
	}
	// 释放旧通道备注码（避免 TTL 惰性回收期间码被占用）
	if strings.HasPrefix(o.Channel, "personal.") {
		s.releaseNoteCode(o)
	}
	// 重新调目标通道生成支付凭据（用原订单号；SelectFrom 按 channel 过滤自动选对平台账户）
	payReq := &channel.PayRequest{
		OutTradeNo:  o.OutTradeNo,
		GatewayNo:   o.GatewayNo,
		Subject:     o.Subject,
		AmountMinor: o.AmountMinor,
		Currency:    o.Currency,
		NotifyURL:   "",
		ReturnURL:   o.ReturnURL,
		PayMode:     channel.ModeQRCode,
		Device:      "pc",
	}
	cred, err := ch.CreatePayment(ctx, payReq)
	if err != nil {
		logger.Error("order", "切换支付方式失败（目标通道下单失败）", "err", err, "out_trade_no", o.OutTradeNo, "to", newChannel)
		return nil, err
	}
	// WAIT_PAY→WAIT_PAY 换凭据；不重置 CreatedAt（过期时间复用，防僵尸订单刷单）
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	ok2, err := s.store.UpdateOrderStatus(dbCtx, o.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.Channel = newChannel
		if cred.PayURL != "" {
			upd.PayURL = cred.PayURL
		}
		if cred.QRCode != "" {
			upd.QrCode = cred.QRCode
		}
		if cred.AccountID > 0 {
			upd.AccountID = cred.AccountID
		}
		if cred.NoteCode != "" {
			upd.NoteCode = cred.NoteCode
		}
		if cred.QrToken != "" {
			upd.QrToken = cred.QrToken
		}
		if cred.DeviceID != "" {
			upd.DeviceID = cred.DeviceID
		}
	})
	if err != nil {
		return nil, err
	}
	if !ok2 {
		return nil, fmt.Errorf("订单状态已变化，请刷新后重试")
	}
	// 读回最新订单（含新凭据）
	latest, _ := s.store.GetOrder(dbCtx, o.OutTradeNo)
	if latest != nil {
		o = latest
	}
	logger.Info("order", "支付方式已切换 ✅",
		"out_trade_no", o.OutTradeNo, "from", o.Channel, "to", newChannel,
		"account_id", cred.AccountID, "status", o.Status, "amount", o.AmountMinor)
	// WAIT_PAY 池同步：通道/账户/备注码已更新，覆盖池条目
	s.notifyWaitPayUpsert(o)
	return o, nil
}

// ReviveOrder 已关闭订单同号重提复活：TRADE_CLOSED → WAIT_PAY，重新走一个支付生命周期。
//
// 上游固定订单号重试（易支付协议标准行为）时调用；已关闭订单不占资源
// （买家串行锁/备注码/倒计时均已释放），复活 = 重新支付，无僵尸订单风险。
// reqType 非空且与订单通道不同时顺带切换支付方式（SelectFrom 按 channel 选目标平台账户）。
func (s *Service) ReviveOrder(ctx context.Context, o *model.Order, reqType string) (*model.Order, error) {
	if o == nil || o.OutTradeNo == "" {
		return nil, fmt.Errorf("订单为空")
	}
	if o.Status != model.StatusClosed {
		return nil, fmt.Errorf("订单状态(%s)不可复活（仅 TRADE_CLOSED）", o.Status)
	}
	newChannel := o.Channel
	if reqType != "" {
		newChannel = reqType
	}
	if !strings.HasPrefix(newChannel, "personal.") {
		return nil, fmt.Errorf("仅支持个人免签通道复活")
	}
	ch, ok := channel.Get(newChannel)
	if !ok {
		return nil, channel.ErrUnsupported
	}
	// 释放旧通道备注码（复活后重新分配，避免 TTL 惰性回收期间码被占用）
	if strings.HasPrefix(o.Channel, "personal.") {
		s.releaseNoteCode(o)
	}
	// 重新调通道生成支付凭据（用原订单号；选账户按目标通道过滤）
	payReq := &channel.PayRequest{
		OutTradeNo:  o.OutTradeNo,
		GatewayNo:   o.GatewayNo,
		Subject:     o.Subject,
		AmountMinor: o.AmountMinor,
		Currency:    o.Currency,
		NotifyURL:   "",
		ReturnURL:   o.ReturnURL,
		PayMode:     channel.ModeQRCode,
		Device:      "pc",
	}
	cred, err := ch.CreatePayment(ctx, payReq)
	if err != nil {
		logger.Error("order", "复活订单失败（目标通道下单失败）", "err", err, "out_trade_no", o.OutTradeNo, "to", newChannel)
		return nil, err
	}
	// TRADE_CLOSED → WAIT_PAY，重置 CreatedAt 重新计时，清除 paid_at
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	ok2, err := s.store.UpdateOrderStatus(dbCtx, o.OutTradeNo, model.StatusClosed, model.StatusWaitPay, func(upd *model.Order) {
		upd.Channel = newChannel
		upd.CreatedAt = time.Now()
		upd.PaidAt = nil // from!=to 分支会 COALESCE 填 now，必须清掉
		if cred.PayURL != "" {
			upd.PayURL = cred.PayURL
		}
		if cred.QRCode != "" {
			upd.QrCode = cred.QRCode
		}
		if cred.AccountID > 0 {
			upd.AccountID = cred.AccountID
		}
		if cred.NoteCode != "" {
			upd.NoteCode = cred.NoteCode
		}
		if cred.QrToken != "" {
			upd.QrToken = cred.QrToken
		}
		if cred.DeviceID != "" {
			upd.DeviceID = cred.DeviceID
		}
	})
	if err != nil {
		return nil, err
	}
	if !ok2 {
		return nil, fmt.Errorf("订单状态已变化，请刷新后重试")
	}
	latest, _ := s.store.GetOrder(dbCtx, o.OutTradeNo)
	if latest != nil {
		o = latest
	}
	logger.Info("order", "已关闭订单已复活 ✅",
		"out_trade_no", o.OutTradeNo, "from", o.Channel, "to", newChannel,
		"account_id", cred.AccountID, "status", o.Status, "amount", o.AmountMinor)
	// WAIT_PAY 池同步：CLOSED → WAIT_PAY 重新进入池
	s.notifyWaitPayAdd(o)
	return o, nil
}

func (r *CreateRequest) rawJSON() string {
	b, _ := json.Marshal(r.RawParams)
	return string(b)
}

// ---------- 回调处理 ----------

// SettlePayment 支付成功结算（核心资金操作，唯一入口）
// nr: 通道验签后的通知结果（含 out_trade_no / 实付金额 / 通道交易号）
func (s *Service) SettlePayment(ctx context.Context, nr *channel.NotifyResult) (bool, error) {
	outTradeNo := nr.OutTradeNo
	// 1. 查订单
	o, err := s.store.GetOrder(ctx, outTradeNo)
	if err != nil {
		return false, err
	}
	if o == nil {
		logger.Error("order", "订单不存在", "out_trade_no", outTradeNo)
		return false, nil
	}

	// 2. 金额校验（防改价攻击）
	// 严格模式：实付金额缺失(0)同样拒绝——所有通道的通知/查单路径都能提供金额，
	// 放行未知金额 = 允许未经验证的资金入账。拒绝后通道会重试，持续失败转人工池。
	if nr.AmountMinor != o.AmountMinor {
		logger.Error("order", "金额校验失败（重大安全事件）", "out_trade_no", outTradeNo, "amount", o.AmountMinor, "paid", nr.AmountMinor)
		s.recordEvent(ctx, o, "amount_mismatch", nr.ChannelTxID, nr.AmountMinor, nr.RawPayload)
		return false, fmt.Errorf("金额不匹配: 订单=%d 实付=%d", o.AmountMinor, nr.AmountMinor)
	}

	// 3. 账户验证：回调的账户与订单记录的账户一致
	if nr.AccountID > 0 && o.AccountID > 0 && nr.AccountID != o.AccountID {
		logger.Error("order", "账户不匹配（重大安全事件）",
			"out_trade_no", outTradeNo,
			"order_account_id", o.AccountID,
			"callback_account_id", nr.AccountID)
		return false, fmt.Errorf("账户不匹配: 订单=%d 回调=%d", o.AccountID, nr.AccountID)
	}

	// 4a. 幂等：已支付直接返回成功（同一通道交易号重复回调）
	if o.Status == model.StatusSuccess {
		// 崩溃恢复：若事件/账本在上次结算中丢失（条件更新成功后进程崩溃），此处补写
		s.ensureRecorded(ctx, o, nr)
		// 备注码释放（幂等：重复释放无害）
		s.releaseNoteCode(o)
		// WAIT_PAY 池同步：确保已移出池（幂等；崩溃恢复场景兜底）
		s.notifyWaitPayRemove(o)
		logger.Info("order", "订单已支付，幂等返回", "out_trade_no", outTradeNo)
		return true, nil
	}

	// 4b. H9 迟到支付回调：订单已被超时关单（CLOSED），上游却在关单后送来支付成功通知。
	// Closed 为资金安全终态，禁止重开（state graph: StatusClosed={}）；
	// 但用户已付款真实存在，丢失证据=无法追讨/退款。落证据事件（不计入账本，避免凭空入账），告警待人工核查与上游对账。
	if o.Status == model.StatusClosed {
		lateEvent := fmt.Sprintf(`{"out_trade_no":%q,"amount":%d,"status":"LATE_PAID_AFTER_CLOSED","channel_tx_id":%q,"payer":%q,"device_id":%q,"account_id":%d,"time":%q}`,
			outTradeNo, o.AmountMinor, nr.ChannelTxID, nr.Payer, nr.DeviceID, nr.AccountID, time.Now().Format(time.RFC3339))
		if err := s.store.InsertEvent(ctx, o.ID, "late_paid_after_closed", nr.ChannelTxID, o.AmountMinor, lateEvent); err != nil {
			logger.Error("order", "LATE_PAID_AFTER_CLOSED 事件落库失败", "err", err, "out_trade_no", outTradeNo)
			return false, err
		}
		logger.Error("order", "H9 迟到支付：订单已关单但上游通知支付成功",
			"out_trade_no", outTradeNo, "channel_tx_id", nr.ChannelTxID,
			"amount", o.AmountMinor, "closed_event_id", o.ID, "notify_payload", nr.RawPayload)
		if s.alert != nil {
			s.alert.Send("error", "迟到支付(已关单)：需人工对账",
				fmt.Sprintf("订单 %s 已被网关关单，但上游 %s 通知支付成功 (channel_tx_id=%s, amount=%d)\n请核对上游商户后台并主动退款或补开，避免用户资金流失投诉",
					outTradeNo, nr.ChannelTxID, nr.ChannelTxID, o.AmountMinor))
		}
		// 返回 true 让通道收到 success 停止重试；资金不自动入账，等待人工对账
		return true, nil
	}

	// 5. 条件更新状态 WAIT_BUYER_PAY -> TRADE_SUCCESS（互斥锁：防并发双写账本/事件）
	ok, err := s.store.UpdateOrderStatus(ctx, outTradeNo, model.StatusWaitPay, model.StatusSuccess, func(upd *model.Order) {
		upd.ChannelTxID = nr.ChannelTxID
		upd.Payer = nr.Payer
		if nr.DeviceID != "" {
			upd.DeviceID = nr.DeviceID
		}
	})
	if err != nil {
		return false, err
	}
	if !ok {
		logger.Warn("order", "状态更新被拒绝（条件不满足）", "out_trade_no", outTradeNo, "from", o.Status)
		return false, nil
	}

	// 6. 资金流水 + 审计账本
	// 写入顺序说明：先事件后账本。任一失败都返回错误让通道重试回调；
	// 重试时进入上面的幂等分支，由 ensureRecorded 补写缺失部分（账本按 eventID 去重，不会重复记账）
	eventPayload := fmt.Sprintf(`{"out_trade_no":%q,"amount":%d,"status":"TRADE_SUCCESS","channel_tx_id":%q,"payer":%q,"device_id":%q,"account_id":%d,"time":%q}`,
		outTradeNo, o.AmountMinor, nr.ChannelTxID, nr.Payer, nr.DeviceID, nr.AccountID, time.Now().Format(time.RFC3339))
	if err := s.store.InsertEvent(ctx, o.ID, "paid", nr.ChannelTxID, o.AmountMinor, nr.RawPayload); err != nil {
		logger.Error("order", "资金流水写入失败（返回错误，等待通道重试）", "err", err, "out_trade_no", outTradeNo)
		return false, err
	}
	if err := s.ledger.Append(ctx, o.ID, eventPayload); err != nil {
		logger.Error("order", "账本写入失败（返回错误，等待通道重试，幂等分支将补写）", "err", err, "out_trade_no", outTradeNo)
		if s.alert != nil {
			s.alert.Send("error", "账本写入失败",
				fmt.Sprintf("订单 %s 入账但账本写入失败（重大）\n错误: %v\n幂等分支将补写，请关注", outTradeNo, err))
		}
		return false, err
	}
	logger.Info("order", "订单支付成功 ✅",
		"out_trade_no", outTradeNo,
		"pid", o.Pid,
		"upstream", o.UpstreamName,
		"amount", o.AmountMinor,
		"tx_id", nr.ChannelTxID)

	// 7. 创建回调任务（outbox）
	s.EnqueueNotify(ctx, o)
	// 备注码释放（订单终态 SUCCESS，备注码不再需要）
	s.releaseNoteCode(o)
	// WAIT_PAY 池同步：订单已入账，移出池
	s.notifyWaitPayRemove(o)
	return true, nil
}

// ensureRecorded 崩溃恢复补写：条件更新成功后进程若崩溃，事件/账本可能缺失。
// 在幂等分支调用：检查并按需补写，保证"资金事件必有账本记录"（账本按 eventID 去重，绝不重复记账）。
func (s *Service) ensureRecorded(ctx context.Context, o *model.Order, nr *channel.NotifyResult) {
	// 账本缺失 → 补写（eventID=订单ID 去重）
	has, err := s.ledger.HasEvent(ctx, o.ID)
	if err != nil {
		logger.Error("order", "检查账本记录失败（崩溃恢复）", "err", err, "out_trade_no", o.OutTradeNo)
		return
	}
	if !has {
		eventPayload := fmt.Sprintf(`{"out_trade_no":%q,"amount":%d,"status":"TRADE_SUCCESS","channel_tx_id":%q,"payer":%q,"device_id":%q,"account_id":%d,"time":%q,"recovered":true}`,
			o.OutTradeNo, o.AmountMinor, nr.ChannelTxID, nr.Payer, nr.DeviceID, nr.AccountID, time.Now().Format(time.RFC3339))
		if err := s.ledger.Append(ctx, o.ID, eventPayload); err != nil {
			logger.Error("order", "崩溃恢复：账本补写失败（严重，需人工核查）", "err", err, "out_trade_no", o.OutTradeNo)
		} else {
			logger.Warn("order", "崩溃恢复：账本已补写", "out_trade_no", o.OutTradeNo, "event_id", o.ID)
		}
	}
	// 事件缺失 → 补写（幂等：同一订单+交易号只记一条 paid 事件）
	if nr.ChannelTxID != "" {
		evs, err := s.store.ListEvents(ctx, o.ID, 50)
		if err == nil {
			found := false
			for _, e := range evs {
				if e.EventType == "paid" && e.ChannelTxID == nr.ChannelTxID {
					found = true
					break
				}
			}
			if !found {
				if err := s.store.InsertEvent(ctx, o.ID, "paid", nr.ChannelTxID, o.AmountMinor, nr.RawPayload); err != nil {
					logger.Error("order", "崩溃恢复：事件补写失败", "err", err, "out_trade_no", o.OutTradeNo)
				} else {
					logger.Warn("order", "崩溃恢复：资金事件已补写", "out_trade_no", o.OutTradeNo)
				}
			}
		}
	}
}

// CloseExpiredOrders 关闭超过 before 仍未支付的订单（定时任务调用）
// 关单前必查单（与 ConfirmScheduler 同策略）：已支付 → 补账；查单失败 → 跳过留待调度器/人工池；确认未支付 → 才关闭
// 返回本次关闭的订单数
func (s *Service) CloseExpiredOrders(ctx context.Context, before time.Time) (int, error) {
	expired, err := s.store.ListExpiredOrders(ctx, before)
	if err != nil {
		return 0, err
	}
	closed := 0
	// 扫描级设备在线缓存：同设备多个到期订单只做一次 adb 探测（Online 有连接开销）。
	// 仅缓存已绑定设备（DeviceID≠""）的结果；未绑定设备查任意可用设备，不缓存。
	type devProbe struct {
		offline bool
		reason  string
	}
	probeCache := map[string]devProbe{}
	for _, o := range expired {
		// ConfirmAbility 分叉：不可主动查单的通道（个人/监听）走安全关单
		if ch, ok := channel.Lookup(o.Channel); ok && ch.ConfirmAbility() != channel.ConfirmByQuery {
			// 动态关单窗口：池中记录的 CloseAt 未到 → 本次扫描跳过（窗口由设备负载决定，见 PLAN-DYNAMIC-CLOSE-ALLOC）
			if s.waitPay != nil && s.waitPay.CloseAtOf != nil {
				if ct, ok := s.waitPay.CloseAtOf(o.OutTradeNo); ok && time.Now().Before(ct) {
					continue
				}
			}
			// 关单前安全检查（与 confirmPersonal 同语义，防止两条关单路径安全检查分叉）：
			// 监听端离线 → 进异常池人工核实（用户可能已付款但未监听到），绝不直接关单。
			if s.personalCheck != nil {
				offline, reason := false, ""
				if o.DeviceID != "" {
					if pr, ok := probeCache[o.DeviceID]; ok {
						offline, reason = pr.offline, pr.reason
					} else {
						offline, reason = s.personalCheck(ctx, o)
						probeCache[o.DeviceID] = devProbe{offline: offline, reason: reason}
					}
				} else {
					offline, reason = s.personalCheck(ctx, o)
				}
				if offline {
					s.EnqueueException(ctx, o, model.ExReasonListenOffline, "LISTEN_OFFLINE", "",
						fmt.Sprintf(`{"action":"close_scan_offline","time":%q}`, time.Now().Format(time.RFC3339)),
						"监听端掉线，无法确认用户是否已付款，需人工核实： "+reason)
					continue
				}
			}
			// 监听端在线但此前已进过 LISTEN_TIMEOUT 异常池（复查过仍无推送）→ 不进异常池重复核查，
			// 说明监听端僵死风险已存在：不直接关单，交由人工核实（与 confirmPersonal 的复查语义一致）。
			if ex, _ := s.store.GetExceptionByOutTradeNo(ctx, o.OutTradeNo); ex != nil && ex.Reason == model.ExReasonListenTimeout {
				s.EnqueueException(ctx, o, model.ExReasonListenTimeout, "LISTEN_TIMEOUT", "",
					fmt.Sprintf(`{"action":"close_scan_stalled","time":%q}`, time.Now().Format(time.RFC3339)),
					"监听端在线但长时间未收到收款推送（扫描器复查命中历史 LISTEN_TIMEOUT），需人工确认实际收款情况")
				continue
			}
			if s.closeExpiredOrder(ctx, o, nil, "") {
				closed++
			}
			continue
		}
		// 商家通道关单前必查单；只查订单所属通道避免跨通道命中同名订单
		// 到期判定：ORDER_CLOSE_MINUTES（商家通道固定关单时间，不受个人免签动态窗口影响）。
		// 扫描器候选集可能被动态窗口放宽（max(商家,动态MAX)+裕量），此处二次过滤真正到期订单。
		if !s.isExpiredWindow(o) {
			continue
		}
		st, qerr := s.queryOrderFor(ctx, o.OutTradeNo, o.Channel)
		if qerr == nil && st != nil && st.Status == model.StatusSuccess {
			if _, serr := s.SettleByQuery(ctx, o.OutTradeNo, st.ChannelTxID, st.AmountMinor,
				fmt.Sprintf(`{"action":"close_scan_found_paid","time":%q}`, time.Now().Format(time.RFC3339))); serr != nil {
				logger.Error("order", "扫描关单前查单发现已支付但补账失败（转人工核查）",
					"err", serr, "out_trade_no", o.OutTradeNo)
			}
			continue
		}
		if qerr != nil && !errors.Is(qerr, ErrNotQueryable) {
			// 查单失败绝不关单（资金安全）。但不能每轮扫描都重查+重 WARN 同一批卡单：
			// 首次失败入人工池（close_uncertain），已存在则静默跳过，交人工池处理。
			if ex, gerr := s.store.GetExceptionByOutTradeNo(ctx, o.OutTradeNo); gerr == nil && ex != nil {
				continue
			}
			s.EnqueueException(ctx, o, model.ExReasonCloseUncertain, "QUERY_FAILED", "",
				fmt.Sprintf(`{"action":"close_scan_query_failed","time":%q,"err":%q}`,
					time.Now().Format(time.RFC3339), qerr.Error()),
				"关单扫描查单失败，无法确认是否已支付，需人工核对后处理")
			continue
		}
		if s.closeExpiredOrder(ctx, o, st, "") {
			closed++
		}
	}
	if closed > 0 {
		logger.Info("order", "自动关闭扫描完成", "closed", closed)
	}
	return closed, nil
}

// closeExpiredOrder 执行单笔超时关单：st=nil 表示无法/未查单（直接关），st != nil 表示已查过确认未支付。
func (s *Service) closeExpiredOrder(ctx context.Context, o *model.Order, st *channel.OrderStatus, evidenceJSON string) bool {
	ok, err := s.store.UpdateOrderStatus(ctx, o.OutTradeNo, model.StatusWaitPay, model.StatusClosed, nil)
	if err != nil {
		logger.Error("order", "关闭订单失败", "err", err, "out_trade_no", o.OutTradeNo)
		return false
	}
	if !ok {
		return false
	}
	rawPayload := fmt.Sprintf(`{"reason":"timeout","queried":%t%s}`, st != nil, evidenceJSON)
	if err := s.store.InsertEvent(ctx, o.ID, "closed", "", o.AmountMinor, rawPayload); err != nil {
		logger.Warn("order", "写关闭事件失败", "err", err, "out_trade_no", o.OutTradeNo)
	}
	s.releaseNoteCode(o)
	// WAIT_PAY 池同步：订单已关单，移出池
	s.notifyWaitPayRemove(o)
	logger.Info("order", "超时未支付订单已关闭", "out_trade_no", o.OutTradeNo, "id", o.ID)
	return true
}

// CloseOrder 单笔幂等关单（条件更新 WAIT→CLOSED，写关闭事件审计）
// 供 ConfirmScheduler / 人工池 / 查单确认"未支付"后调用
func (s *Service) CloseOrder(ctx context.Context, outTradeNo string, reason string) (bool, error) {
	o, err := s.store.GetOrder(ctx, outTradeNo)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, nil
	}
	if o.Status == model.StatusClosed {
		return true, nil // 幂等
	}
	ok, err := s.store.UpdateOrderStatus(ctx, outTradeNo, model.StatusWaitPay, model.StatusClosed, nil)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil // 已被并发处理（如刚支付成功）
	}
	if err := s.store.InsertEvent(ctx, o.ID, "closed", "", o.AmountMinor,
		fmt.Sprintf(`{"reason":%q,"time":%q}`, reason, time.Now().Format(time.RFC3339))); err != nil {
		logger.Warn("order", "写关闭事件失败", "err", err, "out_trade_no", outTradeNo)
	}
	// 备注码释放（订单终态 CLOSED）
	s.releaseNoteCode(o)
	// WAIT_PAY 池同步：订单已关单，移出池
	s.notifyWaitPayRemove(o)
	logger.Info("order", "订单已关闭", "out_trade_no", outTradeNo, "reason", reason)
	return true, nil
}

// SettleByQuery 查单补账：查单确认已支付后，走 SettlePayment 幂等入账
// 金额不一致 → 进异常池（amount_mismatch），绝不自动入账
func (s *Service) SettleByQuery(ctx context.Context, outTradeNo string, channelTxID string, paidAmount int64, evidence string) (bool, error) {
	o, err := s.store.GetOrder(ctx, outTradeNo)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, nil
	}
	// 金额校验：查单金额与订单金额不一致 → 进异常池，不自动入账
	if paidAmount != 0 && paidAmount != o.AmountMinor {
		logger.Error("order", "查单金额与订单不一致（重大）", "out_trade_no", outTradeNo,
			"order_amount", o.AmountMinor, "query_amount", paidAmount)
		s.EnqueueException(ctx, o, model.ExReasonAmountMismatch, model.StatusSuccess, channelTxID,
			evidence, fmt.Sprintf("查单金额 %d 与订单金额 %d 不一致", paidAmount, o.AmountMinor))
		return false, fmt.Errorf("查单金额不匹配: 订单=%d 查单=%d", o.AmountMinor, paidAmount)
	}
	// 幂等入账（走 SettlePayment：状态机 + 账本 + 通知）
	return s.SettlePayment(ctx, &channel.NotifyResult{
		OutTradeNo:  outTradeNo,
		ChannelTxID: channelTxID,
		AmountMinor: paidAmount,
		RawPayload:  evidence,
	})
}

// SettleManual 人工确认入账（管理员授权操作，允许金额覆盖实际支付额）
// 与 SettlePayment 同路径：状态机 + 账本 hash 链 + 创建通知任务，全程审计
func (s *Service) SettleManual(ctx context.Context, outTradeNo, channelTxID string, paidAmount int64, payer, handler, note string) (bool, error) {
	o, err := s.store.GetOrder(ctx, outTradeNo)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, nil
	}
	if o.Status == model.StatusSuccess {
		// 崩溃恢复：若账本在上次人工入账中丢失（条件更新成功后进程崩溃），此处补写
		s.ensureRecorded(ctx, o, &channel.NotifyResult{ChannelTxID: channelTxID})
		// 备注码释放（幂等：重复释放无害）
		s.releaseNoteCode(o)
		logger.Info("order", "订单已支付，人工入账幂等返回", "out_trade_no", outTradeNo)
		return true, nil
	}
	// 终态保护：已关单订单禁止人工入账重开（与 SettlePayment H9 守卫一致）
	if o.Status == model.StatusClosed {
		logger.Error("order", "人工入账拒绝：订单已关单", "out_trade_no", outTradeNo, "amount", paidAmount, "handler", handler)
		if s.alert != nil {
			s.alert.Send("error", "人工入账被拒(已关单)",
				fmt.Sprintf("管理员 %s 试图人工入账已关单订单 %s (amount=%d)\n关单终态不可重开，请走 H9 迟到支付证据流程或上游对账退款",
					handler, outTradeNo, paidAmount))
		}
		return false, fmt.Errorf("订单 %s 已关单，禁止人工入账", outTradeNo)
	}
	// 互斥锁：条件更新（防并发双写账本/事件）
	ok, err := s.store.UpdateOrderStatus(ctx, outTradeNo, model.StatusWaitPay, model.StatusSuccess, func(upd *model.Order) {
		if channelTxID != "" {
			upd.ChannelTxID = channelTxID
		}
		if payer != "" {
			upd.Payer = payer
		}
	})
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil // 状态非法（如已关闭）
	}
	amount := paidAmount
	if amount <= 0 {
		amount = o.AmountMinor
	}
	eventPayload := fmt.Sprintf(`{"out_trade_no":%q,"amount":%d,"status":"TRADE_SUCCESS","channel_tx_id":%q,"source":"manual","handler":%q,"note":%q,"time":%q}`,
		outTradeNo, amount, channelTxID, handler, note, time.Now().Format(time.RFC3339))
	// 先事件后账本；任一失败返回错误，重试时由幂等分支 ensureRecorded 补写
	if err := s.store.InsertEvent(ctx, o.ID, "paid_manual", channelTxID, amount, eventPayload); err != nil {
		logger.Error("order", "人工入账流水写入失败（等待重试）", "err", err, "out_trade_no", outTradeNo)
		return false, err
	}
	if err := s.ledger.Append(ctx, o.ID, eventPayload); err != nil {
		logger.Error("order", "人工入账账本写入失败（等待重试，幂等分支将补写）", "err", err, "out_trade_no", outTradeNo)
		if s.alert != nil {
			s.alert.Send("error", "人工入账账本写入失败",
				fmt.Sprintf("订单 %s 人工入账但账本写入失败（重大）\n错误: %v", outTradeNo, err))
		}
		return false, err
	}
	logger.Warn("order", "人工确认入账（管理员操作）", "out_trade_no", outTradeNo,
		"amount", amount, "handler", handler, "pid", o.Pid, "upstream", o.UpstreamName)
	// 创建通知任务（易支付协议：通知金额用订单金额）
	s.EnqueueNotify(ctx, o)
	// 备注码释放（订单终态 SUCCESS）
	s.releaseNoteCode(o)
	// WAIT_PAY 池同步：订单已入账，移出池
	s.notifyWaitPayRemove(o)
	return true, nil
}

// EnqueueException 订单进人工异常池（幂等：同单已存在则仅补审计）
func (s *Service) EnqueueException(ctx context.Context, o *model.Order, reason, channelStatus, channelTxID, evidence, suggestion string) {
	e := &model.ExceptionOrder{
		OutTradeNo:    o.OutTradeNo,
		Pid:           o.Pid,
		UpstreamName:  o.UpstreamName,
		Channel:       o.Channel,
		AmountMinor:   o.AmountMinor,
		OrderStatus:   o.Status,
		ChannelStatus: channelStatus,
		ChannelTxID:   channelTxID,
		Reason:        reason,
		Evidence:      evidence,
		Suggestion:    suggestion,
	}
	if err := s.store.CreateException(ctx, e); err != nil {
		logger.Error("order", "创建异常单失败", "err", err, "out_trade_no", o.OutTradeNo)
		return
	}
	logger.Warn("order", "订单进入人工异常池", "out_trade_no", o.OutTradeNo,
		"reason", reason, "channel_status", channelStatus)
	// 告警：进池事件（RESILIENCE P1-3）
	if s.alert != nil {
		s.alert.Send("error", "异常池进单",
			fmt.Sprintf("订单 %s 进入人工异常池\n原因: %s\n通道状态: %s\n建议: %s",
				o.OutTradeNo, reason, channelStatus, suggestion))
	}
}

// BuildNotifyPayload 构造易支付协议回调参数并签名，返回 JSON payload。
// 供 EnqueueNotify（下单入队）与死信手动重发（用订单最新 notify_url）复用，保证字段/签名单一来源。
func (s *Service) BuildNotifyPayload(o *model.Order) string {
	params := map[string]string{
		"trade_no":     o.GatewayNo,
		"out_trade_no": o.OutTradeNo,
		"type":         o.Channel,
		"name":         o.Subject,
		"money":        o.MoneyYuan(),
		"trade_status": "TRADE_SUCCESS",
		// notify_url 必须进 payload：worker 用它作为发送地址，
		// 且参与签名（易支付协议标准），New API 验签时用收到的全部参数重算，保证一致
		"notify_url": o.NotifyURL,
	}
	// attach（上游请求ID）原样带回，参与签名，供上游对账
	if o.Attach != "" {
		params["attach"] = o.Attach
	}
	if o.ChannelTxID != "" {
		params["trade_no"] = o.ChannelTxID // 优先用通道交易号（易支付协议语义）
	}
	epay.BuildSignedParams(params, s.signKey(o.Pid))
	payload, _ := json.Marshal(params)
	return string(payload)
}

// EnqueueNotify 创建回调接入方任务（outbox）
func (s *Service) EnqueueNotify(ctx context.Context, o *model.Order) {
	payload := s.BuildNotifyPayload(o)
	taskID, err := s.store.CreateNotifyTask(ctx, o.ID, payload)
	if err != nil {
		logger.Error("order", "创建回调任务失败", "err", err, "out_trade_no", o.OutTradeNo)
		return
	}
	logger.Info("order", "回调任务已入队", "task_id", taskID, "out_trade_no", o.OutTradeNo)
}

// recordEvent 记录资金流水
func (s *Service) recordEvent(ctx context.Context, o *model.Order, eventType, txID string, amount int64, raw string) {
	if err := s.store.InsertEvent(ctx, o.ID, eventType, txID, amount, raw); err != nil {
		logger.Error("order", "写入资金流水失败", "err", err, "out_trade_no", o.OutTradeNo, "event", eventType)
	}
}

// QueryOrder 查单（易支付协议 /api.php）
func (s *Service) QueryOrder(ctx context.Context, outTradeNo string) (*model.Order, error) {
	return s.store.GetOrder(ctx, outTradeNo)
}

// ---------- WAIT_PAY 池同步钩子（个人免签通道，资金判定不依赖钩子） ----------

// notifyWaitPayAdd 订单进入 WAIT_PAY 且已绑定账户 → 通知池（仅 personal.* 通道）
func (s *Service) notifyWaitPayAdd(o *model.Order) {
	if s.waitPay == nil || s.waitPay.Add == nil || o == nil || o.AccountID <= 0 {
		return
	}
	if !strings.HasPrefix(o.Channel, "personal.") {
		return
	}
	s.waitPay.Add(o)
}

// notifyWaitPayUpsert 订单凭据刷新/换通道（WAIT_PAY 内 NoteCode/AccountID 变化）→ 覆盖更新池
func (s *Service) notifyWaitPayUpsert(o *model.Order) {
	if s.waitPay == nil || s.waitPay.Upsert == nil || o == nil || o.AccountID <= 0 {
		return
	}
	if !strings.HasPrefix(o.Channel, "personal.") {
		return
	}
	s.waitPay.Upsert(o)
}

// notifyWaitPayRemove 订单离开 WAIT_PAY（入账/关单终态）→ 通知池移除
func (s *Service) notifyWaitPayRemove(o *model.Order) {
	if s.waitPay == nil || s.waitPay.Remove == nil || o == nil {
		return
	}
	if !strings.HasPrefix(o.Channel, "personal.") {
		return
	}
	s.waitPay.Remove(o.OutTradeNo)
}
