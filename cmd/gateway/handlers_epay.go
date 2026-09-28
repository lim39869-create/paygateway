// paygateway 支付网关 handlers_epay（从 main.go 拆分，易支付下单/回调/查单）
package main

import (
	"net/http"
	"paygateway/internal/account"
	"paygateway/internal/channel"
	wechatch "paygateway/internal/channel/wechat"
	"paygateway/internal/epay"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/notify"
	"paygateway/internal/order"
	"paygateway/internal/upstream"
	"paygateway/internal/web"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (a *App) epayKeyFor(pid string) (string, bool) {
	if a.upstreamPool == nil || pid == "" {
		return "", false
	}
	u := a.upstreamPool.ByPid(pid)
	if u == nil || u.Status != upstream.StatusEnabled || u.Key == "" {
		return "", false
	}
	return u.Key, true
}

// isExplicitPersonalType 是否显式指定个人免签通道（personal_alipay / personal_wxpay / personal_qq）
// 显式指定时不回退商家（用户明确选择个人通道，回退可能造成歧义）
func isExplicitPersonalType(epayType string) bool {
	switch epayType {
	case "personal_alipay", "personal_wxpay", "personal_qq":
		return true
	}
	return false
}

// personalChannelReady 检查个人通道是否有可用收款账户
// （normal 状态 + 已配收款码 + 未冷却 + 未达日限额，任一满足即可接单）
func (a *App) personalChannelReady(name string) bool {
	for _, acct := range a.accountPool.ListByChannel(name) {
		if acct.Status != account.StatusNormal {
			continue
		}
		if acct.QRContent == "" {
			continue
		}
		if acct.FrozenUntil != nil && acct.FrozenUntil.After(time.Now()) {
			continue
		}
		if acct.DailyLimit > 0 && acct.DailyUsed >= acct.DailyLimit {
			continue
		}
		return true
	}
	return false
}

func (a *App) handleEpaySubmit(c *gin.Context) {
	params := collectParams(c)

	// 验签（严格多商户：pid 必须已登记且启用，密钥按 pid 路由）
	key, ok := a.epayKeyFor(params["pid"])
	if !ok {
		logger.Error("epay", "商户未登记或已禁用 ❌", "pid", params["pid"])
		c.String(http.StatusOK, "商户ID错误或已禁用")
		return
	}
	if !epay.VerifySign(params, key) {
		logger.Error("epay", "下单签名验证失败 ❌", "pid", params["pid"])
		c.String(http.StatusOK, "签名验证失败")
		return
	}
	logger.Info("epay", "下单签名验证通过 ✅", "pid", params["pid"], "out_trade_no", params["out_trade_no"])

	// 设备智能识别：优先 URL 参数 device，其次 User-Agent 自动判断
	// 电脑浏览器 → pc（收银台/扫码），手机浏览器 → mobile（WAP）
	device := c.Query("device")
	if device == "" {
		ua := c.GetHeader("User-Agent")
		if web.IsMobileUA(ua) {
			device = "mobile"
		} else {
			device = "pc"
		}
		logger.Info("epay", "设备智能识别", "device", device, "ua", ua)
	}

	buyerKey, fpKey := buyerKeyFor(c)
	req := &order.CreateRequest{
		OutTradeNo: params["out_trade_no"],
		Type:       params["type"],
		Name:       params["name"],
		Money:      params["money"],
		NotifyURL:  params["notify_url"],
		ReturnURL:  params["return_url"],
		PayMode:    params["pay_type"],
		Device:     device,
		BuyerKey:   buyerKey,
		FPKey:      fpKey,
		RawParams:  params,
	}
	if req.OutTradeNo == "" || req.Money == "" || req.Type == "" {
		c.String(http.StatusOK, "参数不完整")
		return
	}
	// return_url 兜底重写：上游传内网/localhost → Referer/上游公网地址逐层替换
	// （return_url 是付款人浏览器的回跳，与 notify_url 的 SSRF 校验语义不同，允许改写）
	req.ReturnURL = a.sanitizeReturnURL(c, req.OutTradeNo, params["pid"], req.ReturnURL)
	params["return_url"] = req.ReturnURL

	// SSRF 防护：下单时即拒绝非法回调地址（scheme 白名单 + 可选私网封锁），
	// 不让恶意 URL 进入 outbox 等待发送时才暴露
	if err := notify.ValidateNotifyURL(req.NotifyURL, a.cfg.NotifyAllowPrivateIPs); err != nil {
		logger.Warn("epay", "notify_url 校验失败", "err", err, "out_trade_no", req.OutTradeNo)
		c.String(http.StatusOK, "notify_url 非法: "+err.Error())
		return
	}

	// 校验支付类型（epay 协议 → 通道注册名映射）
	// 个人免签优先：personal.alipay/personal.wechat 已注册且启用 → 优先路由（0 费率）
	chName := channel.ResolveEpayType(req.Type)
	// 增强：个人通道无可用收款账户（未配置/冻结/限额/冷却）→ 自动回退商家通道
	// 仅对隐式类型（alipay/wxpay/qqpay）回退；显式 personal_alipay/personal_wxpay 不回退（报错提示更明确）
	if strings.HasPrefix(chName, "personal.") && !isExplicitPersonalType(req.Type) {
		if !a.personalChannelReady(chName) {
			if mName, ok := channel.EpayTypeToChannel[req.Type]; ok {
				logger.Info("epay", "个人通道无可用账户，回退商家通道",
					"type", req.Type, "from", chName, "to", mName)
				chName = mName
			}
		}
	}
	if chName == "" {
		logger.Warn("epay", "不支持的支付类型", "type", req.Type)
		c.String(http.StatusOK, "不支持的支付类型: "+req.Type)
		return
	}
	// 同 IP 下单限流（防脚本批量刷单；仅个人免签通道启用，商家通道不限）
	if a.cfg.MaxOrdersPerIPPerMin > 0 && strings.HasPrefix(chName, "personal.") {
		ip := c.ClientIP()
		if !a.ipLimiter.Allow(ip) {
			logger.Warn("epay", "下单限流：IP 超限", "ip", ip, "limit", a.cfg.MaxOrdersPerIPPerMin, "out_trade_no", params["out_trade_no"])
			c.String(http.StatusOK, "下单过于频繁，请稍后重试")
			return
		}
	}
	if _, ok := channel.Get(chName); !ok {
		logger.Warn("epay", "支付通道未注册", "type", req.Type, "channel", chName)
		c.String(http.StatusOK, "不支持的支付类型: "+req.Type)
		return
	}
	req.Type = chName // 转为通道注册名（如 "merchant.alipay" / "personal.alipay"）

	res, err := a.orders.CreateOrder(c.Request.Context(), req)
	if err != nil {
		if res != nil && res.Reused {
			// 复用已有订单：按原凭据重新渲染
			a.renderPayment(c, res.Existing)
			return
		}
		logger.Error("epay", "下单失败", "err", err, "out_trade_no", req.OutTradeNo)
		c.String(http.StatusOK, "下单失败: "+err.Error())
		return
	}
	// 进入确认调度器：超时未回调 → 主动查单兜底
	a.confirmSched.OnOrderCreated(res.Order)
	// 账本页 adb 监听（docs/PLAN-ADB-LEDGER-IMPL.md §2.2）：下单路由账户后激活该账户
	// 监听循环 → 高频轮询 + 高优先级窗口申请（下单后用户很可能马上付款，1s 级确认）
	if a.ledgermon != nil && strings.HasPrefix(res.Order.Channel, "personal.") {
		a.ledgermon.ActivateAccount(res.Order.AccountID)
	}
	a.renderPayment(c, res.Order)
}

// renderPayment 根据订单凭据渲染支付页面
func (a *App) renderPayment(c *gin.Context, o *model.Order) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	// 已支付订单：直接渲染成功页（付款人刷新/回退支付 URL 时不该再看到收款码）
	if o.Status == model.StatusSuccess {
		a.setPaymentPageHeaders(c)
		c.String(http.StatusOK, web.BuildSuccessPage(o, o.Channel))
		return
	}
	chName := o.Channel
	if chName == "" {
		chName = "alipay"
	}
	// 个人免签通道：直发码支付页（真实收款码 + 关单倒计时）
	if strings.HasPrefix(chName, "personal.") && o.QrCode != "" {
		var imgSrc string
		if strings.HasPrefix(o.QrCode, "/qr/real") {
			imgSrc = o.QrCode
		} else {
			// 历史数据兜底：旧套码 token 二维码内嵌渲染
			imgSrc = web.QRPngDataURI(o.QrCode)
		}
		// 关单时间（个人免签订单）：动态窗口取池内 CloseAt（设备负载驱动）；
		// 否则 = 创建时间 + 动态下限 ORDER_CLOSE_MIN（动态窗口已启用时绝不回退静态
		// ORDER_CLOSE_MINUTES，否则环境配置会拉长个人订单关单+轮询时长）。0=未开启自动关单。
		expireAt := a.personalOrderCloseAt(o)
		a.setPaymentPageHeaders(c)
		// 支付方式切换无需页面按钮：上游换支付模式入口（同号不同 type）下单时自动迁移订单
		c.String(http.StatusOK, web.BuildPersonalQRPage(o, imgSrc, chName, expireAt))
		return
	}
	switch {
	case o.QrCode != "":
		// 有二维码优先展示二维码页面（扫码/H5 备用 URL 不覆盖）
		a.setPaymentPageHeaders(c)
		c.String(http.StatusOK, web.BuildQRPage(o, o.QrCode, chName))
	case strings.Contains(o.PayURL, "alipay.trade.page.pay"):
		c.String(http.StatusOK, web.BuildRedirectPage(o, o.PayURL, "正在跳转到支付宝收银台..."))
	case strings.Contains(o.PayURL, "alipay.trade.wap.pay"):
		c.String(http.StatusOK, web.BuildRedirectPage(o, o.PayURL, "正在跳转到支付宝..."))
	case strings.Contains(o.PayURL, "wx.tenpay.com") || strings.Contains(o.PayURL, "weixin://"):
		c.String(http.StatusOK, web.BuildRedirectPage(o, o.PayURL, "正在跳转到微信支付..."))
	default:
		c.String(http.StatusOK, "订单创建成功，无支付凭据")
	}
}

