// paygateway 支付网关 ledger（从 main.go 拆分，账本监听/匹配引擎）
package main

import (
	"context"
	"fmt"
	"paygateway/internal/account"
	"paygateway/internal/admin/capture"
	"paygateway/internal/channel"
	"paygateway/internal/ledgermon"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/personal"
	"paygateway/internal/pool"
	"paygateway/internal/store"
	"strings"
	"time"
)

func (a *App) newLedgerReader(d ledgermon.MonitoredDevice) ledgermon.LedgerReader {
	switch d.Provider {
	case "alipay":
		return ledgermon.NewAlipayReader(adbMgr(), d.Serial, d.UserID)
	case "wechat":
		return ledgermon.NewWechatReader(adbMgr(), d.Serial, d.UserID)
	}
	return nil
}

// dispatchLedgerRows 账本新收款行推送（ledgermon.DispatchFunc 实现）：
// 复用个人通道匹配入账链路（/personal/push 同款），source=ledger，窗口 30min（账本轮询兜底）。
// 匹配顺序（池消费模型，每笔金额独立匹配池中订单）：
//  1. 备注码精确匹配（微信账本行自带"付款方备注"；支付宝账本页无备注需探测）
//  2. 金额+时间窗口匹配：唯一候选 → 直接命中（免探测）；零候选 → no_order 转人工
//  3. 多候选歧义：支付宝可点金额进详情页探测备注码消歧（探测失败/无备注码 → 保持 ambiguous 转人工，
//     禁止自动挑选——资金安全不变量）
//
// 注：探测在 DeviceActor 串行窗口内执行（PollCmd.Execute → Dispatch），无 adb 并发冲突。
func (a *App) dispatchLedgerRows(ctx context.Context, deviceID string, provider string, space int, rows []capture.AlipayLedgerRow) error {
	// 1. 设备加载（验签不需要；匹配链路需要 dev.ID/AccountID 解析账户集合）
	dev, err := a.personalDeviceCtx(ctx, deviceID)
	if err != nil {
		logger.Warn("ledgermon", "账本推送：设备加载失败", "err", err, "device_id", deviceID)
		return err
	}
	if dev.Status == personal.DeviceFrozen {
		logger.Warn("ledgermon", "账本推送：设备已冻结，跳过", "device_id", deviceID)
		return nil
	}
	// 2. 逐行匹配入账（窗口 30min：账本轮询兜底语义，覆盖读取间隔+推送延迟）
	window := 30 * time.Minute
	pushed := false
	for _, r := range rows {
		// 幂等去重：同一设备同原文哈希只处理一次（fingerprint=时间|金额，同指纹跨轮去重）
		raw := fmt.Sprintf("ledger:%s|%s|%s", r.Payer, r.Time, r.Amount)
		rawHash := fmt.Sprintf("%x", sha256Hash([]byte(raw)))
		dup, err := a.personalDB.PushExists(ctx, deviceID, rawHash)
		if err != nil {
			logger.Error("ledgermon", "账本推送去重查询失败", "err", err, "device_id", deviceID)
			continue
		}
		if dup {
			continue
		}
		// 大额告警（对齐原 /personal/push 链路）：账本行金额超阈值 → 告警留痕。
		// listener 下线后服务端是唯一告警点，走 alertC（原实现仅 logger，此处升级）。
		if a.cfg != nil && a.cfg.PersonalBigAmount > 0 && r.AmountMinor >= a.cfg.PersonalBigAmount {
			logger.Warn("ledgermon", "大额收款告警",
				"device_id", deviceID, "provider", provider,
				"amount", r.AmountMinor, "row", raw, "threshold", a.cfg.PersonalBigAmount)
			if a.alertC != nil {
				a.alertC.Send("warn", "大额收款",
					fmt.Sprintf("账本监听：设备 %s 收到 %d 分，需留意", deviceID, r.AmountMinor))
			}
		}
		orderNo, matched, orderAccountID, effNote, err := a.matchLedgerRow(ctx, dev, provider, space, r, window, a.ledgerProber())
		if err != nil {
			logger.Error("ledgermon", "账本订单匹配失败", "err", err, "device_id", deviceID, "amount", r.AmountMinor)
			continue
		}
		r.Note = effNote // 审计：流水记录实际用于消歧的备注码（探测命中时非空）
		// 记录推送流水（审计，先记后入账）
		push := &personal.Push{
			DeviceID: deviceID, Provider: provider, AmountMinor: r.AmountMinor,
			Note: r.Note, RawHash: rawHash, Raw: raw,
			MatchedOrder: orderNo, MatchResult: matched, Space: space,
		}
		if err := a.personalDB.InsertPush(ctx, push); err != nil {
			logger.Error("ledgermon", "账本推送流水写入失败", "err", err, "device_id", deviceID)
		}
		pushed = true
		switch matched {
		case "matched":
			// 账户冻结守卫（对齐原 /personal/push 链路）：冻结账户不自动入账，转异常池人工核实
			//（钱已在账户里，不会丢；只是不自动确认）。原实现仅 continue 导致行只进审计流水
			// 不进异常池、且指纹已提交永不再现——2026-08-30 迁移补 EnqueueException 保全核查路径。
			if acct := a.accountPool.GetByID(orderAccountID); acct != nil && acct.Status == account.StatusFrozen {
				logger.Warn("ledgermon", "账户已冻结，账本收款转人工", "order", orderNo, "account_id", orderAccountID)
				if cur, cerr := a.store.GetOrder(ctx, orderNo); cerr == nil && cur != nil {
					a.orders.EnqueueException(ctx, cur, model.ExReasonListenOffline, "PAID_WHILE_FROZEN",
						"", raw, "账本监听：收款到达时账户处于冻结态，需人工核实际收款后入账")
				}
				continue
			}
			nr := &channel.NotifyResult{
				Valid:       true,
				OutTradeNo:  orderNo,
				ChannelTxID: fmt.Sprintf("LEDGER-%s-%d", deviceID, time.Now().UnixNano()),
				Status:      model.StatusSuccess,
				AmountMinor: r.AmountMinor,
				Payer:       "ledger:" + deviceID,
				DeviceID:    deviceID,
				AccountID:   orderAccountID,
				RawPayload:  raw,
			}
			if ok, serr := a.orders.SettlePayment(ctx, nr); serr != nil {
				logger.Error("ledgermon", "账本入账失败", "err", serr, "order", orderNo)
				continue
			} else if !ok {
				// ok=false, serr=nil：并发已支付/已关单（幂等），非异常不重复入异常池
				if cur, cerr := a.store.GetOrder(ctx, orderNo); cerr == nil && cur != nil && cur.Status == model.StatusClosed {
					a.orders.EnqueueException(ctx, cur, model.ExReasonStateConflict, "TRADE_SUCCESS_AFTER_CLOSED",
						nr.ChannelTxID, raw, "账本监听：关单后收到收款记录，需人工核对实际收款")
				}
				continue
			}
			// 回写订单 device_id（审计追溯）
			if _, uerr := a.store.UpdateOrderStatus(ctx, orderNo, model.StatusSuccess, model.StatusSuccess, func(upd *model.Order) {
				upd.DeviceID = deviceID
			}); uerr != nil {
				logger.Warn("ledgermon", "回写订单 device_id 失败", "err", uerr, "order", orderNo)
			}
			a.confirmSched.OnSettled(orderNo)
			logger.Info("ledgermon", "账本监听收款入账成功 ✅",
				"order", orderNo, "amount", r.AmountMinor, "device", deviceID, "row", raw)
		case "settled_dup":
			// 同一笔收款已被通知栏/账本另一链路入账（真机实测 5s 内双链同捕）
			logger.Info("ledgermon", "账本重复捕获，另一链路已入账（幂等跳过）",
				"order", orderNo, "amount", r.AmountMinor, "device", deviceID)
		case "no_order":
			a.saveUnmatched(ctx, provider, r.AmountMinor, r.Note, raw, r.Payer, deviceID)
			logger.Warn("ledgermon", "账本无订单收款（需人工处理）", "device", deviceID, "amount", r.AmountMinor)
		default: // ambiguous
			a.saveUnmatched(ctx, provider, r.AmountMinor, r.Note, raw, r.Payer, deviceID)
			logger.Warn("ledgermon", "账本多订单歧义（需人工确认）", "device", deviceID, "amount", r.AmountMinor)
		}
	}
	// 有推送流水则刷新 last_push（管理后台展示真实推送时间；listener 下线后由账本链路维护）
	if pushed {
		if err := a.personalDB.SetDevicePushTime(ctx, deviceID); err != nil {
			logger.Warn("ledgermon", "更新设备推送时间失败", "err", err, "device_id", deviceID)
		}
	}
	return nil
}

