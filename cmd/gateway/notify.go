// paygateway 支付网关 notify（从 main.go 拆分，QR real/通知处理）
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"paygateway/internal/account"
	"paygateway/internal/channel"
	personalch "paygateway/internal/channel/personal"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/notifywatcher"
	"paygateway/internal/personal"
	"paygateway/internal/web"
	"time"

	"github.com/gin-gonic/gin"
)

func (a *App) resolvePersonalQr(c *gin.Context, token string) (*model.Order, *personalch.Channel, *account.Account, error) {
	key, _, err := personal.ParseQrToken(token)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("二维码无效或已过期: %w", err)
	}
	o, err := a.store.GetOrder(c.Request.Context(), key)
	if err != nil || o == nil {
		return nil, nil, nil, fmt.Errorf("订单不存在")
	}
	if o.Status != model.StatusWaitPay {
		return nil, nil, nil, fmt.Errorf("订单已处理（状态: %s）", o.Status)
	}
	if o.QrToken != "" && o.QrToken != token {
		return nil, nil, nil, fmt.Errorf("二维码与订单不匹配")
	}
	ch := a.personalChannelFor(o.Channel)
	if ch == nil {
		return nil, nil, nil, fmt.Errorf("通道配置错误")
	}
	acct := ch.GetAccountByID(o.AccountID)
	if acct == nil || acct.QRContent == "" {
		return nil, nil, nil, fmt.Errorf("收款码配置错误")
	}
	return o, ch, acct, nil
}

// handleQrReal 直发码：校验 token → 生成真实收款码 PNG → 返回图片（不进支付页 HTML）
// GET /qr/real?token={token}
func (a *App) handleQrReal(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		c.String(http.StatusNotFound, "无效二维码")
		return
	}
	o, _, acct, err := a.resolvePersonalQr(c, token)
	if err != nil {
		logger.Warn("personal", "直发码 token 校验失败", "err", err)
		c.String(http.StatusNotFound, "二维码无效或已过期")
		return
	}
	png := web.QRPng(acct.QRContent)
	if len(png) == 0 {
		c.String(http.StatusInternalServerError, "收款码生成失败")
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'")
	logger.Info("personal", "直发码图片下发", "out_trade_no", o.OutTradeNo, "amount", o.AmountMinor)
	c.Data(http.StatusOK, "image/png", png)
}

// personalChannelFor 按订单通道名返回个人通道实例
func (a *App) personalChannelFor(chName string) *personalch.Channel {
	switch chName {
	case "personal.alipay":
		return a.personalAlipay
	case "personal.wechat":
		return a.personalWechat
	}
	return nil
}

// sha256Hash 计算 SHA-256 哈希（hex）
func sha256Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}

