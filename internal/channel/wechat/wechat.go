// Package wechat 微信支付通道（基于 gopay 库，V2 协议）
//
// 账户池集成：
//   - 持有 AccountPool，下单选账户、回调反查 mch_id
//   - 每个账户对应一个 gopay.Client，按需创建并缓存
package wechat

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-pay/gopay"
	"github.com/go-pay/gopay/wechat"
	"paygateway/internal/account"
	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/model"
)

// Config 微信通道配置（仅回调地址 + 调试开关；账户密钥一律来自账户池）
type Config struct {
	NotifyURL string // 异步回调地址
	Debug     bool   // 调试日志
}

// Channel 微信通道实现
type Channel struct {
	cfg         Config
	pool        *account.AccountPool
	clientCache map[int64]*wechat.Client
	clientMu    sync.RWMutex
}

// New 创建微信通道（账户池模式：所有账户数据从 pool 读取）
func New(cfg Config, pool *account.AccountPool) (*Channel, error) {
	ch := &Channel{
		cfg:         cfg,
		pool:        pool,
		clientCache: make(map[int64]*wechat.Client),
	}
	return ch, nil
}

// getOrCreateClient 获取或创建账户对应的 gopay client
func (c *Channel) getOrCreateClient(acct *account.Account) (*wechat.Client, error) {
	c.clientMu.RLock()
	if cli, ok := c.clientCache[acct.ID]; ok {
		c.clientMu.RUnlock()
		return cli, nil
	}
	c.clientMu.RUnlock()

	c.clientMu.Lock()
	defer c.clientMu.Unlock()

	if cli, ok := c.clientCache[acct.ID]; ok {
		return cli, nil
	}

	if acct.AppID == "" || acct.MchID == "" || acct.ApiKey == "" {
		return nil, fmt.Errorf("微信账户配置不完整: AppID/MchID/ApiKey 必填")
	}
	client := wechat.NewClient(acct.AppID, acct.MchID, acct.ApiKey, acct.IsProd)
	if c.cfg.Debug {
		client.DebugSwitch = gopay.DebugOn
	}
	c.clientCache[acct.ID] = client
	logger.Info("wechat", "账户客户端已创建", "account_id", acct.ID, "name", acct.Name)
	return client, nil
}

// SetNotifyURL 动态更新回调地址
func (c *Channel) SetNotifyURL(url string) {
	c.cfg.NotifyURL = url
	logger.Info("wechat", "回调地址已更新", "notify_url", url)
}

// RemoveClient 移除账户对应的 gopay client 缓存（账户 app_id/密钥变更后必须调用）
func (c *Channel) RemoveClient(accountID int64) {
	c.clientMu.Lock()
	if _, ok := c.clientCache[accountID]; ok {
		delete(c.clientCache, accountID)
		logger.Info("wechat", "账户客户端缓存已清除", "account_id", accountID)
	}
	c.clientMu.Unlock()
}

// Name 通道标识
func (c *Channel) Name() string { return "merchant.wechat" }

// Type 通道大类：商家通道
func (c *Channel) Type() channel.ChannelType { return channel.TypeMerchant }

// Provider 支付品牌：微信
func (c *Channel) Provider() channel.Provider { return channel.ProviderWechat }

// ConfirmAbility 确认能力：可主动查单
func (c *Channel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByQuery }

// NotifyURL 返回当前回调地址
func (c *Channel) NotifyURL() string { return c.cfg.NotifyURL }

// Kind 通道类型：回调驱动
func (c *Channel) Kind() channel.ChannelKind { return channel.KindCallback }

// Modes 支持的支付模式
func (c *Channel) Modes() []channel.PayMode {
	return []channel.PayMode{channel.ModeQRCode, channel.ModeWap, channel.ModePage}
}

// Listener 回调型无监听器
func (c *Channel) Listener() channel.EventListener { return nil }

// selectAccount 从账户池选一个可用账户
func (c *Channel) selectAccount(group string) *account.Account {
	if c.pool == nil {
		return nil
	}
	return c.pool.Select("wechat", group)
}