// alipayNoteProber 支付宝备注码探测函数（多候选歧义消解用；注入便于测试）
// 生产 = probeAlipayLedgerNote（adb 详情页探测）；测试 = 桩返回固定备注码。
type alipayNoteProber func(ctx context.Context, dev *personal.Device, bounds string) (string, error)

// matchLedgerRow 单账本行池消费匹配：
//  1. matchPersonalOrder：备注码精确（微信行自带）> 金额+时间窗口（唯一候选直接命中，免探测）
//  2. 多候选歧义（支付宝且行有金额坐标）→ prober 探测详情页备注码 → 重新匹配消歧
//  3. 探测失败/无备注码/重新匹配仍歧义 → 保持 ambiguous（禁止自动挑选，资金安全）
//
// 返回 (订单号, 匹配结果, 订单账户ID, 实际备注码, error)。实际备注码供审计流水记录
// （探测命中时非空，未命中沿用行内 Note）。
func (a *App) matchLedgerRow(ctx context.Context, dev *personal.Device, provider string, space int, r capture.AlipayLedgerRow, window time.Duration, prober alipayNoteProber) (string, string, int64, string, error) {
	orderNo, matched, orderAccountID, err := a.matchPersonalOrder(ctx, dev, provider, r.Note, r.AmountMinor, time.Now(), window, space)
	if err != nil {
		return "", "", 0, r.Note, err
	}
	effNote := r.Note
	// 多候选歧义消解：支付宝账本行无备注 → 探测详情页备注码后重新匹配（池消费模型第 3 步）。
	// 探测耗时 ≈3.5s/行（点击+dump+返回），仅歧义行触发（金额唯一已短路免探测）。
	if matched == "ambiguous" && provider == "alipay" && r.AmountBounds != "" && prober != nil && a.waitPayPool != nil {
		probed, perr := prober(ctx, dev, r.AmountBounds)
		if perr != nil {
			logger.Warn("ledgermon", "支付宝备注码探测失败，保持多候选转人工",
				"err", perr, "device_id", dev.DeviceID, "amount", r.AmountMinor)
		} else if probed != "" {
			orderNo2, matched2, acct2, err2 := a.matchPersonalOrder(ctx, dev, provider, probed, r.AmountMinor, time.Now(), window, space)
			if err2 != nil {
				logger.Error("ledgermon", "探测备注码重新匹配失败", "err", err2, "device_id", dev.DeviceID, "amount", r.AmountMinor)
			} else if matched2 == "matched" {
				effNote = probed
				logger.Info("ledgermon", "支付宝备注码探测消歧成功 ✅",
					"order", orderNo2, "amount", r.AmountMinor, "note", probed, "device", dev.DeviceID)
				return orderNo2, matched2, acct2, effNote, nil
			}
		}
	}
	return orderNo, matched, orderAccountID, effNote, nil
}

