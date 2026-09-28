// Package alipay 支付宝支付通道（基于 gopay 库）
//
// 账户池集成：
//   - 持有 AccountPool，下单选账户、回调反查 app_id
//   - 每个账户对应一个 gopay.Client，按需创建并缓存
//   - 下单成功/失败后上报结果给 pool（熔断统计）
package alipay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-pay/gopay"
	"github.com/go-pay/gopay/alipay"
	"paygateway/internal/account"
	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/model"
)

// Config 支付宝通道配置（仅回调地址 + 调试开关；账户密钥一律来自账户池）
type Config struct {
	NotifyURL string // 异步回调地址
	Debug     bool   // 调试日志
}

// Channel 支付宝通道实现
type Channel struct {
	cfg          Config
	pool         *account.AccountPool     // 账户池（nil = 传统单账户模式）
	clientCache  map[int64]*alipay.Client // 账户ID → gopay client 缓存
	clientMu     sync.RWMutex
	modesMu      sync.RWMutex    // 保护 enabledModes（管理后台切换与下单并发）
	enabledModes map[string]bool // 支付模式启停状态，默认全开
}

// defaultEnabledModes 默认开启所有模式
func defaultEnabledModes() map[string]bool {
	return map[string]bool{
		"page":   true,
		"wap":    true,
		"qrcode": true,
	}
}

// New 创建支付宝通道（账户池模式：所有账户数据从 pool 读取）
func New(cfg Config, pool *account.AccountPool) (*Channel, error) {
	ch := &Channel{
		cfg:          cfg,
		pool:         pool,
		clientCache:  make(map[int64]*alipay.Client),
		enabledModes: defaultEnabledModes(),
	}
	return ch, nil
}

// newClient 创建 gopay 支付宝客户端
func (c *Channel) newClient(appID, privateKey string, isProd bool, notifyURL string, debug bool) (*alipay.Client, error) {
	client, err := alipay.NewClient(appID, privateKey, isProd)
	if err != nil {
		return nil, fmt.Errorf("支付宝客户端初始化失败: %w", err)
	}
	client.SetCharset("utf-8").
		SetSignType(alipay.RSA2).
		SetNotifyUrl(notifyURL)
	client.GetHttpClient().SetTimeout(120 * time.Second)
	if debug {
		client.DebugSwitch = gopay.DebugOn
	}
	return client, nil
}

// getOrCreateClient 获取或创建账户对应的 gopay client
func (c *Channel) getOrCreateClient(acct *account.Account) (*alipay.Client, error) {
	// 先查缓存
	c.clientMu.RLock()
	if cli, ok := c.clientCache[acct.ID]; ok {
		c.clientMu.RUnlock()
		return cli, nil
	}
	c.clientMu.RUnlock()

	// 创建新 client
	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	// 双重检查
	if cli, ok := c.clientCache[acct.ID]; ok {
		return cli, nil
	}

	notifyURL := c.cfg.NotifyURL
	debug := c.cfg.Debug
	client, err := c.newClient(acct.AppID, acct.PrivateKey, acct.IsProd, notifyURL, debug)
	if err != nil {
		return nil, err
	}
	c.clientCache[acct.ID] = client
	logger.Info("alipay", "账户客户端已创建", "account_id", acct.ID, "name", acct.Name)
	return client, nil
}

// SetNotifyURL 动态更新回调地址（隧道重启换域名时调用）
func (c *Channel) SetNotifyURL(url string) {
	c.cfg.NotifyURL = url
	// 更新所有缓存的 client
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	for id, client := range c.clientCache {
		client.SetNotifyUrl(url)
		_ = id
	}
	logger.Info("alipay", "回调地址已更新", "notify_url", url)
}

// RemoveClient 移除账户对应的 gopay client 缓存。
// 账户更新（app_id / 私钥变更）后必须调用，否则缓存的旧 client 会继续使用旧参数下单。
func (c *Channel) RemoveClient(accountID int64) {
	c.clientMu.Lock()
	if _, ok := c.clientCache[accountID]; ok {
		delete(c.clientCache, accountID)
		logger.Info("alipay", "账户客户端缓存已清除", "account_id", accountID)
	}
	c.clientMu.Unlock()
}