// CreatePayment 下单
func (c *Channel) CreatePayment(ctx context.Context, req *channel.PayRequest) (*channel.PayCredential, error) {
	// 从账户池选账户
	var acct *account.Account
	if c.pool != nil {
		acct = c.pool.Select("wechat", "default")
		if acct == nil {
			return nil, fmt.Errorf("微信暂无可用账户（所有账户已达日限额或冷却中）")
		}
	}

	// 获取 gopay client（账户池模式：必选账户）
	var client *wechat.Client
	if acct == nil {
		return nil, fmt.Errorf("微信暂无可用账户")
	}
	client, err := c.getOrCreateClient(acct)
	if err != nil {
		return nil, fmt.Errorf("获取微信客户端失败: %w", err)
	}

	bm := make(gopay.BodyMap)
	bm.Set("nonce_str", randomStr(32)).
		Set("body", truncate(req.Subject, 30)).
		Set("out_trade_no", req.OutTradeNo).
		Set("total_fee", req.AmountMinor).
		Set("spbill_create_ip", "127.0.0.1").
		Set("notify_url", c.cfg.NotifyURL)

	payMode := c.resolveMode(req)
	apiCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var cred *channel.PayCredential
	var payErr error

	switch payMode {
	case channel.ModeWap:
		logger.Info("wechat", "模式: H5支付 (MWEB)")
		bm.Set("trade_type", wechat.TradeType_H5).
			Set("scene_info", `{"h5_info":{"type":"Wap","wap_url":"http://localhost:8080","wap_name":"PayGateway"}}`)
		wxRsp, err := client.UnifiedOrder(apiCtx, bm)
		if err != nil {
			payErr = fmt.Errorf("H5下单失败: %w", err)
		} else if wxRsp.ReturnCode != "SUCCESS" || wxRsp.ResultCode != "SUCCESS" {
			payErr = fmt.Errorf("H5下单失败: return=%s(%s) result=%s(%s)",
				wxRsp.ReturnCode, wxRsp.ReturnMsg, wxRsp.ResultCode, wxRsp.ErrCodeDes)
		} else {
			cred = &channel.PayCredential{PayURL: wxRsp.MwebUrl, Mode: channel.ModeWap, ExpireAt: time.Now().Add(2 * time.Hour)}
		}

	default:
		logger.Info("wechat", "模式: Native扫码支付")
		bm.Set("trade_type", wechat.TradeType_Native)
		wxRsp, err := client.UnifiedOrder(apiCtx, bm)
		if err != nil {
			payErr = fmt.Errorf("Native下单失败: %w", err)
		} else if wxRsp.ReturnCode != "SUCCESS" || wxRsp.ResultCode != "SUCCESS" {
			payErr = fmt.Errorf("Native下单失败: return=%s(%s) result=%s(%s)",
				wxRsp.ReturnCode, wxRsp.ReturnMsg, wxRsp.ResultCode, wxRsp.ErrCodeDes)
		} else {
			cred = &channel.PayCredential{QRCode: wxRsp.CodeUrl, Mode: channel.ModeQRCode, ExpireAt: time.Now().Add(2 * time.Hour)}
		}
	}

	// 上报结果给账户池
	if acct != nil && c.pool != nil {
		c.pool.ReportResult(acct.ID, payErr == nil)
		if payErr == nil && cred != nil {
			c.pool.AddDailyUsed(acct.ID, req.AmountMinor)
		}
	}

	// 记录使用的账户ID
	if payErr == nil && cred != nil && acct != nil {
		cred.AccountID = acct.ID
	}

	if payErr != nil {
		return nil, payErr
	}
	return cred, nil
}

// resolveMode 解析支付模式
func (c *Channel) resolveMode(req *channel.PayRequest) channel.PayMode {
	switch {
	case req.PayMode == channel.ModeWap || req.Device == "wap":
		return channel.ModeWap
	case req.PayMode == channel.ModePage || req.Device == "page":
		return channel.ModeQRCode
	case req.Device == "mobile":
		return channel.ModeWap
	default:
		return channel.ModeQRCode
	}
}