// ledgerProber 返回备注码探测函数：测试注入桩优先（alipayProber 字段），nil 回退生产 adb 探针。
func (a *App) ledgerProber() alipayNoteProber {
	if a.alipayProber != nil {
		return a.alipayProber
	}
	return a.probeAlipayLedgerNote
}

// probeAlipayLedgerNote 生产备注码探针：adb 详情页探测（DeviceActor 串行窗口内执行，无并发冲突）。
// 无可用 serial 时返回错误（调用方保持歧义转人工，不阻塞入账）。
func (a *App) probeAlipayLedgerNote(ctx context.Context, dev *personal.Device, bounds string) (string, error) {
	serial := deviceSerialFromDevice(dev)
	if serial == "" {
		return "", fmt.Errorf("设备无可用 adb serial")
	}
	return capture.ProbeAlipayNote(ctx, adbMgr(), serial, bounds)
}
func (a *App) personalDeviceCtx(ctx context.Context, deviceID string) (*personal.Device, error) {
	enc, err := a.personalDB.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if enc == nil {
		return nil, fmt.Errorf("设备未登记: %s", deviceID)
	}
	secret, err := decryptDeviceSecret(enc.SecretEnc)
	if err != nil {
		return nil, fmt.Errorf("解密设备密钥失败: %w", err)
	}
	return &personal.Device{
		ID: enc.ID, DeviceID: enc.DeviceID, Name: enc.Name,
		AccountID: enc.AccountID, Provider: enc.Provider, Secret: secret,
		Status:        personal.DeviceStatus(enc.Status),
		AdbHost:       enc.AdbHost,
		AdbPort:       enc.AdbPort,
		AdbWifiHost:   enc.AdbWifiHost,
		AdbWifiPort:   enc.AdbWifiPort,
		SetupProgress: enc.SetupProgress,
		TsIP:          enc.TsIP,
		AdbListen:     enc.AdbListen != 0,
		ControlState:  enc.ControlState,
	}, nil
}

