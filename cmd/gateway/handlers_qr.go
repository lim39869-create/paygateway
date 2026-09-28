// paygateway 支付网关 handlers_qr（从 main.go 拆分，QR 系接口/指纹/参数收集）
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/web"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (a *App) handleBindFingerprint(c *gin.Context) {
	orderNo := c.Query("o")
	fpB64 := c.Query("fp")
	if orderNo == "" || fpB64 == "" {
		c.Status(http.StatusOK)
		return
	}
	// base64 解码指纹 JSON（失败静默，指纹是增强非必须）
	fpJSON, err := base64.StdEncoding.DecodeString(fpB64)
	if err != nil || len(fpJSON) == 0 || len(fpJSON) > 1024 {
		c.Status(http.StatusOK)
		return
	}
	// 指纹单独存 fp_key 列（不改写 buyer_key）：
	// buyer_key 保持 = H(ip|ua|device|buyer_id) 不变，串行检查 buyer_key 或 fp_key 均命中；
	// 换 IP 场景：buyer_key 变但 fp_key 同 → 仍被串行拦截
	fpHash := sha256.Sum256(fpJSON)
	fpKey := fmt.Sprintf("%x", fpHash[:16])
	ctx := c.Request.Context()
	o, err := a.store.GetOrder(ctx, orderNo)
	if err != nil || o == nil || o.Status != model.StatusWaitPay {
		c.Status(http.StatusOK)
		return
	}
	if _, err := a.store.UpdateOrderStatus(ctx, orderNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.FPKey = fpKey
	}); err != nil {
		logger.Warn("personal", "指纹绑定失败", "err", err, "order", orderNo)
	}
	// Set-Cookie 写入指纹 hash：后续下单携带该 cookie，服务端按 fp_key 匹配（换 IP 串行兜底）
	c.SetCookie("pg_fp", fpKey, 86400, "/", "", false, true)
	logger.Info("personal", "浏览器指纹已绑定", "order", orderNo, "fp_key", fpKey[:8], "fp_len", len(fpJSON))
	c.Status(http.StatusOK)
}

func (a *App) handleQrRefresh(c *gin.Context) {
	oldNo := c.Query("out_trade_no")
	if oldNo == "" {
		c.String(http.StatusOK, "参数缺失: out_trade_no")
		return
	}
	ctx := c.Request.Context()

	// 查原订单，校验状态
	o, err := a.store.GetOrder(ctx, oldNo)
	if err != nil || o == nil {
		c.String(http.StatusOK, "订单不存在")
		return
	}
	if o.Status != model.StatusWaitPay {
		c.String(http.StatusOK, "订单当前状态("+o.Status+")不可刷新")
		return
	}

	logger.Info("qr", "刷新二维码", "out_trade_no", oldNo)

	// 保持原单号刷新支付凭据（重新分配备注码 + 套码 token）
	refresh, err := a.orders.RefreshPayment(ctx, o)
	if err != nil {
		logger.Error("qr", "刷新二维码失败", "err", err, "old", oldNo)
		c.String(http.StatusOK, "刷新失败: "+err.Error())
		return
	}
	// 通知栏监听驱动：账本导航兜底已废弃（用户决策），刷新后由监听端通知推送匹配入账
	a.renderPayment(c, refresh)
}

func (a *App) handleQrExpired(c *gin.Context) {
	outNo := c.Query("out_trade_no")
	provider := ""
	if outNo != "" {
		if o, err := a.store.GetOrder(c.Request.Context(), outNo); err == nil && o != nil {
			if strings.HasSuffix(o.Channel, "wechat") {
				provider = "微信"
			} else if strings.HasSuffix(o.Channel, "alipay") {
				provider = "支付宝"
			}
		}
	}
	a.setPaymentPageHeaders(c)
	// 过期订单是终态，不提供复活入口（防僵尸订单刷单）；引导返回上游重新下单
	c.String(http.StatusOK, web.BuildExpiredPage(provider, outNo, c.Query("return_url")))
}

// handleQrStatus 订单状态探测：过期页 JS 周期性轮询。
// 上游同号重提复活订单后返回 alive=true，过期页自动跳回支付页继续支付。
// 仅返回轻量 JSON，不携带敏感信息。
func (a *App) handleQrStatus(c *gin.Context) {
	outNo := c.Query("out_trade_no")
	logger.Info("qr", "status 探测", "out_trade_no", outNo, "ua", c.Request.UserAgent())
	if outNo == "" {
		c.JSON(http.StatusOK, gin.H{"alive": false})
		return
	}
	o, err := a.store.GetOrder(c.Request.Context(), outNo)
	if err != nil || o == nil {
		c.JSON(http.StatusOK, gin.H{"alive": false})
		return
	}
	if o.Status == model.StatusWaitPay && (o.PayURL != "" || o.QrCode != "") {
		// 个人免签订单：关单时间已到（页面倒计时已归零 / 后台扫描器即将关单）→ 视为过期，
		// 返回 alive=false，让过期页停在该分支，避免「过期页→/qr/refresh→支付页」死循环幽灵订单。
		if strings.HasPrefix(o.Channel, "personal.") {
			if ct := a.personalOrderCloseAt(o); !ct.IsZero() && time.Now().After(ct) {
				c.JSON(http.StatusOK, gin.H{"alive": false, "expired": true, "status": model.StatusClosed})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"alive": true, "channel": o.Channel})
		return
	}
	c.JSON(http.StatusOK, gin.H{"alive": false, "status": o.Status})
}

