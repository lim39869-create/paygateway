// Package personal 个人免签通道（收款码 + 备注码 + 监听端确认）
//
// 与商家通道（merchant.*）的本质区别：
//   - 无法主动查单（ConfirmByManual）：用户扫收款码转账 → 监听端推送 → 服务端匹配入账
//   - 支付凭据 = 真实收款码直发（/qr/real 动态图片接口）
//   - 每笔订单分配 4 位备注码，用户转账时填写 → 精确匹配
//   - 账户池复用：每个账户 = 一个收款码（QRContent），轮换/冷却/限额复用
//
// 实现要点（docs/MIANQIAN.md）：
//   - CreatePayment：选账户 → 分配备注码 → 生成二维码 token → 直发码 URL
//   - 收款确认：/personal/push（监听端推送）→ HMAC 验签 → 备注码/金额匹配 → SettlePayment
//   - VerifyNotify / QueryOrder 不支持（个人通道无官方 API），分别返回错误
package personal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// Config 个人通道配置
type Config struct {
	BaseURL   string        // 网关公网基础地址（管理后台展示，隧道域名动态更新）
	OrderTTL  time.Duration // 订单有效期（备注码/二维码 token 过期时间）
	NoteTTL   time.Duration // 备注码占用有效期（默认 5 分钟）
	TokenTTL  time.Duration // 二维码 token 有效期（默认 30 分钟）
	MaxAmount int64         // 单笔限额（分，0=不限；docs/MIANQIAN.md §10 风控）
}

// Channel 个人免签通道实现
type Channel struct {
	cfg        Config
	pool       *account.AccountPool
	store      store.Store         // 业务库（订单查询/匹配）
	personalDB store.PersonalStore // personal.db（设备/推送流水）
	alloc      *personal.NoteCodeAllocator
	provider   channel.Provider                              // alipay / wechat
	baseURL    string                                        // 当前公网基础地址（SetBaseURL 动态更新）
	adbOnline  func(ctx context.Context, serial string) bool // 设备 adb 可达判定（main.go 注入 adbMgr().Online）

	// balancedSelect 均衡分配器（main.go 注入，nil = 退化为旧 selectServableAccount 加权随机）：
	// 两层分配：先选设备（最闲最健康），再选设备内账户（最闲最健康）。
	// 返回选中账户；nil = 无可用。main.go 实现持有 waitPayPool/ledgermon 等负载数据源。
	balancedSelect func(ctx context.Context) *account.Account
}

// SetBalancedSelect 注入均衡分配器（docs/PLAN-DYNAMIC-CLOSE-ALLOC.md §5）。
// nil = 退化为旧逻辑（SelectFrom 加权随机，不感知负载）。
func (c *Channel) SetBalancedSelect(fn func(ctx context.Context) *account.Account) {
	c.balancedSelect = fn
}

// SetAdbOnline 注入设备 adb 可达判定函数（main.go 注入 adbMgr().Online 包装）。
// 2026-08-29 重构：设备在线判定改为 adb 可达性优先，不再依赖监听 App 心跳。
func (c *Channel) SetAdbOnline(fn func(ctx context.Context, serial string) bool) {
	c.adbOnline = fn
}

// New 创建个人免签通道
// provider: channel.ProviderAlipay / channel.ProviderWechat
func New(cfg Config, provider channel.Provider, pool *account.AccountPool, st store.Store, pdb store.PersonalStore) *Channel {
	if cfg.OrderTTL <= 0 {
		cfg.OrderTTL = 30 * time.Minute
	}
	if cfg.NoteTTL <= 0 {
		cfg.NoteTTL = personal.NoteCodeTTL
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 30 * time.Minute
	}
	return &Channel{
		cfg:        cfg,
		pool:       pool,
		store:      st,
		personalDB: pdb,
		alloc:      personal.NewNoteCodeAllocator(),
		provider:   provider,
		baseURL:    cfg.BaseURL,
	}
}

// SetBaseURL 动态更新公网基础地址（隧道重启换域名时调用）
func (c *Channel) SetBaseURL(url string) {
	c.baseURL = strings.TrimSuffix(url, "/")
	logger.Info("personal", "公网地址已更新", "provider", c.provider, "base_url", c.baseURL)
}

// BaseURL 当前公网基础地址（管理后台展示）
func (c *Channel) BaseURL() string { return c.baseURL }