// Name 通道标识
func (c *Channel) Name() string { return "merchant.alipay" }

// Type 通道大类：商家通道
func (c *Channel) Type() channel.ChannelType { return channel.TypeMerchant }

// Provider 支付品牌：支付宝
func (c *Channel) Provider() channel.Provider { return channel.ProviderAlipay }

// ConfirmAbility 确认能力：可主动查单
func (c *Channel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByQuery }

// NotifyURL 返回当前回调地址（管理后台展示）
func (c *Channel) NotifyURL() string { return c.cfg.NotifyURL }

// MchID 支付宝无商户号概念
func (c *Channel) MchID() string { return "" }

// Kind 通道类型：回调驱动
func (c *Channel) Kind() channel.ChannelKind { return channel.KindCallback }

// Modes 所有支持的支付模式
func (c *Channel) Modes() []channel.PayMode {
	return []channel.PayMode{channel.ModePage, channel.ModeWap, channel.ModeQRCode}
}

// SetModeEnabled 开启/关闭某个支付模式
func (c *Channel) SetModeEnabled(mode string, enabled bool) {
	c.modesMu.Lock()
	c.enabledModes[mode] = enabled
	c.modesMu.Unlock()
	logger.Info("alipay", "支付模式已切换", "mode", mode, "enabled", enabled)
}

// IsModeEnabled 检查某个支付模式是否开启
func (c *Channel) IsModeEnabled(mode string) bool {
	c.modesMu.RLock()
	defer c.modesMu.RUnlock()
	return c.enabledModes[mode]
}

// EnabledModes 返回当前启用的模式列表
func (c *Channel) EnabledModes() map[string]bool {
	c.modesMu.RLock()
	defer c.modesMu.RUnlock()
	result := make(map[string]bool)
	for k, v := range c.enabledModes {
		result[k] = v
	}
	return result
}

// Listener 回调型无监听器
func (c *Channel) Listener() channel.EventListener { return nil }

// selectAccount 从账户池选一个可用账户
// 返回 nil 表示无可用账户或未启用账户池
func (c *Channel) selectAccount(group string) *account.Account {
	if c.pool == nil {
		return nil
	}
	return c.pool.Select("alipay", group)
}