// matchPersonalOrder 匹配订单（通知栏监听方案：备注码可选加速 + 金额时间窗口）
//  1. 备注码精确匹配：note 非空且命中唯一 WAIT_PAY 订单 → 直接命中（通知栏方案下通常为空，自禁用）
//  2. 金额+时间窗口：候选唯一 → matched；多个 → ambiguous（转人工，禁止自动挑选）；零个 → no_order
//
// 时间窗口语义（2026-08-30 通知栏方案修正）：
//   - anchor 为收款发生时刻（账本链路传 time.Now()；通知栏链路传通知 when 毫秒时间戳）；
//   - 窗口 = [anchor-30min, anchor]：订单必须创建于收款之前（收款时订单必已存在），
//     且不早于窗口下界（防止远古订单被新收款误匹配）。
//     不能用纯下界（通知不清理会一直在，旧通知会错误匹配之后创建的新订单）。
//
// 返回 (订单号, 匹配结果, 订单账户ID, error)；匹配结果: matched / no_order / ambiguous
// 订单账户ID 用于入账账本记录（区分主账户/附加账户收款），0 表示未匹配成功。
func (a *App) matchPersonalOrder(ctx context.Context, dev *personal.Device, provider, note string, amountMinor int64, anchor time.Time, window time.Duration, space int) (string, string, int64, error) {
	since := anchor.Add(-window)
	// 设备可服务的账户集合（主账户 + 附加账户，应用双开多账户）：
	// 匹配时订单账户必须属于该集合（防串单），通道校验也按该集合的通道判断。
	acctIDs := store.DeviceAccountIDs(ctx, a.personalDB, dev.ID, dev.AccountID)
	inAcctIDs := func(id int64) bool {
		for _, v := range acctIDs {
			if v == id {
				return true
			}
		}
		return false
	}
	// 空间账户定位：按通知来源空间(0主/999双开) + 设备 + 平台 定位该空间专属账户。
	// 通过 device_qrcodes 拿该空间的收款码 → 匹配账户池中同收款码的账户，
	// 匹配时优先只在该账户的订单中找（区分双开账户收款，杜绝跨空间错配）。
	spaceAcctID := a.accountIDForSpace(ctx, dev, provider, space)
	if spaceAcctID > 0 && !inAcctIDs(spaceAcctID) {
		spaceAcctID = 0 // 空间账户不在设备绑定集合 → 不启用空间过滤（数据不一致兜底）
	}
	// 设备可服务通道集合（绑定账户的通道，如 {personal.alipay, personal.wechat}）
	servesChannel := func(ch string) bool {
		if !strings.HasPrefix(ch, "personal.") {
			return false
		}
		for _, id := range acctIDs {
			if acct := a.accountPool.GetByID(id); acct != nil && acct.Channel == ch {
				return true
			}
		}
		return false
	}

	// 1. 备注码精确匹配（首选，资金不错配核心）：
	// 内存池索引优先（空间账户定位 + noteCode 精确命中，零 DB）；
	// 池未命中/池不可用 → 回退 DB GetOrderByNoteCode（DB 是真相，两路径结果等价）。
	// 命中后统一 DB 二次校验（状态仍 WAIT_PAY 才采信）+ 通道/账户校验。
	if note != "" {
		// 优先池匹配（生产路径；测试环境无池自动回退 DB）
		var noteTarget string
		if e := a.noteMatchFromPool(dev, provider, space, note, since, spaceAcctID); e != nil {
			noteTarget = e.OutTradeNo
		} else if o, err := a.store.GetOrderByNoteCode(ctx, note, since); err == nil && o != nil {
			noteTarget = o.OutTradeNo
		}
		if noteTarget != "" {
			// DB 二次校验：订单仍待支付才采信（池可能落后于 DB 状态变更）
			o, err := a.store.GetOrder(ctx, noteTarget)
			if err != nil {
				return "", "", 0, err
			}
			if o != nil && o.Status == model.StatusWaitPay {
				// 校验通道 + 账户双重匹配：订单账户必须 ∈ 设备绑定账户集合（防串单）
				if servesChannel(o.Channel) && inAcctIDs(o.AccountID) {
					return o.OutTradeNo, "matched", o.AccountID, nil
				}
				logger.Warn("personal", "备注码命中的订单账户设备不可服务", "note", note,
					"order_channel", o.Channel, "order_account", o.AccountID, "device_id", dev.DeviceID)
			}
		}
	}

	// 2. 金额 + 时间窗口匹配（通知栏监听方案主路径：仅 包名+金额+时间 信号）
	// 候选收集后按数量分派：0→no_order；1→matched；≥2→ambiguous 转人工。
	// 空间账户已定位（spaceAcctID>0）时不回退设备集合：钱落在该空间的收款码上，
	// 其他账户的订单不可能是归属方，回退 = 跨账户错配。
	orders, err := a.store.GetOrdersByAmount(ctx, amountMinor, since)
	if err != nil {
		return "", "", 0, err
	}
	var candidates []*model.Order
	for _, o := range orders {
		if !servesChannel(o.Channel) {
			continue
		}
		if o.AccountID > 0 && !inAcctIDs(o.AccountID) {
			continue
		}
		if spaceAcctID > 0 && o.AccountID != spaceAcctID {
			continue
		}
		// 上界过滤：订单必须创建于收款时刻之前（收款时订单必已存在）。
		// 纯下界会允许"通知之后创建的新订单"被旧通知误匹配（通知不清理会一直在）。
		if o.CreatedAt.After(anchor) {
			continue
		}
		candidates = append(candidates, o)
	}
	switch len(candidates) {
	case 0:
		// 零 WAIT_PAY 候选 ≠ 无订单：通知栏/账本双链 5s 内同时捕获同一笔收款时，
		// 胜者已把订单结算（TRADE_SUCCESS），败者这时匹配查不到 WAIT_PAY。
		// 回查本设备可服务账户的同金额最近已支付订单（按账户过滤，防同金额异地/他户撞车）：
		//   - 过滤后又按空间账户精确限缩：1 笔 → settled_dup（另一链路已入账，幂等跳过）
		//   - 0 笔 → 真无订单（no_order）  ≥2 笔 → 同账户金额歧义，保持 no_order（资金安全，不猜，转人工）
		settled, serr := a.store.GetRecentSettledByAmount(ctx, amountMinor, since, acctIDs)
		if serr == nil {
			var owned []*model.Order
			for _, o := range settled {
				if servesChannel(o.Channel) && (o.AccountID == 0 || inAcctIDs(o.AccountID)) {
					if spaceAcctID > 0 && o.AccountID != spaceAcctID {
						continue
					}
					owned = append(owned, o)
				}
			}
			if len(owned) == 1 {
				logger.Info("personal", "重复捕获判定：同账户订单已由另一链路入账，幂等跳过",
					"order", owned[0].OutTradeNo, "amount", amountMinor, "device", dev.DeviceID)
				return owned[0].OutTradeNo, "settled_dup", owned[0].AccountID, nil
			}
			if len(owned) > 1 {
				logger.Warn("personal", "同账户同金额多笔已支付，不可归属，保持 no_order 转人工",
					"amount", amountMinor, "device", dev.DeviceID, "settled", len(owned))
				return "", "no_order", 0, nil
			}
		}
		return "", "no_order", 0, nil
	case 1:
		logger.Info("personal", "金额+时间窗口匹配成功", "order", candidates[0].OutTradeNo,
			"amount", amountMinor, "device", dev.DeviceID, "account_id", candidates[0].AccountID)
		return candidates[0].OutTradeNo, "matched", candidates[0].AccountID, nil
	default:
		ids := make([]string, 0, len(candidates))
		for _, o := range candidates {
			ids = append(ids, fmt.Sprintf("%s(acct=%d)", o.OutTradeNo, o.AccountID))
		}
		logger.Warn("personal", "同金额多候选，拒绝自动入账转人工",
			"amount", amountMinor, "device", dev.DeviceID, "candidates", strings.Join(ids, ", "))
		return "", "ambiguous", 0, nil
	}
}

