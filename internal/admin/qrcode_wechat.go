// 微信收款码捕获 HTTP 层：设备查找 → 调用 capture 包自动化 → 去重 → 入库。
package admin

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// handleCaptureWechatQrcode 通过服务器 adb 全自动捕获微信个人收款码
// POST /api/personal/qrcodes/:deviceId/wechat/capture?user=0|999
// 流程：唤醒 → 启动微信(可指定双开 user) → 导航到收付款页 → 切「个人收款码」→ 点「保存收款码」
//
//	→ 相册拉最新 → 解码 → 与库中最新码去重 → 入库
func (h *PersonalHandler) handleCaptureWechatQrcode(c *gin.Context) {
	deviceID := c.Param("deviceId")
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "设备控制台未启用（adb 管理器未配置）"})
		return
	}
	dev, err := h.pdb.GetDevice(c.Request.Context(), deviceID)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	// 双开空间参数（默认 0 = 主空间；>0 如 999 = 小米 XSpace 分身）
	userId, _ := strconv.Atoi(c.DefaultQuery("user", "0"))
	// 找可用的 adb serial（USB 优先 → WiFi 备用）
	serial, _, ok := h.pickOnlineSerial(c.Request.Context(), dev)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备离线：所有 adb 通道不可用"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	// 核心捕获（自动化逻辑在 capture 包）
	local, fileName, content, cerr := capture.CaptureWechatQrcode(ctx, h.adbMgr, serial, userId)
	if cerr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": cerr.Error()})
		return
	}
	defer os.Remove(local)

	// 去重：与库中该设备最新码比对，相同则不再入库
	if latest, lerr := h.pdb.GetLatestQrcode(ctx, deviceID); lerr == nil && latest != nil &&
		latest.Platform == "wechat" && latest.QRContent == content && latest.Status == "active" {
		logger.Info("admin", "微信收款码未变化，跳过入库", "device_id", deviceID, "file", fileName)
		// 码未变不代表账户同步过：仍需确保账户池 QRContent 与码一致（历史不同步的补救路径）
		if _, created, aerr := h.ensureAccountForQR(ctx, dev, "wechat", content, userId); aerr != nil {
			logger.Error("admin", "同步账户池失败", "device_id", deviceID, "user", userId, "err", aerr)
		} else if created {
			logger.Info("admin", "已自动创建账户", "device_id", deviceID, "platform", "wechat")
		}
		if n := capture.CleanupWechatGallery(ctx, h.adbMgr, serial, userId); n > 0 {
			logger.Info("admin", "已清理设备旧收款码图", "device", serial, "user", userId, "deleted", n)
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "unchanged": true, "content": content}})
		return
	}

	// 入库（保存的静态码长期有效，7 天安全缓冲；手动 adb 捕获可随时刷新）
	accountName := h.resolveQrcodeAccount(ctx, dev, "wechat", userId, content)
	q := &personal.DeviceQrcode{
		DeviceID:  dev.DeviceID,
		Platform:  "wechat",
		UserID:    userId,
		QRContent: content,
		Account:   accountName,
		ExpireIn:  qrcodeExpireIn,
		ExpireAt:  store.ExpireAtStr(qrcodeExpireIn),
		Status:    "active",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if _, err := h.pdb.UpsertQrcode(c.Request.Context(), q); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "收款码入库失败: " + err.Error()})
		return
	}
	logger.Info("admin", "微信收款码已捕获", "device_id", dev.DeviceID, "user", userId,
		"name", accountName, "file", fileName, "content", capture.TruncateStr(content, 40))
	// 同步账户池：码内容变了 → 更新绑定账户的 QRContent；无主码 → 自动建户（与监听端上报路径同逻辑）
	if _, created, aerr := h.ensureAccountForQR(c.Request.Context(), dev, "wechat", content, userId); aerr != nil {
		logger.Error("admin", "同步账户池失败（码已入库但账户未更新）", "device_id", dev.DeviceID, "user", userId, "err", aerr)
	} else if created {
		logger.Info("admin", "监听端上报已自动创建账户", "device_id", dev.DeviceID, "platform", "wechat")
	}
	// 每次捕获后顺带清理该空间相册旧码（保留最近 5 张），防频繁捕获打满存储
	if n := capture.CleanupWechatGallery(ctx, h.adbMgr, serial, userId); n > 0 {
		logger.Info("admin", "已清理设备旧收款码图", "device", serial, "user", userId, "deleted", n)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "user": userId, "file": fileName, "account": accountName, "content": content}})
}

// wechatAccountName 从设备绑定账户解析微信账户名（device_accounts 附加账户优先，主账户兜底）。
// 识别依据是账户 channel=personal.wechat，不依赖相册路径。
func (h *PersonalHandler) wechatAccountName(ctx context.Context, dev *personal.DeviceEncrypted) string {
	if dev == nil {
		return ""
	}
	if h.pool == nil {
		return ""
	}
	if ids, err := h.pdb.ListDeviceAccounts(ctx, dev.ID); err == nil {
		for _, id := range ids {
			if acct := h.pool.GetByID(id); acct != nil && strings.HasPrefix(acct.Channel, "personal.wechat") {
				return acct.Name
			}
		}
	}
	if acct := h.pool.GetByID(dev.AccountID); acct != nil && strings.HasPrefix(acct.Channel, "personal.wechat") {
		return acct.Name
	}
	return dev.Name
}
