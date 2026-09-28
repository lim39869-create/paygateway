// 收款码自动续期 + 定期清理调度。
// 续期：扫描设备各平台收款码，缺失/非 active/过期 → 自动 adb 捕获补新码（静态码长期有效，正常不触发）。
// 清理：DB 删过期记录（每设备每平台保留最新）；设备端微信相册保留最近 N 张。
package admin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// qrcodeExpireIn 保存的收款码有效期（秒）。微信/支付宝「保存收款码」是静态码长期有效，
// 设 7 天作安全缓冲（防极端情况下码失效），到期由续期调度补捕。
const qrcodeExpireIn = 7 * 24 * 3600

// qrcodeRenewPlatforms 续期检查的平台（服务端 adb 主动捕获）
var qrcodeRenewPlatforms = []string{"wechat", "alipay"}

// qrcodeMinRenewInterval 每平台两次自动捕获的最小间隔（防调度异常时无限重捕风暴）
const qrcodeMinRenewInterval = 10 * time.Minute

// qrcodeUserSpaces 每设备自动捕获的 Android 用户空间。
// 0 = 主空间（机主），999 = 双开（小米 XSpace 分身）。
// 覆盖微信/支付宝在双开空间也有安装的场景。
var qrcodeUserSpaces = []int{0, 999}

// RenewQrcodes 自动续期调度：扫描所有设备各平台各用户空间的收款码，发现缺失/非 active/已过期
// → 自动触发服务端 adb 捕获补新码。静态码长期有效，正常情况几乎不触发；
// 仅在码丢失或明确过期时补捕，且每平台每用户空间受最小间隔限制。
//
// 由 main 以固定间隔（默认 1 小时）调用。
func (h *PersonalHandler) RenewQrcodes(ctx context.Context) {
	if h.adbMgr == nil || h.pdb == nil {
		return
	}
	devs, err := h.pdb.ListDevices(ctx)
	if err != nil {
		logger.Error("admin", "续期扫描: 获取设备列表失败", "err", err)
		return
	}
	for _, dev := range devs {
		serial, _, ok := h.pickOnlineSerial(ctx, dev)
		if !ok {
			continue // 设备离线，无法捕获
		}
		for _, userID := range qrcodeUserSpaces {
			for _, platform := range qrcodeRenewPlatforms {
				if !h.qrcodeNeedsRenew(ctx, dev.DeviceID, platform, userID) {
					continue
				}
				// 限频：距上次自动捕获不足最小间隔则跳过（防重捕风暴）
				if h.renewLimiter != nil && !h.renewLimiter.allow(dev.DeviceID, platform, userID) {
					logger.Debug("admin", "收款码补捕被限频跳过", "device", dev.DeviceID, "platform", platform, "user", userID)
					continue
				}
				h.renewOneQrcode(ctx, dev, serial, platform, userID)
			}
		}
	}
}

// qrcodeNeedsRenew 判断设备某平台某用户空间是否需要补捕：无码 / 非 active / 已过期。
// 静态码未过期不触发（不做提前续期，避免无意义捕获）。
func (h *PersonalHandler) qrcodeNeedsRenew(ctx context.Context, deviceID, platform string, userID int) bool {
	latest, err := h.pdb.GetLatestQrcodeByPlatformUser(ctx, deviceID, platform, userID)
	if err != nil || latest == nil {
		return true // 无码 → 需要
	}
	if latest.Status != "active" {
		return true
	}
	expireAt, err := time.Parse(time.RFC3339, latest.ExpireAt)
	if err != nil {
		return true // 过期时间解析失败 → 重捕
	}
	return time.Now().After(expireAt)
}

// qrcodeLastRenew 记录每设备每平台每用户空间最近一次自动捕获时间（限频）
type qrcodeLastRenew struct {
	mu    sync.Mutex
	lasts map[string]time.Time // key: deviceID|platform|userID
}