// noteMatchFromPool 备注码池匹配：优先空间账户（spaceAcctID>0 时只在该账户桶内找，
// 杜绝跨空间错配）；未定位空间账户时遍历设备绑定账户桶。返回池条目或 nil。
// 注意：仅池内索引匹配，调用方必须 DB 二次校验。
func (a *App) noteMatchFromPool(dev *personal.Device, provider string, space int, note string, since time.Time, spaceAcctID int64) *pool.Entry {
	if a.waitPayPool == nil {
		return nil
	}
	if spaceAcctID > 0 {
		if e := a.waitPayPool.MatchByNote(spaceAcctID, note, since); e != nil {
			return e
		}
		return nil // 空间已定位：其他账户桶的备注码不可能是本空间收款（防跨空间错配）
	}
	for _, id := range store.DeviceAccountIDs(context.Background(), a.personalDB, dev.ID, dev.AccountID) {
		if e := a.waitPayPool.MatchByNote(id, note, since); e != nil {
			return e
		}
	}
	return nil
}

// accountIDForSpace 按通知来源空间定位设备专属账户：
// 查 device_qrcodes（设备+平台+用户空间）的最新收款码 → 与账户池中账户的 QRContent 匹配。
// space=0(主空间)与 space=999(双开)都解析：主空间收款只匹配主账户订单、双开只匹配双开账户订单，
// 彻底消除同设备同金额下主/双开账户错配。查不到（无码/未绑定/码不匹配）返回 0，
// 此时匹配范围放宽为设备绑定账户集合（歧义由多候选拒绝机制兜底，不再自动挑选）。
func (a *App) accountIDForSpace(ctx context.Context, dev *personal.Device, provider string, space int) int64 {
	if dev == nil {
		return 0
	}
	qr, err := a.personalDB.GetLatestQrcodeByPlatformUser(ctx, dev.DeviceID, provider, space)
	if err != nil || qr == nil || qr.QRContent == "" {
		return 0
	}
	for _, id := range store.DeviceAccountIDs(ctx, a.personalDB, dev.ID, dev.AccountID) {
		acct := a.accountPool.GetByID(id)
		if acct != nil && acct.QRContent == qr.QRContent {
			return acct.ID
		}
	}
	return 0
}

// resolvePersonalQr 校验套码 token 并解析 (订单, 个人通道, 账户)；/mq 与 /qr/real 共用
// 校验链：token 签名/过期 → 订单存在 → WAIT_PAY → token 与订单匹配 → 账户收款码存在

// ledgerUserSpaceOf 账户所属用户空间：收款码内容匹配该设备 (平台, 用户空间) 最新码。
// 主空间(user 0)与双开(user 999)的码内容不同，用 QRContent 精确匹配归属；
// 匹配不到（无码/未上报）默认主空间 0。
func ledgerUserSpaceOf(ctx context.Context, pdb store.PersonalStore, deviceID, provider, qrContent string) int {
	if qrContent == "" {
		return 0
	}
	for _, space := range []int{999, 0} {
		qr, err := pdb.GetLatestQrcodeByPlatformUser(ctx, deviceID, provider, space)
		if err == nil && qr != nil && qr.QRContent == qrContent {
			return space
		}
	}
	return 0
}

// newLedgerReader 按设备构造账本读取器（ledgermon.ReaderFactory 实现）。
// Phase 1 仅支付宝 dump 文本通道；微信 OCR 通道 Phase 2 接入。
