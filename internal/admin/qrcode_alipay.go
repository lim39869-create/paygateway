// 支付宝收款码捕获 HTTP 层：设备查找 → 调用 capture 包自动化 → 去重 → 入库。
package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// handleCaptureAlipayQrcode 通过服务器 adb 捕获支付宝收款码
// POST /api/personal/qrcodes/:deviceId/alipay/capture?user=0|999
func (h *PersonalHandler) handleCaptureAlipayQrcode(c *gin.Context) {
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
	userId, _ := strconv.Atoi(c.DefaultQuery("user", "0"))
	serial, _, ok := h.pickOnlineSerial(c.Request.Context(), dev)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备离线：所有 adb 通道不可用"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	content, cerr := capture.CaptureAlipayQrcode(ctx, h.adbMgr, serial, userId)
	if cerr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": cerr.Error()})
		return
	}
	// 去重：与库中最新码相同则跳过
	if latest, lerr := h.pdb.GetLatestQrcode(ctx, deviceID); lerr == nil && latest != nil &&
		latest.Platform == "alipay" && latest.QRContent == content && latest.Status == "active" {
		logger.Info("admin", "支付宝收款码未变化，跳过入库", "device_id", deviceID)
		// 码未变不代表账户同步过：仍需确保账户池 QRContent 与码一致（历史不同步的补救路径）
		if _, created, aerr := h.ensureAccountForQR(c.Request.Context(), dev, "alipay", content, userId); aerr != nil {
			logger.Error("admin", "同步账户池失败", "device_id", deviceID, "user", userId, "err", aerr)
		} else if created {
			logger.Info("admin", "已自动创建账户", "device_id", deviceID, "platform", "alipay")
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "unchanged": true, "content": content}})
		return
	}
	accountName := h.resolveQrcodeAccount(ctx, dev, "alipay", userId, content)
	q := &personal.DeviceQrcode{
		DeviceID:  dev.DeviceID,
		Platform:  "alipay",
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
	logger.Info("admin", "支付宝收款码已捕获", "device_id", dev.DeviceID, "user", userId,
		"name", accountName, "content", capture.TruncateStr(content, 40))
	// 同步账户池：码内容变了 → 更新绑定账户的 QRContent；无主码 → 自动建户（与监听端上报路径同逻辑）
	if _, created, aerr := h.ensureAccountForQR(c.Request.Context(), dev, "alipay", content, userId); aerr != nil {
		logger.Error("admin", "同步账户池失败（码已入库但账户未更新）", "device_id", dev.DeviceID, "user", userId, "err", aerr)
	} else if created {
		logger.Info("admin", "监听端上报已自动创建账户", "device_id", dev.DeviceID, "platform", "alipay")
	}
	// 捕获后顺带清理该空间支付宝相册旧码（保留最新 1 张），防频繁捕获打满存储
	if n := capture.CleanupAlipayGallery(ctx, h.adbMgr, serial, userId); n > 0 {
		logger.Info("admin", "已清理设备旧支付宝收款码图", "device", serial, "user", userId, "deleted", n)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "user": userId, "account": accountName, "content": content}})
}

// handleNavigateAlipayLedger 通过服务器 adb 导航到支付宝完整收款记录页（监听界面）。
// 供事件驱动监听（PLAN-EVENT-MONITOR §2.3）手动触发/测试；监听端收到窗口事件后建立基线进入监听态。
// POST /api/personal/devices/:id/navigate-ledger?provider=alipay&user=0|999
func (h *PersonalHandler) handleNavigateAlipayLedger(c *gin.Context) {
	deviceID := c.Param("id")
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
	userId, _ := strconv.Atoi(c.DefaultQuery("user", "0"))
	serial, _, ok := h.pickOnlineSerial(c.Request.Context(), dev)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备离线：所有 adb 通道不可用"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()

	if err := capture.CaptureAlipayLedger(ctx, h.adbMgr, serial, userId); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	logger.Info("admin", "已导航到支付宝收款记录页", "device_id", dev.DeviceID, "user", userId)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "user": userId}})
}

// alipayAccountName 从设备绑定账户解析支付宝账户名（device_accounts 附加账户优先，主账户兜底）。
func (h *PersonalHandler) alipayAccountName(ctx context.Context, dev *personal.DeviceEncrypted) string {
	if dev == nil || h.pool == nil {
		return ""
	}
	if ids, err := h.pdb.ListDeviceAccounts(ctx, dev.ID); err == nil {
		for _, id := range ids {
			if acct := h.pool.GetByID(id); acct != nil && strings.HasPrefix(acct.Channel, "personal.alipay") {
				return acct.Name
			}
		}
	}
	if acct := h.pool.GetByID(dev.AccountID); acct != nil && strings.HasPrefix(acct.Channel, "personal.alipay") {
		return acct.Name
	}
	return dev.Name
}