// allow 检查并记录该设备该平台该用户空间是否允许自动捕获（距上次至少 qrcodeMinRenewInterval）
func (r *qrcodeLastRenew) allow(deviceID, platform string, userID int) bool {
	key := fmt.Sprintf("%s|%s|%d", deviceID, platform, userID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.lasts[key]; ok && time.Since(t) < qrcodeMinRenewInterval {
		return false
	}
	r.lasts[key] = time.Now()
	return true
}

// renewOneQrcode 对单个设备单平台单用户空间执行一次续期捕获并入库（失败记日志，下轮再试）。
func (h *PersonalHandler) renewOneQrcode(ctx context.Context, dev *personal.DeviceEncrypted, serial, platform string, userID int) {
	ctx2, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var content string
	switch platform {
	case "wechat":
		_, _, c, err := capture.CaptureWechatQrcode(ctx2, h.adbMgr, serial, userID)
		if err != nil {
			logger.Warn("admin", "微信收款码续期失败", "device", dev.DeviceID, "user", userID, "err", err)
			return
		}
		content = c
	case "alipay":
		c, err := capture.CaptureAlipayQrcode(ctx2, h.adbMgr, serial, userID)
		if err != nil {
			logger.Warn("admin", "支付宝收款码续期失败", "device", dev.DeviceID, "user", userID, "err", err)
			return
		}
		content = c
	default:
		return
	}
	// 确保有对应的支付账户（自动创建 + 绑定），使支付闭环可用
	if _, created, aerr := h.ensureAccountForQR(ctx, dev, platform, content, userID); aerr != nil {
		logger.Error("admin", "自动创建账户失败", "device", dev.DeviceID, "platform", platform, "err", aerr)
	} else if created {
		logger.Info("admin", "收款码自动续期已同步创建账户", "device", dev.DeviceID, "platform", platform)
	}
	q := &personal.DeviceQrcode{
		DeviceID:  dev.DeviceID,
		Platform:  platform,
		UserID:    userID,
		QRContent: content,
		Account:   h.resolveQrcodeAccount(ctx, dev, platform, userID, content),
		ExpireIn:  qrcodeExpireIn,
		ExpireAt:  store.ExpireAtStr(qrcodeExpireIn),
		Status:    "active",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if _, err := h.pdb.UpsertQrcode(ctx, q); err != nil {
		logger.Error("admin", "续期收款码入库失败", "device", dev.DeviceID, "platform", platform, "user", userID, "err", err)
		return
	}
	logger.Info("admin", "收款码自动续期完成", "device", dev.DeviceID, "platform", platform, "user", userID,
		"account", q.Account, "content", capture.TruncateStr(content, 40))
}

// platformAccountName 按平台解析设备绑定账户名（微信/支付宝），并加上用户空间后缀（主空间/双开）。
// 使管理后台能区分同一平台不同空间的收款码归属于哪个账户。
func (h *PersonalHandler) platformAccountName(ctx context.Context, dev *personal.DeviceEncrypted, platform string, userID int) string {
	var base string
	switch platform {
	case "wechat":
		base = h.wechatAccountName(ctx, dev)
	case "alipay":
		base = h.alipayAccountName(ctx, dev)
	default:
		base = dev.Name
	}
	return spaceAccountName(base, userID)
}

// spaceAccountName 账户名追加用户空间后缀（主空间/双开），区分同一平台不同空间的收款码归属。
func spaceAccountName(base string, userID int) string {
	spaceLabel := "主空间"
	if userID == 999 {
		spaceLabel = "双开"
	}
	return base + " (" + spaceLabel + ")"
}

// resolveQrcodeAccount 自动识别捕获到的收款码归属账户。
// 原理：每个 personal 账户在账户池中配置了收款码内容（QRContent），
// 用本次捕获到的二维码内容与设备绑定账户逐一比对，命中即确认账户身份，
// 从而「自动知道那个账户是那个空间的什么账户」。
// 匹配失败时回退 platformAccountName（绑定顺序取第一个同平台账户）。
func (h *PersonalHandler) resolveQrcodeAccount(ctx context.Context, dev *personal.DeviceEncrypted, platform string, userID int, qrContent string) string {
	if dev == nil || qrContent == "" {
		return ""
	}
	if h.pool != nil {
		prefix := "personal.alipay"
		if platform == "wechat" {
			prefix = "personal.wechat"
		}
		// 附加账户优先，主账户兜底：同一平台可能绑定多个账户（主空间+双开各一个）
		ids := store.DeviceAccountIDs(ctx, h.pdb, dev.ID, dev.AccountID)
		for _, id := range ids {
			acct := h.pool.GetByID(id)
			if acct == nil || !strings.HasPrefix(acct.Channel, prefix) {
				continue
			}
			if acct.QRContent == qrContent {
				logger.Info("admin", "收款码账户自动识别", "device", dev.DeviceID, "platform", platform,
					"user", userID, "account", acct.Name)
				return spaceAccountName(acct.Name, userID)
			}
		}
		logger.Warn("admin", "收款码账户未匹配，回退绑定账户", "device", dev.DeviceID,
			"platform", platform, "user", userID, "content", capture.TruncateStr(qrContent, 30))
	}
	return h.platformAccountName(ctx, dev, platform, userID)
}

// CleanupQrcodes 定期清理：
//  1. DB 层删除过期收款码记录（每设备保留最新一条，防清空后无码可用）
//  2. 设备端相册清理：对每台在线设备保留最近 N 张微信收款码图片，删更旧的
//
// 由 main 以固定间隔（默认 1 小时）调用。
func (h *PersonalHandler) CleanupQrcodes(ctx context.Context) {
	// 1. DB 清理
	if h.pdb != nil {
		if n, err := h.pdb.DeleteExpiredQrcodes(ctx); err != nil {
			logger.Error("admin", "收款码 DB 清理失败", "err", err)
		} else if n > 0 {
			logger.Info("admin", "已清理过期收款码记录", "count", n)
		}
	}
	// 2. 设备端相册清理
	if h.adbMgr == nil {
		return
	}
	devs, err := h.pdb.ListDevices(ctx)
	if err != nil {
		logger.Error("admin", "收款码相册清理: 获取设备列表失败", "err", err)
		return
	}
	for _, dev := range devs {
		serial, _, ok := h.pickOnlineSerial(ctx, dev)
		if !ok {
			continue // 设备离线，跳过（离线时相册不增长）
		}
		for _, userId := range []int{0, 999} {
			if n := capture.CleanupWechatGallery(ctx, h.adbMgr, serial, userId); n > 0 {
				logger.Info("admin", "已清理设备旧收款码图", "device", serial, "user", userId, "deleted", n)
			}
			if n := capture.CleanupAlipayGallery(ctx, h.adbMgr, serial, userId); n > 0 {
				logger.Info("admin", "已清理设备旧支付宝收款码图", "device", serial, "user", userId, "deleted", n)
			}
		}
	}
}