// personalOrderCloseAt 计算个人免签订单的有效关单时间点；零值=未开启自动关单。
// 语义（与关单扫描器 CloseExpiredOrders 对齐，避免页面倒计时与后台关单漂移）：
//  1. 动态窗口模式：取 WAIT_PAY 池内 CloseAt（设备负载驱动，最精确）
//  2. 池内无该订单：动态窗口已启用则用动态下限 ORDER_CLOSE_MIN
//  3. 动态窗口未启用：用静态 ORDER_CLOSE_MINUTES
func (a *App) personalOrderCloseAt(o *model.Order) time.Time {
	if o == nil {
		return time.Time{}
	}
	if a.waitPayPool != nil {
		if ct, ok := a.waitPayPool.CloseAtOf(o.OutTradeNo); ok && !ct.IsZero() {
			return ct
		}
	}
	var win time.Duration
	if a.cfg.OrderCloseMin > 0 && a.cfg.OrderCloseMax > a.cfg.OrderCloseMin {
		win = time.Duration(a.cfg.OrderCloseMin) * time.Minute
	} else if a.cfg.OrderCloseMinutes > 0 {
		win = time.Duration(a.cfg.OrderCloseMinutes) * time.Minute
	}
	if win <= 0 || (o.CreatedAt.IsZero()) {
		return time.Time{}
	}
	closeAt := o.CreatedAt.Add(win)
	if !o.SessionExpiresAt.IsZero() && o.SessionExpiresAt.Before(closeAt) {
		return o.SessionExpiresAt
	}
	return closeAt
}