// handleNotify 通知栏收款事件回调（notifywatcher.Handler 实现）。
//
// 快速通道语义（2026-08-30 用户需求确认）：
//   - 通知栏比账本页快 ~15s，收到收款通知即尝试快速匹配入账；
//   - 匹配窗口以通知 when（收款发生时刻）为锚：since = when - 30min，
//     不能用 time.Now()（通知不清理会一直在，旧通知会错误匹配新订单）；
//   - 复用 matchPersonalOrder（空间账户过滤 + 金额时间窗 + 备注码），
//     与账本链路共享同一套资金安全逻辑；
//   - 金额唯一 → 直接入账（冻结守卫 + SettlePayment 幂等）；
//     多候选(ambiguous) / 无订单(no_order) → 不动作，留给账本复查兜底。
func (a *App) handleNotify(ctx context.Context, ev notifywatcher.Event) error {
	dev, err := a.personalDeviceCtx(ctx, ev.DeviceID)
	if err != nil {
		return fmt.Errorf("设备加载失败: %w", err)
	}
	if dev.Status == personal.DeviceFrozen {
		logger.Warn("notifywatcher", "通知回调：设备已冻结，跳过", "device_id", ev.DeviceID)
		return nil
	}
	// 窗口锚点 = 通知 when（毫秒时间戳）；与账本 30min 窗口语义一致
	notifyTime := time.UnixMilli(ev.When)
	window := 30 * time.Minute

	orderNo, matched, acctID, err := a.matchPersonalOrder(ctx, dev, ev.Provider, "", ev.AmountMinor, notifyTime, window, ev.Space)
	if err != nil {
		return fmt.Errorf("通知订单匹配失败: %w", err)
	}

	// 指纹落库（幂等去重的写入侧）：notify 事件此前从不回写 raw_hash，PushExists
	// 每轮查空 → 同一通知每轮重复捕获+重复匹配（真机风暴实测 30s 内 21 次）。
	// 账本链路（ledger.go）本就在写本表，此处对齐（空间隔离：指纹含 space+when）。
	rawHash := ev.Fingerprint()
	if rawHash != "" {
		if dup, derr := a.personalDB.PushExists(ctx, ev.DeviceID, rawHash); derr == nil && !dup {
			_ = a.personalDB.InsertPush(ctx, &personal.Push{
				DeviceID: ev.DeviceID, Provider: ev.Provider, AmountMinor: ev.AmountMinor,
				RawHash: rawHash, MatchedOrder: orderNo, MatchResult: matched,
				Space: ev.Space, Raw: ev.Raw,
			})
		}
	}

	raw := ev.Raw
	switch matched {
	case "matched":
		// 冻结账户守卫（对齐账本链路）：冻结账户不自动入账，转异常池人工核实
		if acct := a.accountPool.GetByID(acctID); acct != nil && acct.Status == account.StatusFrozen {
			logger.Warn("notifywatcher", "账户已冻结，通知收款转人工", "order", orderNo, "account_id", acctID)
			if cur, cerr := a.store.GetOrder(ctx, orderNo); cerr == nil && cur != nil {
				a.orders.EnqueueException(ctx, cur, model.ExReasonListenOffline, "PAID_WHILE_FROZEN",
					"", raw, "通知栏监听：收款到达时账户处于冻结态，需人工核实际收款后入账")
			}
			return nil
		}
		nr := &channel.NotifyResult{
			Valid:       true,
			OutTradeNo:  orderNo,
			ChannelTxID: fmt.Sprintf("NOTIFY-%s-%d", ev.DeviceID, ev.When),
			Status:      model.StatusSuccess,
			AmountMinor: ev.AmountMinor,
			Payer:       "notify:" + ev.DeviceID,
			DeviceID:    ev.DeviceID,
			AccountID:   acctID,
			RawPayload:  raw,
		}
		if ok, serr := a.orders.SettlePayment(ctx, nr); serr != nil {
			logger.Error("notifywatcher", "通知快速入账失败", "err", serr, "order", orderNo)
			return serr
		} else if !ok {
			// ok=false, serr=nil：并发已支付/已关单（幂等），账本复查也会幂等跳过
			logger.Info("notifywatcher", "通知入账幂等跳过（已支付/已关单）", "order", orderNo)
			return nil
		}
		if _, uerr := a.store.UpdateOrderStatus(ctx, orderNo, model.StatusSuccess, model.StatusSuccess, func(upd *model.Order) {
			upd.DeviceID = ev.DeviceID
		}); uerr != nil {
			logger.Warn("notifywatcher", "回写订单 device_id 失败", "err", uerr, "order", orderNo)
		}
		a.confirmSched.OnSettled(orderNo)
		logger.Info("notifywatcher", "通知栏快速入账成功 ✅（比账本快 ~15s）",
			"order", orderNo, "amount", ev.AmountMinor, "device", ev.DeviceID,
			"provider", ev.Provider, "space", ev.Space)
	case "settled_dup":
		// 同一笔收款已被账本/通知另一链路入账（双链同捕竞态，真机实测 5s 内），
		// 幂等跳过：不动作（防重复/防入账），仅留审计
		logger.Info("notifywatcher", "通知重复捕获，另一链路已入账（幂等跳过）", "order", orderNo)
	case "no_order":
		// 通知栏可能是手动收款等非订单场景；账本是 no_order 唯一判定者，避免双写。
		// 但通知栏先到达时该笔收款尚未落 unmatched——账本 5s 后复查（含 settled_dup），
		// 若账本侧也无主则重复有一次记录留痕（幂等由 CreateUnmatched 无唯一约束，可接受），
		// 待账本落库后此处不再单独入 unmatched（避免双链重复记录）。
		logger.Debug("notifywatcher", "通知无匹配订单（留给账本复查）",
			"device", ev.DeviceID, "amount", ev.AmountMinor, "provider", ev.Provider)
	default: // ambiguous
		logger.Warn("notifywatcher", "通知多订单歧义（账本复查确认）",
			"device", ev.DeviceID, "amount", ev.AmountMinor, "provider", ev.Provider)
	}
	return nil
}

// 防止 gopay 未使用（保持依赖）