// channelName 通道注册名
func (c *Channel) channelName() string { return "personal." + string(c.provider) }

// Name 通道标识
func (c *Channel) Name() string { return c.channelName() }

// Type 通道大类：个人通道
func (c *Channel) Type() channel.ChannelType { return channel.TypePersonal }

// Provider 支付品牌
func (c *Channel) Provider() channel.Provider { return c.provider }

// Kind 通道类型：监听驱动
func (c *Channel) Kind() channel.ChannelKind { return channel.KindListener }

// ConfirmAbility 确认能力：无法主动查单，最终兜底 = 人工异常池
func (c *Channel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByManual }

// Modes 支持的支付模式：仅扫码（个人收款码）
func (c *Channel) Modes() []channel.PayMode {
	return []channel.PayMode{channel.ModeQRCode}
}

// Listener 监听驱动但通过 HTTP 推送，无独立事件监听器
func (c *Channel) Listener() channel.EventListener { return nil }

// ErrNotQueryable 个人通道无法主动查单
var ErrNotQueryable = errors.New("个人通道无法主动查单（靠监听端推送确认）")

// formatYuan 分 → 元字符串（如 50000 → "500.00"）
func formatYuan(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// CreatePayment 下单（个人免签）
//
// 流程：
//  1. 账户池选一个可用收款账户（含收款码内容 QRContent）
//  2. 分配 4 位备注码（系统生成，5 分钟占用）
//  3. 生成二维码 token（HMAC 签名，防劫持）
//  4. 直发码 URL：/qr/real?token={token}（动态图片接口下发真实收款码）
//
// 返回的 PayCredential.QRCode 就是二维码内容；前端按 src 渲染。
func (c *Channel) CreatePayment(ctx context.Context, req *channel.PayRequest) (*channel.PayCredential, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("个人通道公网地址未配置（隧道未就绪）")
	}
	// 单笔限额风控（docs/MIANQIAN.md §10）：大额拒绝下单 + 日志告警
	if c.cfg.MaxAmount > 0 && req.AmountMinor > c.cfg.MaxAmount {
		logger.Warn("personal", "单笔限额风控：金额超限，拒绝下单",
			"provider", c.provider, "out_trade_no", req.OutTradeNo,
			"amount", req.AmountMinor, "max", c.cfg.MaxAmount)
		return nil, fmt.Errorf("单笔金额超限（最多 ¥%s）", formatYuan(c.cfg.MaxAmount))
	}
	// 1. 选账户（仅选有在线监听设备绑定的账户，防路由到无设备账户造成死单）
	acct := c.selectServableAccount(ctx)
	if acct == nil {
		return nil, fmt.Errorf("%s 个人通道暂无可用收款账户（无在线监听设备绑定或已达日限额/冷却中）", c.provider)
	}
	if acct.QRContent == "" {
		return nil, fmt.Errorf("账户「%s」未配置收款码内容（qr_content），无法接单", acct.Name)
	}
	if reason := account.InvalidQRContent(c.channelName(), acct.QRContent); reason != "" {
		logger.Error("personal", "账户收款码内容非法，拒绝路由（脏数据防线）",
			"provider", c.provider, "account", acct.Name, "account_id", acct.ID, "reason", reason)
		return nil, fmt.Errorf("账户「%s」收款码内容异常（%s），已拒绝路由，请联系管理员修复", acct.Name, reason)
	}
	// 2. 分配备注码
	noteCode, err := c.alloc.Alloc()
	if err != nil {
		return nil, fmt.Errorf("备注码分配失败: %w", err)
	}
	// 3. 生成二维码 token（自包含签名，/qr/real 动态图片接口用）
	token, err := personal.GenerateQrToken(req.OutTradeNo, time.Now().Add(c.cfg.TokenTTL))
	if err != nil {
		c.alloc.Release(noteCode)
		return nil, fmt.Errorf("二维码 token 生成失败: %w", err)
	}
	qrURL := "/qr/real?token=" + token
	// 通知栏监听方案只依赖 包名+金额+时间 匹配，备注码已无提示意义；日志不再提备注码
	logger.Info("personal", "下单成功（直发码）",
		"provider", c.provider, "out_trade_no", req.OutTradeNo,
		"account", acct.Name, "account_id", acct.ID,
		"amount", req.AmountMinor)

	// 4. 保存备注码/套码 token 到订单（order.Service 通过 cred 回调写回，这里无法直接写）
	// 通过 PayCredential 附加字段透传给 order.Service 持久化
	return &channel.PayCredential{
		QRCode:    qrURL,
		Mode:      channel.ModeQRCode,
		ExpireAt:  time.Now().Add(c.cfg.OrderTTL),
		AccountID: acct.ID,
		// 个人通道附加数据：备注码 + 套码 token + 收款码内容（用于 /mq 302 / /qr/real 图片下发）
		NoteCode:  noteCode,
		QrToken:   token,
		QRContent: acct.QRContent,
	}, nil
}