// CreatePayment 下单：按 PayMode 创建对应支付宝订单
func (c *Channel) CreatePayment(ctx context.Context, req *channel.PayRequest) (*channel.PayCredential, error) {
	payMode := c.resolveMode(req)

	// 检查支付模式是否已开启（resolveMode 已降级，走到这里说明全部模式都已关闭）
	if !c.IsModeEnabled(string(payMode)) {
		enabled := make([]string, 0)
		for m, on := range c.enabledModes {
			if on {
				enabled = append(enabled, m)
			}
		}
		return nil, fmt.Errorf("支付模式 %q 未开启（当前已开启: %v）", payMode, enabled)
	}

	// 从账户池选账户
	var acct *account.Account
	if c.pool != nil {
		acct = c.pool.Select("alipay", "default")
		if acct == nil {
			return nil, fmt.Errorf("支付宝暂无可用账户（所有账户已达日限额或冷却中）")
		}
	}

	// 获取 gopay client（账户池模式：必选账户）
	var client *alipay.Client
	if acct == nil {
		return nil, fmt.Errorf("支付宝暂无可用账户")
	}
	client, err := c.getOrCreateClient(acct)
	if err != nil {
		return nil, fmt.Errorf("获取支付宝客户端失败: %w", err)
	}

	bm := make(gopay.BodyMap)
	bm.Set("subject", req.Subject).
		Set("out_trade_no", req.OutTradeNo).
		Set("total_amount", yuan(req.AmountMinor)).
		Set("timeout_express", "30m")

	// 支付宝 API 调用超时 120 秒
	apiCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var cred *channel.PayCredential
	var payErr error

	switch payMode {
	case channel.ModePage:
		logger.Info("alipay", "模式: 电脑网站支付 (page)")
		bm.Set("product_code", "FAST_INSTANT_TRADE_PAY")
		payURL, err := client.TradePagePay(apiCtx, bm)
		if err != nil {
			payErr = fmt.Errorf("电脑网站支付下单失败: %w", err)
		} else {
			cred = &channel.PayCredential{PayURL: payURL, Mode: channel.ModePage, ExpireAt: time.Now().Add(30 * time.Minute)}
		}

	case channel.ModeWap:
		logger.Info("alipay", "模式: 手机网站支付 (wap)")
		bm.Set("product_code", "QUICK_WAP_WAY")
		payURL, err := client.TradeWapPay(apiCtx, bm)
		if err != nil {
			payErr = fmt.Errorf("手机网站支付下单失败: %w", err)
		} else {
			cred = &channel.PayCredential{PayURL: payURL, Mode: channel.ModeWap, ExpireAt: time.Now().Add(30 * time.Minute)}
		}

	default: // ModeQRCode 扫码支付
		logger.Info("alipay", "模式: 扫码支付 (当面付)")
		aliRsp, err := client.TradePrecreate(apiCtx, bm)
		if err != nil {
			payErr = fmt.Errorf("当面付下单失败: %w", err)
		} else {
			qrCode := aliRsp.Response.QrCode
			cred = &channel.PayCredential{QRCode: qrCode, Mode: channel.ModeQRCode, ExpireAt: time.Now().Add(2 * time.Hour)}
			// 生成备用 WAP 链接（仅当 wap 模式开启时；关闭 wap 则不再生成备用跳转）
			if c.IsModeEnabled(string(channel.ModeWap)) {
				if wapURL, werr := c.wapBackup(apiCtx, client, bm); werr == nil {
					cred.PayURL = wapURL
				}
			}
		}
	}

	// 上报结果给账户池
	if acct != nil && c.pool != nil {
		c.pool.ReportResult(acct.ID, payErr == nil)
		if payErr == nil && cred != nil {
			c.pool.AddDailyUsed(acct.ID, req.AmountMinor)
		}
	}

	if payErr != nil {
		return nil, payErr
	}
	// 记录使用的账户ID
	if cred != nil && acct != nil {
		cred.AccountID = acct.ID
	}
	return cred, nil
}

// resolveMode 解析支付模式（用户指定 > 设备识别 > 智能降级到已开启模式）
// 用户显式指定 pay_type 但该模式已关闭时，降级到设备匹配的已开启模式；
// 设备识别出的模式已关闭时，按降级链选择第一个已开启的模式。
// 全部模式关闭时返回识别出的默认模式（由 CreatePayment 报错提示）。
func (c *Channel) resolveMode(req *channel.PayRequest) channel.PayMode {
	// 用户显式指定
	if req.PayMode != "" {
		if c.IsModeEnabled(string(req.PayMode)) {
			return channel.PayMode(req.PayMode)
		}
		logger.Warn("alipay", "用户指定支付模式未开启，按设备降级", "requested", req.PayMode, "device", req.Device)
	}
	// 设备识别 + 降级链
	return c.fallbackMode(req.Device)
}

// fallbackMode 按设备选择第一个已开启的模式（降级链）
// pc 偏好：page → qrcode → wap（电脑端扫码兜底）
// mobile 偏好：wap → qrcode → page（手机端扫码兜底）
// 未知设备默认：qrcode → page → wap
func (c *Channel) fallbackMode(device string) channel.PayMode {
	var candidates []channel.PayMode
	switch device {
	case "page", "pc":
		candidates = []channel.PayMode{channel.ModePage, channel.ModeQRCode, channel.ModeWap}
	case "wap", "mobile":
		candidates = []channel.PayMode{channel.ModeWap, channel.ModeQRCode, channel.ModePage}
	default:
		candidates = []channel.PayMode{channel.ModeQRCode, channel.ModePage, channel.ModeWap}
	}
	for _, m := range candidates {
		if c.IsModeEnabled(string(m)) {
			return m
		}
	}
	// 全部关闭：返回设备首选，由 CreatePayment 给出明确错误
	return candidates[0]
}

