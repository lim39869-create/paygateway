// paygateway 支付网关 channels（从 main.go 拆分，通道注册/设备管理/中间件）
package main

import (
	"fmt"
	"paygateway/internal/account"
	"paygateway/internal/adbmgr"
	"paygateway/internal/admin"
	"paygateway/internal/channel"
	alipaych "paygateway/internal/channel/alipay"
	personalch "paygateway/internal/channel/personal"
	wechatch "paygateway/internal/channel/wechat"
	"paygateway/internal/logger"
	"paygateway/internal/store"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func adbMgr() *adbmgr.Manager {
	return adbmgr.New("")
}

// regChannels 注册支付通道（每加一种支付方式 = 这里加一行）
// 返回个人免签通道实例（main 用于动态更新公网地址 + 推送处理）
func regChannels(cfg *Config, accountPool *account.AccountPool, st store.Store, personalDB store.PersonalStore) (*personalch.Channel, *personalch.Channel) {
	// 通道始终注册（可插拔），支付账户一律从账户池读取（管理后台「账户池」配置，加密入库）
	// 空 Config 仅保留回调地址：账户的 AppID/私钥/公钥/环境由各自账户决定
	alipayCh, err := alipaych.New(alipaych.Config{
		NotifyURL: cfg.AlipayNotifyURL,
		Debug:     cfg.ChannelDebug,
	}, accountPool)
	if err != nil {
		logger.Error("main", "支付宝通道初始化失败", "err", err)
	} else {
		channel.Register(alipayCh)
		logger.Info("main", "支付宝通道注册成功 ✅", "accounts", accountPool.CountByChannel("alipay"))
	}

	wxCh, err := wechatch.New(wechatch.Config{
		NotifyURL: cfg.WechatNotifyURL,
		Debug:     cfg.ChannelDebug,
	}, accountPool)
	if err != nil {
		logger.Error("main", "微信通道初始化失败", "err", err)
	} else {
		channel.Register(wxCh)
		logger.Info("main", "微信通道注册成功 ✅", "accounts", accountPool.CountByChannel("wechat"))
	}

	// 个人免签通道（阶段 2）：personal.alipay / personal.wechat
	// 账户池 channel = "personal.alipay" / "personal.wechat"，每个账户 = 一个收款码（qr_content）
	baseURL := cfg.PersonalBaseURL
	if baseURL == "" {
		port := strings.TrimPrefix(cfg.ListenAddr, ":")
		baseURL = "http://127.0.0.1:" + port
	}
	var personalAlipay, personalWechat *personalch.Channel
	personalAlipay = personalch.New(personalch.Config{BaseURL: baseURL, MaxAmount: cfg.PersonalMaxAmount}, channel.ProviderAlipay, accountPool, st, personalDB)
	channel.Register(personalAlipay)
	logger.Info("main", "个人免签支付宝通道注册成功 ✅", "accounts", accountPool.CountByChannel("personal.alipay"))

	personalWechat = personalch.New(personalch.Config{BaseURL: baseURL, MaxAmount: cfg.PersonalMaxAmount}, channel.ProviderWechat, accountPool, st, personalDB)
	channel.Register(personalWechat)
	logger.Info("main", "个人免签微信通道注册成功 ✅", "accounts", accountPool.CountByChannel("personal.wechat"))

	return personalAlipay, personalWechat
}

// buildEpayInfo 易支付服务地址信息（admin 展示用）
// 多商户模式下不再有全局 pid/key：每个上游的 pid/密钥/对接指引在「上游对接」页独立配置
// 展示地址优先公网（固定隧道域名）：给上游看的对接信息写 127.0.0.1 会让对方配成内网兜底失效
func buildEpayInfo(cfg *Config) *admin.EpayInfo {
	base := cfg.EpayPublicBase
	if base == "" {
		base = cfg.PersonalBaseURL
	}
	if base == "" {
		port := strings.TrimPrefix(cfg.ListenAddr, ":")
		base = "http://127.0.0.1:" + port
	}
	base = strings.TrimSuffix(base, "/")
	return &admin.EpayInfo{
		BaseURL:   base,
		SubmitURL: base + "/submit.php",
		QueryURL:  base + "/api.php",
		Pid:       "",
		Key:       "",
		SignType:  "MD5",
	}
}

// buildChannelInfos 组装通道信息给管理后台（admin 不依赖具体通道包）
func buildChannelInfos(pool *account.AccountPool) []admin.ChannelInfo {
	var out []admin.ChannelInfo
	for _, name := range channel.AllNames() {
		ch, ok := channel.Lookup(name)
		if !ok {
			continue
		}
		info := admin.ChannelInfo{
			Name:    name,
			Enabled: channel.IsEnabled(name),
			Kind:    "callback",
		}
		// 按通道实际类型修正 kind（callback / listener）
		if ch.Kind() == channel.KindListener {
			info.Kind = "listener"
		}
		// 账户配置状态（引导用）
		if pool != nil {
			info.AccountCount = pool.CountByChannel(name)
			info.Configured = info.AccountCount > 0
		}
		for _, m := range ch.Modes() {
			info.Modes = append(info.Modes, string(m))
		}
		// 通道特有信息
		// 支付模式启停状态（有 EnabledModes 方法的通道）
		type modeProvider interface {
			EnabledModes() map[string]bool
		}
		if mp, ok := ch.(modeProvider); ok {
			info.ModesEnabled = mp.EnabledModes()
		}

		// 支付宝特有信息（AppID/环境从账户池取第一个账户展示）
		if ac, ok := ch.(*alipaych.Channel); ok {
			info.NotifyURL = ac.NotifyURL()
			if accts := pool.ListByChannel("alipay"); len(accts) > 0 {
				info.AppID = accts[0].AppID
				info.IsProd = accts[0].IsProd
			}
		}
		// 微信通道特有信息
		if wc, ok := ch.(*wechatch.Channel); ok {
			info.NotifyURL = wc.NotifyURL()
			if accts := pool.ListByChannel("wechat"); len(accts) > 0 {
				info.AppID = accts[0].AppID
				info.MchID = accts[0].MchID
				info.IsProd = accts[0].IsProd
			}
		}
		out = append(out, info)
	}
	return out
}

// traceMiddleware 为每个请求生成 trace_id

// traceMiddleware 为每个请求生成 trace_id
func traceMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID := c.GetHeader("X-Trace-ID")
		if traceID == "" {
			traceID = fmt.Sprintf("tr-%d", time.Now().UnixNano())
		}
		c.Set("trace_id", traceID)
		c.Header("X-Trace-ID", traceID)
		c.Next()
	}
}

// ========== 易支付下单接口 ==========

// epayKeyFor 按 pid 返回验签密钥（多商户，严格模式）
// 返回 (key, true)：pid 已登记且启用
// 返回 ("", false)：pid 未登记或已禁用 → 拒绝（不再有任何 .env / 默认密钥回退）