// setPaymentPageHeaders 支付页安全响应头：防二维码替换/爬取/点击劫持
// script-src 'unsafe-inline'：支付页内联 JS 用于浏览器指纹采集（防换 IP 刷单）
// connect-src 'self'：轮询 /qr/status 的 XHR 与指纹 sendBeacon 需要；缺省回落 default-src 'none'
// 会静默拦截所有 XHR（真机事故：倒计时能跳 /qr/expired 但支付成功永远探测不到，根因即此）。
// Content-Type 必须显式 text/html：c.String 默认 text/plain 会把 HTML 当源码显示
func (a *App) setPaymentPageHeaders(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'self'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
}

// ========== 支付宝异步回调 ==========
func (a *App) handleAlipayNotify(c *gin.Context) {
	ch, ok := channel.Get("merchant.alipay")
	if !ok {
		c.String(http.StatusOK, "fail")
		return
	}
	nr, err := ch.VerifyNotify(c.Request.Context(), c.Request)
	if err != nil {
		logger.Error("epay", "支付宝回调验签失败", "err", err)
		c.String(http.StatusOK, "fail")
		return
	}
	logger.Info("epay", "支付宝验签通过 ✅", "out_trade_no", nr.OutTradeNo, "status", nr.Status)

	if nr.Status == model.StatusSuccess {
		if ok, err := a.orders.SettlePayment(c.Request.Context(), nr); err != nil || !ok {
			logger.Error("epay", "支付结算失败", "err", err, "ok", ok)
			c.String(http.StatusOK, "fail")
			return
		}
		a.confirmSched.OnSettled(nr.OutTradeNo)
	} else if nr.Status == model.StatusClosed {
		logger.Info("epay", "订单关闭通知", "out_trade_no", nr.OutTradeNo)
		a.confirmSched.OnClosed(nr.OutTradeNo)
	}

	// 支付宝要求返回 success
	c.String(http.StatusOK, "success")
}