// VerifyNotify 验签微信异步回调
// 通过 mch_id 反查账户池中的账户，用该账户的 API 密钥验签
func (c *Channel) VerifyNotify(ctx context.Context, req *http.Request) (*channel.NotifyResult, error) {
	bm, err := wechat.ParseNotifyToBodyMap(req)
	if err != nil {
		return nil, fmt.Errorf("解析微信回调失败: %w", err)
	}

	mchID := bm.GetString("mch_id")

	if c.pool == nil {
		return nil, fmt.Errorf("账户池未初始化，无法验签")
	}
	acct := c.pool.LookupByMchID(mchID)
	if acct == nil {
		logger.Warn("wechat", "回调 mch_id 未匹配到账户", "mch_id", mchID)
		return nil, fmt.Errorf("未找到 mch_id=%s 对应的账户", mchID)
	}
	ok, err := wechat.VerifySign(acct.ApiKey, wechat.SignType_MD5, bm)
	if err != nil || !ok {
		return nil, fmt.Errorf("微信验签失败（账户: %s）: %v", acct.Name, err)
	}
	logger.Info("wechat", "回调验签通过", "account", acct.Name, "mch_id", mchID)

	acctID := int64(0)
	if acct != nil {
		acctID = acct.ID
	}

	if bm.GetString("return_code") != "SUCCESS" || bm.GetString("result_code") != "SUCCESS" {
		return &channel.NotifyResult{
			Valid: true, OutTradeNo: bm.GetString("out_trade_no"),
			Status: model.StatusFailed, MchID: mchID, AccountID: acctID, RawPayload: rawXML(bm),
		}, nil
	}

	amountMinor, _ := strconv.ParseInt(bm.GetString("total_fee"), 10, 64)
	return &channel.NotifyResult{
		Valid: true, OutTradeNo: bm.GetString("out_trade_no"),
		ChannelTxID: bm.GetString("transaction_id"), Status: model.StatusSuccess,
		AmountMinor: amountMinor, Payer: bm.GetString("openid"),
		MchID: mchID, AccountID: acctID, RawPayload: rawXML(bm),
	}, nil
}

// QueryOrder 主动查询订单
func (c *Channel) QueryOrder(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error) {
	if c.pool == nil {
		return nil, fmt.Errorf("账户池未初始化")
	}
	accts := c.pool.ListByChannel("wechat")
	for _, acct := range accts {
		client, err := c.getOrCreateClient(acct)
		if err != nil {
			continue
		}
		bm := make(gopay.BodyMap)
		bm.Set("nonce_str", randomStr(32)).Set("out_trade_no", outTradeNo)
		wxRsp, _, err := client.QueryOrder(ctx, bm)
		if err != nil {
			continue
		}
		return parseOrderStatus(wxRsp), nil
	}
	return nil, fmt.Errorf("所有微信账户查询均失败")
}

func parseOrderStatus(wxRsp *wechat.QueryOrderResponse) *channel.OrderStatus {
	st := &channel.OrderStatus{ChannelTxID: wxRsp.TransactionId}
	switch wxRsp.TradeState {
	case "SUCCESS":
		st.Status = model.StatusSuccess
	case "CLOSED", "REVOKED":
		st.Status = model.StatusClosed
	case "PAYERROR":
		st.Status = model.StatusFailed
	default:
		st.Status = model.StatusWaitPay
	}
	if fee, err := strconv.ParseInt(wxRsp.TotalFee, 10, 64); err == nil {
		st.AmountMinor = fee
	}
	return st
}

// NotifySuccessResponse 微信成功应答 XML
func NotifySuccessResponse() string {
	return `<xml><return_code><![CDATA[SUCCESS]]></return_code><return_msg><![CDATA[OK]]></return_msg></xml>`
}

// NotifyFailResponse 微信失败应答 XML
func NotifyFailResponse() string {
	return `<xml><return_code><![CDATA[FAIL]]></return_code><return_msg><![CDATA[FAIL]]></return_msg></xml>`
}

func rawXML(bm gopay.BodyMap) string {
	type kv struct {
		XMLName xml.Name `xml:"xml"`
		Fields  map[string]string
	}
	fields := make(map[string]string)
	for k, v := range bm {
		if s, ok := v.(string); ok {
			fields[k] = s
		}
	}
	b, _ := xml.Marshal(kv{Fields: fields})
	return string(b)
}

func randomStr(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	h := md5.Sum([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(letters))))
	s := hex.EncodeToString(h[:])
	if len(s) > n {
		return s[:n]
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