// wapBackup 生成备用 WAP 链接
func (c *Channel) wapBackup(ctx context.Context, client *alipay.Client, bm gopay.BodyMap) (string, error) {
	wapBM := make(gopay.BodyMap)
	for k, v := range bm {
		wapBM[k] = v
	}
	wapBM.Set("product_code", "QUICK_WAP_WAY")
	wapCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return client.TradeWapPay(wapCtx, wapBM)
}

// VerifyNotify 验签支付宝异步回调
// 账户池模式下：通过 app_id 反查账户，用该账户的公钥验签
func (c *Channel) VerifyNotify(ctx context.Context, req *http.Request) (*channel.NotifyResult, error) {
	notifyReq, err := alipay.ParseNotifyToBodyMap(req)
	if err != nil {
		return nil, fmt.Errorf("解析支付宝回调失败: %w", err)
	}

	appID := notifyReq.GetString("app_id")

	// 通过 app_id 反查账户池中的账户
	if c.pool == nil {
		return nil, fmt.Errorf("账户池未初始化，无法验签")
	}
	acct := c.pool.LookupByAppID(appID)
	if acct == nil {
		logger.Warn("alipay", "回调 app_id 未匹配到账户", "app_id", appID)
		return nil, fmt.Errorf("未找到 app_id=%s 对应的账户", appID)
	}
	// 用该账户的公钥验签
	ok, err := alipay.VerifySign(acct.PublicKey, notifyReq)
	if err != nil || !ok {
		return nil, fmt.Errorf("支付宝验签失败（账户: %s）: %v", acct.Name, err)
	}
	logger.Info("alipay", "回调验签通过", "account", acct.Name, "app_id", appID)

	tradeStatus := notifyReq.GetString("trade_status")
	status := model.StatusWaitPay
	switch tradeStatus {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		status = model.StatusSuccess
	case "TRADE_CLOSED":
		status = model.StatusClosed
	}
	amountMinor, _ := model.ParseMoneyToMinor(notifyReq.GetString("total_amount"))

	acctID := int64(0)
	if acct != nil {
		acctID = acct.ID
	}

	return &channel.NotifyResult{
		Valid:       true,
		OutTradeNo:  notifyReq.GetString("out_trade_no"),
		ChannelTxID: notifyReq.GetString("trade_no"),
		Status:      status,
		AmountMinor: amountMinor,
		Payer:       notifyReq.GetString("buyer_id"),
		AppID:       appID,
		AccountID:   acctID,
		RawPayload:  rawJSON(notifyReq),
	}, nil
}

// QueryOrder 主动查询（对账用）
func (c *Channel) QueryOrder(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error) {
	// 遍历账户池中所有支付宝账户查单
	if c.pool == nil {
		return nil, fmt.Errorf("账户池未初始化")
	}
	accts := c.pool.ListByChannel("alipay")
	for _, acct := range accts {
		client, err := c.getOrCreateClient(acct)
		if err != nil {
			continue
		}
		bm := make(gopay.BodyMap)
		bm.Set("out_trade_no", outTradeNo)
		rsp, err := client.TradeQuery(ctx, bm)
		if err != nil {
			continue
		}
		st := &channel.OrderStatus{ChannelTxID: rsp.Response.TradeNo}
		switch rsp.Response.TradeStatus {
		case "TRADE_SUCCESS", "TRADE_FINISHED":
			st.Status = model.StatusSuccess
		case "TRADE_CLOSED":
			st.Status = model.StatusClosed
		default:
			st.Status = model.StatusWaitPay
		}
		// 解析实际支付金额（元字符串 → 分，字符串十进制解析，避免 float 精度问题）
		if amt, err := model.ParseMoneyToMinor(rsp.Response.TotalAmount); err == nil {
			st.AmountMinor = amt
		}
		return st, nil
	}
	return nil, fmt.Errorf("所有支付宝账户查询均失败")
}

// yuan 最小单位(分) → 元字符串（支付宝协议）
func yuan(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

func rawJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