// ========== 微信异步回调 ==========
func (a *App) handleWechatNotify(c *gin.Context) {
	ch, ok := channel.Get("merchant.wechat")
	if !ok {
		c.String(http.StatusOK, wechatch.NotifyFailResponse())
		return
	}
	nr, err := ch.VerifyNotify(c.Request.Context(), c.Request)
	if err != nil {
		logger.Error("epay", "微信回调验签失败", "err", err)
		c.String(http.StatusOK, wechatch.NotifyFailResponse())
		return
	}
	logger.Info("epay", "微信验签通过 ✅", "out_trade_no", nr.OutTradeNo, "status", nr.Status)

	if nr.Status == model.StatusSuccess {
		if ok, err := a.orders.SettlePayment(c.Request.Context(), nr); err != nil || !ok {
			logger.Error("epay", "支付结算失败", "err", err, "ok", ok)
			c.String(http.StatusOK, wechatch.NotifyFailResponse())
			return
		}
		a.confirmSched.OnSettled(nr.OutTradeNo)
	} else if nr.Status == model.StatusClosed {
		logger.Info("epay", "订单关闭通知", "out_trade_no", nr.OutTradeNo)
		a.confirmSched.OnClosed(nr.OutTradeNo)
	}

	// 微信要求返回 XML SUCCESS
	c.String(http.StatusOK, wechatch.NotifySuccessResponse())
}

func (a *App) handleEpayQuery(c *gin.Context) {
	params := make(map[string]string)
	for k, v := range c.Request.URL.Query() {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	key, ok := a.epayKeyFor(params["pid"])
	if !ok || !epay.VerifySign(params, key) {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "签名验证失败"})
		return
	}
	o, err := a.orders.QueryOrder(c.Request.Context(), params["out_trade_no"])
	if err != nil || o == nil {
		c.JSON(http.StatusOK, gin.H{"code": -1, "msg": "订单不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"code":         1,
		"msg":          "success",
		"out_trade_no": o.OutTradeNo,
		"trade_no":     o.GatewayNo,
		"money":        o.MoneyYuan(),
		"trade_status": o.Status,
	})
}

// handleQrRefresh 刷新二维码：为原订单重新生成二维码/备注码（保持原单号不变）
// 易支付协议 out_trade_no 是上游业务主键，改单号会导致回调上游找不到订单 → 入账失败
// handleBindFingerprint 绑定浏览器指纹到订单 buyer_key（防换 IP 刷单）
// GET /personal/bind-fp?o={out_trade_no}&fp={base64(JSON指纹)}
// 指纹并入 buyer_key：原 buyer_key = H(ip|ua|device|buyer_id)，追加指纹后抗换 IP 攻击。