// handleQrSuccess 支付成功落地页：仅当订单状态为 TRADE_SUCCESS 时渲染成功页，
// 否则（未支付/已关闭/不存在）回退到支付页或过期页，防伪造"支付成功"页面。
// GET /qr/success?out_trade_no={no}
func (a *App) handleQrSuccess(c *gin.Context) {
	outNo := c.Query("out_trade_no")
	if outNo == "" {
		c.String(http.StatusOK, "参数缺失: out_trade_no")
		return
	}
	o, err := a.store.GetOrder(c.Request.Context(), outNo)
	if err != nil || o == nil {
		c.String(http.StatusOK, "订单不存在")
		return
	}
	if o.Status != model.StatusSuccess {
		// 未支付/确认中 → 回到支付页继续支付；已关闭/失败 → 过期引导页
		if o.Status == model.StatusWaitPay || o.Status == model.StatusConfirming {
			a.renderPayment(c, o)
			return
		}
		a.setPaymentPageHeaders(c)
		c.String(http.StatusOK, web.BuildExpiredPage("", outNo, o.ReturnURL))
		return
	}
	a.setPaymentPageHeaders(c)
	c.String(http.StatusOK, web.BuildSuccessPage(o, o.Channel))
}

func (a *App) handleQrSwitch(c *gin.Context) {
	outNo := c.Query("out_trade_no")
	method := c.Query("method") // alipay / wechat
	if outNo == "" || method == "" {
		c.String(http.StatusOK, "参数缺失: out_trade_no / method")
		return
	}
	ctx := c.Request.Context()
	o, err := a.store.GetOrder(ctx, outNo)
	if err != nil || o == nil {
		c.String(http.StatusOK, "订单不存在")
		return
	}
	if !strings.HasPrefix(o.Channel, "personal.") {
		c.String(http.StatusOK, "该订单不支持切换支付方式")
		return
	}
	// method → 个人免签通道名
	newChannel := ""
	switch method {
	case "alipay":
		newChannel = "personal.alipay"
	case "wechat":
		newChannel = "personal.wechat"
	}
	if newChannel == "" || newChannel == o.Channel {
		c.String(http.StatusOK, "无效的切换目标")
		return
	}
	// 目标通道必须有可用收款账户（无账户切过去会白屏报错）
	if !a.personalChannelReady(newChannel) {
		c.String(http.StatusOK, "该支付方式暂不可用（无可用收款账户）")
		return
	}
	logger.Info("qr", "切换支付方式", "out_trade_no", outNo, "from", o.Channel, "to", newChannel)
	switched, err := a.orders.SwitchChannel(ctx, o, newChannel)
	if err != nil {
		logger.Error("qr", "切换支付方式失败", "err", err, "out_trade_no", outNo)
		c.String(http.StatusOK, "切换支付方式失败: "+err.Error())
		return
	}
	a.renderPayment(c, switched)
}

// collectParams 收集 GET query + POST form 参数
// buyerKeyFor 计算买家环境唯一标识（买家级串行/防刷单）。
// 环境 = 客户端 IP + User-Agent + device 参数 + 可选 buyer_id（下游可透传真实用户标识），
// SHA-256 哈希后作为 buyer_key；同环境同时只允许一笔待支付订单。
// 浏览器指纹(cookie pg_fp)单独作为 fp_key 返回，用于换 IP 场景串行兜底。
func buyerKeyFor(c *gin.Context) (buyerKey, fpKey string) {
	ip := c.ClientIP()
	ua := c.GetHeader("User-Agent")
	device := c.Query("device")
	bid := c.Query("buyer_id")
	if ip == "" && bid == "" {
		return "", ""
	}
	h := sha256.Sum256([]byte(ip + "|" + ua + "|" + device + "|" + bid))
	fp := c.GetHeader("Cookie")
	if fp != "" {
		for _, part := range strings.Split(fp, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "pg_fp=") {
				fpKey = strings.TrimPrefix(part, "pg_fp=")
				break
			}
		}
	}
	return fmt.Sprintf("%x", h[:16]), fpKey
}

func collectParams(c *gin.Context) map[string]string {
	params := make(map[string]string)
	for k, v := range c.Request.URL.Query() {
		if len(v) > 0 {
			params[k] = v[0]
		}
	}
	if c.Request.Method == http.MethodPost {
		c.Request.ParseForm()
		for k, v := range c.Request.PostForm {
			params[k] = v[0]
		}
	}
	return params
}

// ========== 个人免签通道：监听端通信 ==========

// decryptDeviceSecret 解密设备通信密钥（复用账户 AES 主密钥）
// 设备密钥以密文存 personal.db，这里用账户包的全局解密函数