// VerifyNotify 个人通道不支持 HTTP 验签回调（收款确认走 /personal/push，HMAC 验签在 handler）
func (c *Channel) VerifyNotify(ctx context.Context, req *http.Request) (*channel.NotifyResult, error) {
	return nil, fmt.Errorf("个人通道不支持 HTTP 回调验签，收款确认走 /personal/push")
}

// QueryOrder 个人通道无法主动查单
func (c *Channel) QueryOrder(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error) {
	return nil, ErrNotQueryable
}

// SelectAccount 按收款码内容选择可用账户（供 /mq 302 校验用）
func (c *Channel) SelectAccount() *account.Account {
	return c.pool.Select(c.channelName(), "default")
}

// servableAccountIDs 返回有**adb 可达**设备绑定的账户 ID 集合（设备主账户 + 附加账户）。
// 设备可用判定：adb 可达（USB/WiFi/LAN）。
// 不再依赖监听 App 心跳/DB status（2026-08-29 重构）。
// 设备冻结/通道不匹配均不计入；查询失败返回 nil（调用方按无可用账户处理）。
func (c *Channel) servableAccountIDs(ctx context.Context) map[int64]bool {
	devs, err := c.personalDB.ListDevices(ctx)
	if err != nil {
		logger.Error("personal", "查询监听设备失败", "err", err)
		return nil
	}
	out := make(map[int64]bool)
	for _, d := range devs {
		// 设备可用判定：adb 可达性。
		// 注：不做 d.Provider 过滤——一台真机同时装有支付宝+微信，设备行只存单一 provider
		// 标签，按标签过滤会让另一平台永远无账户可用（老 bug，被僵尸设备数据掩盖过）。
		// 通道归属由 SelectFrom 的 channel 过滤保证。
		if serial := c.serialOf(d); serial == "" || c.adbOnline == nil || !c.adbOnline(ctx, serial) {
			continue
		}
		for _, id := range store.DeviceAccountIDs(ctx, c.personalDB, d.ID, d.AccountID) {
			out[id] = true
		}
	}
	return out
}

// serialOf 解析设备 adb serial（TCP host:port 或 USB 纯 serial）
func (c *Channel) serialOf(d *personal.DeviceEncrypted) string {
	if d.AdbHost != "" {
		if d.AdbPort > 0 {
			return fmt.Sprintf("%s:%d", d.AdbHost, d.AdbPort)
		}
		return d.AdbHost
	}
	if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
		return fmt.Sprintf("%s:%d", d.AdbWifiHost, d.AdbWifiPort)
	}
	return ""
}

// selectServableAccount 设备感知选号：可用账户 ∩ 在线监听设备绑定账户。
// 均衡分配器已注入时走两层评分（设备负载×账户负载×健康度）；否则退化为加权随机。
func (c *Channel) selectServableAccount(ctx context.Context) *account.Account {
	if c.balancedSelect != nil {
		return c.balancedSelect(ctx)
	}
	servable := c.servableAccountIDs(ctx)
	if len(servable) == 0 {
		logger.Warn("personal", "无在线监听设备可服务，拒绝路由", "provider", c.provider)
		return nil
	}
	return c.pool.SelectFrom(c.channelName(), "default", servable)
}

// GetAccountByID 按 ID 获取账户
func (c *Channel) GetAccountByID(id int64) *account.Account {
	return c.pool.GetByID(id)
}

// ReleaseNoteCode 释放备注码（订单支付成功/关闭后调用）
// 幂等：重复释放不会产生副作用（delete 不存在 key 是 no-op）
func (c *Channel) ReleaseNoteCode(code string) {
	if code != "" {
		c.alloc.Release(code)
	}
}
