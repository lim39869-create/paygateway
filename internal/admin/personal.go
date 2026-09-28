package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"paygateway/internal/account"
	"paygateway/internal/adbmgr"
	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// PersonalHandler 个人免签通道管理：监听设备 + 推送流水 + 无订单收款
type PersonalHandler struct {
	pdb  store.PersonalStore
	pool *account.AccountPool
	// accountDB 账户存储（用于自动创建个人账户）
	accountDB *store.AccountDB
	// adb 设备操作管理器（web 可视化操作 USB 真机 / adb over WiFi 设备；nil = 未启用）
	adbMgr *adbmgr.Manager
	// restartLimiter 设备重启 5min/次的限频器（同时被 HTTP handler 和 watcher 自动自愈复用）
	restartLimiter *restartLimiter
	// renewLimiter 收款码自动补捕限频器（每设备每平台最小间隔，防重捕风暴）
	renewLimiter *qrcodeLastRenew
	// ledgerWarmup 设备手动预热（main.go 注入；nil = 未启用）。DeviceActor 模型中
	// 手动预热是队列中的一个命令，与自动预热/轮询天然互斥，无需窗口独占。
	ledgerWarmup func(ctx context.Context, deviceID string, providers map[string]bool) error
}

// SetLedgerWarmup 注入设备手动预热函数（main 设置，warmup 走 DeviceActor 命令队列）
func (h *PersonalHandler) SetLedgerWarmup(fn func(ctx context.Context, deviceID string, providers map[string]bool) error) {
	h.ledgerWarmup = fn
}

// NewPersonalHandler 创建个人通道管理处理器
// adbMgr: adb 操作管理器（可选，nil = 设备控制台不可用）
func NewPersonalHandler(pdb store.PersonalStore, pool *account.AccountPool, accountDB *store.AccountDB, adbMgr *adbmgr.Manager) *PersonalHandler {
	return &PersonalHandler{pdb: pdb, pool: pool, accountDB: accountDB, adbMgr: adbMgr, restartLimiter: newRestartLimiter(), renewLimiter: &qrcodeLastRenew{lasts: make(map[string]time.Time)}}
}

// RegisterRoutes 注册路由（需在 authMiddleware 组内）
func (h *PersonalHandler) RegisterRoutes(rg *gin.RouterGroup) {
	// 监听设备（新设备统一走 USB 引导向导的 wizard-start，手填表单已移除）
	rg.GET("/personal/devices", h.handleListDevices)
	rg.DELETE("/personal/devices/:id", h.handleDeleteDevice)
	rg.POST("/personal/devices/:id/freeze", h.handleFreezeDevice)
	rg.POST("/personal/devices/:id/unfreeze", h.handleUnfreezeDevice)
	rg.POST("/personal/devices/:id/restart", h.handleRestartDevice)
	// 设备控制台（web 可视化操作 USB 真机 / adb over WiFi 设备）
	rg.GET("/personal/devices/:id/screenshot", h.handleDeviceScreenshot)
	rg.POST("/personal/devices/:id/action", h.handleDeviceAction)
	rg.POST("/personal/devices/:id/adb", h.handleSetDeviceAdb)
	// 设备 ↔ 账户 多对多绑定（应用双开多账户）
	rg.GET("/personal/devices/:id/accounts", h.handleListDeviceAccounts)
	rg.POST("/personal/devices/:id/accounts", h.handleAddDeviceAccount)
	rg.DELETE("/personal/devices/:id/accounts/:accountId", h.handleRemoveDeviceAccount)
	rg.POST("/personal/devices/batch/restart", h.handleBatchRestartDevices)
	rg.POST("/personal/devices/batch/status", h.handleBatchSetDeviceStatus)
	// 引导向导（USB 真机集成自动化）
	rg.GET("/personal/usb-detect", h.handleUSBCheck)
	rg.POST("/personal/devices/wizard-start", h.handleWizardStart)
	rg.POST("/personal/devices/:id/wizard-wifi", h.handleWizardWifiBackup)
	rg.POST("/personal/devices/:id/wizard-verify", h.handleWizardVerify)
	rg.POST("/personal/devices/:id/wifi-reconnect", h.handleWifiReconnect)
	// 推送流水（审计）
	rg.GET("/personal/pushes", h.handleListPushes)
	// 无订单收款（人工处理）
	rg.GET("/personal/unmatched", h.handleListUnmatched)
	rg.POST("/personal/unmatched/:id/resolve", h.handleResolveUnmatched)
	// 动态收款码管理
	rg.GET("/personal/qrcodes", h.handleListQrcodes)
	rg.DELETE("/personal/qrcodes/:deviceId/:platform/:userId", h.handleDeleteQrcode)
	rg.POST("/personal/qrcodes/:deviceId/wechat/capture", h.handleCaptureWechatQrcode)
	rg.POST("/personal/qrcodes/:deviceId/alipay/capture", h.handleCaptureAlipayQrcode)
	// 导航到收款记录页（监听界面，PLAN-EVENT-MONITOR §2.3）
	rg.POST("/personal/devices/:id/navigate-ledger", h.handleNavigateAlipayLedger)
}

// ========== 监听设备 ==========

// handleListDevices 设备列表（解密密钥不返回，仅展示状态）
func (h *PersonalHandler) handleListDevices(c *gin.Context) {
	devs, err := h.pdb.ListDevices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(devs))
	for _, d := range devs {
		out = append(out, gin.H{
			"id": d.ID, "device_id": d.DeviceID, "name": d.Name,
			"account_id": d.AccountID, "provider": d.Provider,
			"status":    d.Status,
			"last_push": d.LastPush, "ip": d.IP, "created_at": d.CreatedAt,
			"adb_host": d.AdbHost, "adb_port": d.AdbPort,
			"adb_wifi_host": d.AdbWifiHost, "adb_wifi_port": d.AdbWifiPort,
			"setup_progress": personal.ParseSetupProgress(d.SetupProgress),
			// P2P 控制面字段（docs/P2P-ADB-RECOVERY.md Phase 1）
			"ts_ip":         d.TsIP,
			"adb_listen":    d.AdbListen,
			"control_state": d.ControlState,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"devices": out}})
}

// accountHasConnectedDevice 判断指定账户是否已有 **adb 可达**的设备。
// 用于完成度门槛守恒：新建/删除设备时若账户没有 adb 在线设备则冻结账户，避免账户裸奔接单。
// 在线判定 = 实时 adb 连通性（2026-08-29 重构：不再依赖监听 App 心跳/setup 进度）。
func (h *PersonalHandler) accountHasConnectedDevice(ctx context.Context, accountID int64) bool {
	devs, err := h.pdb.ListDevices(ctx)
	if err != nil {
		return false
	}
	for _, d := range devs {
		if d.AccountID != accountID {
			continue
		}
		serial := adbSerialOf(d)
		if serial != "" && h.adbMgr != nil && h.adbMgr.Online(ctx, serial) {
			return true
		}
	}
	return false
}

// adbSerialOf 解析设备 adb serial（TCP host:port 或 USB 纯 serial）
func adbSerialOf(d *personal.DeviceEncrypted) string {
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

// handleSetDeviceAdb 设置设备 adb 连接信息（USB serial/host:port + WiFi 备用通道）
// POST /api/admin/personal/devices/:id/adb  {adb_host, adb_port, adb_wifi_host?, adb_wifi_port?}
// adb_host 可为 USB 直连 serial（无端口，此时 adb_port=0）或 host:port
func (h *PersonalHandler) handleSetDeviceAdb(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	var req struct {
		AdbHost     string `json:"adb_host"`
		AdbPort     int    `json:"adb_port"`
		AdbWifiHost string `json:"adb_wifi_host"`
		AdbWifiPort int    `json:"adb_wifi_port"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AdbHost == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误：需要 adb_host（USB serial 或 host）"})
		return
	}
	if req.AdbPort > 65535 || req.AdbWifiPort > 65535 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "端口超出范围"})
		return
	}
	if err := h.pdb.SetDeviceAdb(c.Request.Context(), id, req.AdbHost, req.AdbPort, req.AdbWifiHost, req.AdbWifiPort); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Info("admin", "设备 adb 信息已更新", "device_id", id, "adb", req.AdbHost,
		"wifi", fmt.Sprintf("%s:%d", req.AdbWifiHost, req.AdbWifiPort))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// handleListDeviceAccounts 列出设备全部绑定账户（主账户 + 附加账户）。
// GET /api/admin/personal/devices/:id/accounts
// 响应：{"data":{"primary_account_id":3,"accounts":[{"account_id":3,...},{"account_id":4,...}]}}
func (h *PersonalHandler) handleListDeviceAccounts(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	// 主账户 + 附加账户合并去重，附带账户信息（名称/通道/状态）
	ids := store.DeviceAccountIDs(c.Request.Context(), h.pdb, id, dev.AccountID)
	out := make([]gin.H, 0, len(ids))
	for _, aid := range ids {
		acct := h.pool.GetByID(aid)
		if acct == nil {
			continue
		}
		out = append(out, gin.H{
			"account_id": acct.ID, "name": acct.Name,
			"channel": acct.Channel, "status": string(acct.Status),
			"is_primary": aid == dev.AccountID,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"primary_account_id": dev.AccountID, "accounts": out}})
}

// handleAddDeviceAccount 绑定附加账户到设备（应用双开多账户）。
// POST /api/admin/personal/devices/:id/accounts  {account_id}
// 校验：账户存在 + 属于个人通道 + 与设备主账户同通道（双开只区分空间，不混通道）。
func (h *PersonalHandler) handleAddDeviceAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	var req struct {
		AccountID int64 `json:"account_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AccountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误：需要 account_id"})
		return
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	acct := h.pool.GetByID(req.AccountID)
	if acct == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户不存在"})
		return
	}
	if !isPersonalChannelName(acct.Channel) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户不是个人通道账户（channel=" + acct.Channel + "）"})
		return
	}
	if acct.QRContent == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "账户未配置收款码（qr_content），请先上传收款码"})
		return
	}
	// 允许跨通道绑定：一台设备（双开微信 + 支付宝）可服务多通道账户，
	// 监听端按包名识别通道（com.tencent.mm / com.eg.android.AlipayGphone），单实例即可接收全部通知。
	if err := h.pdb.AddDeviceAccount(c.Request.Context(), id, req.AccountID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "绑定账户失败: " + err.Error()})
		return
	}
	logger.Info("admin", "设备绑定附加账户", "device_id", id, "account_id", req.AccountID, "name", acct.Name)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "account_id": req.AccountID}})
}

// handleRemoveDeviceAccount 解绑设备附加账户（主账户不可解绑，需删除设备或改用主账户字段）。
// DELETE /api/admin/personal/devices/:id/accounts/:accountId
func (h *PersonalHandler) handleRemoveDeviceAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	accountID, _ := strconv.ParseInt(c.Param("accountId"), 10, 64)
	if id <= 0 || accountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	if accountID == dev.AccountID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "主账户不可解绑（请先删除设备或更换主账户）"})
		return
	}
	if err := h.pdb.RemoveDeviceAccount(c.Request.Context(), id, accountID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "解绑账户失败: " + err.Error()})
		return
	}
	logger.Info("admin", "设备解绑附加账户", "device_id", id, "account_id", accountID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

func (h *PersonalHandler) handleDeleteDevice(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	// 删前查 account_id 用于守恒门槛：删完后若账户再无在线完成 setup 的设备则冻结账户
	dev, _ := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err := h.pdb.DeleteDevice(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 级联清理：收款码记录、账户绑定；仅被该设备绑定的自动账户一并停用，
	// 否则幽灵设备的码卡与僵尸账户会永远留在池子里
	if dev != nil {
		if err := h.pdb.DeleteQrcodeByDevice(c.Request.Context(), dev.DeviceID); err != nil {
			logger.Warn("admin", "清理设备收款码失败", "device_id", dev.DeviceID, "err", err)
		}
		h.unbindAndRetireAccounts(c.Request.Context(), id, dev)
	}
	if dev != nil && dev.AccountID > 0 && !h.accountHasConnectedDevice(c.Request.Context(), dev.AccountID) {
		if a := h.pool.GetByID(dev.AccountID); a != nil && a.Status == account.StatusNormal {
			// 无在线设备 → 按离线联动语义冻结：新设备 wizard 接入 adb 可达时自动解冻
			h.pool.FreezeByOffline(dev.AccountID)
			logger.Info("admin", "账户已冻结（删除最后一台在线设备）", "account_id", dev.AccountID, "name", a.Name)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// handleFreezeDevice 冻结设备（不可接新单，心跳返回 disable 指令）
func (h *PersonalHandler) handleFreezeDevice(c *gin.Context) {
	h.setDeviceStatus(c, string(personal.DeviceFrozen))
}

// unbindAndRetireAccounts 设备删除后的账户收尾：解绑全部绑定账户；
// 其中"自动创建"且不再属于任何设备的个人账户 → 停用（保留记录可查，管理员可在账户池彻底删除）
func (h *PersonalHandler) unbindAndRetireAccounts(ctx context.Context, devRowID int64, dev *personal.DeviceEncrypted) {
	for _, acctID := range store.DeviceAccountIDs(ctx, h.pdb, devRowID, 0) {
		if err := h.pdb.RemoveDeviceAccount(ctx, devRowID, acctID); err != nil {
			logger.Warn("admin", "解除设备账户绑定失败", "device_row_id", devRowID, "account_id", acctID, "err", err)
			continue
		}
		acct := h.pool.GetByID(acctID)
		if acct == nil || !strings.HasSuffix(acct.Name, "自动") || !strings.HasPrefix(acct.Channel, "personal.") {
			continue
		}
		if bound, err := h.pdb.AccountBound(ctx, acctID); err == nil && bound {
			continue // 仍被其他设备绑定
		}
		h.pool.FreezeByRetire(acctID)
		if err := h.accountDB.SetAccountFrozen(ctx, acctID, account.FrozenReasonRetire); err != nil {
			logger.Warn("admin", "停用僵尸自动账户失败", "account_id", acctID, "err", err)
			continue
		}
		logger.Info("admin", "已停用无主自动账户（设备删除级联）", "account_id", acctID, "name", acct.Name)
	}
}

// handleUnfreezeDevice 解冻设备
func (h *PersonalHandler) handleUnfreezeDevice(c *gin.Context) {
	h.setDeviceStatus(c, string(personal.DeviceOffline))
}

func (h *PersonalHandler) setDeviceStatus(c *gin.Context, status string) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if err := h.pdb.SetDeviceStatus(c.Request.Context(), id, status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// ========== 推送流水 ==========

// handleListPushes 收款推送流水（审计）
func (h *PersonalHandler) handleListPushes(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	pushes, err := h.pdb.ListPushes(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"pushes": pushes}})
}

// ========== 无订单收款 ==========

// handleListUnmatched 无订单收款列表（pending / resolved / refunded）
func (h *PersonalHandler) handleListUnmatched(c *gin.Context) {
	status := c.Query("status")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	list, err := h.pdb.ListUnmatched(c.Request.Context(), status, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"unmatched": list}})
}

// handleResolveUnmatched 处理无订单收款
// POST /api/admin/personal/unmatched/:id/resolve  {status: resolved|refunded, note}
func (h *PersonalHandler) handleResolveUnmatched(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if req.Status != "resolved" && req.Status != "refunded" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status 必须为 resolved 或 refunded"})
		return
	}
	if err := h.pdb.ResolveUnmatched(c.Request.Context(), id, req.Status, req.Note); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// ---------- 辅助 ----------

// isPersonalChannelName 判断账户 channel 是否个人通道（personal.alipay / personal.wechat ...）
func isPersonalChannelName(ch string) bool {
	return len(ch) > 9 && ch[:9] == "personal."
}

// providerFromChannelName 从账户 channel 提取 provider（personal.alipay → alipay）
func providerFromChannelName(ch string) string {
	for i := len(ch) - 1; i >= 0; i-- {
		if ch[i] == '.' {
			return ch[i+1:]
		}
	}
	return ch
}

// ---------- 批量运维 ----------

// batchDeviceReq 批量操作的通用请求体。ids 为空时若 all=true 则作用于全部设备；ids 非空时以 ids 为准。
type batchDeviceReq struct {
	IDs []int64 `json:"ids"`
	All bool    `json:"all"`
}

func (h *PersonalHandler) resolveBatchTargets(c *gin.Context) ([]*personal.DeviceEncrypted, []int64, bool) {
	var req batchDeviceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return nil, nil, false
	}
	ctx := c.Request.Context()
	if len(req.IDs) == 0 {
		if !req.All {
			c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误：需提供 ids 或 all=true"})
			return nil, nil, false
		}
		devs, err := h.pdb.ListDevices(ctx)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return nil, nil, false
		}
		enc := make([]*personal.DeviceEncrypted, len(devs))
		for i, d := range devs {
			enc[i] = d
		}
		return enc, nil, true
	}
	seen := make(map[int64]bool)
	missing := make([]int64, 0)
	out := make([]*personal.DeviceEncrypted, 0, len(req.IDs))
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		dev, err := h.pdb.GetDeviceByID(ctx, id)
		if err != nil || dev == nil {
			missing = append(missing, id)
			continue
		}
		out = append(out, dev)
	}
	return out, missing, true
}

func (h *PersonalHandler) batchDeviceError(c *gin.Context, device *personal.DeviceEncrypted, err error) {
	logger.Warn("admin", "批量设备操作失败", "device_id", device.ID, "device", device.DeviceID, "err", err)
}

// POST /api/admin/personal/devices/batch/restart  {ids:[...]|all:true}
// 逐个调 h.Restart（内部自带 5min 限频 + 90s 超时），不因单个失败中断其余设备。
func (h *PersonalHandler) handleBatchRestartDevices(c *gin.Context) {
	devs, missing, ok := h.resolveBatchTargets(c)
	if !ok {
		return
	}
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "adb 管理器未配置，批量重启不可用"})
		return
	}
	results := make([]map[string]any, 0, len(devs))
	hasFail := false
	for _, dev := range devs {
		res, rerr := h.Restart(c.Request.Context(), dev)
		if rerr != nil {
			h.batchDeviceError(c, dev, rerr)
			results = append(results, map[string]any{"device_id": dev.DeviceID, "id": dev.ID, "ok": false, "error": rerr.Error()})
			hasFail = true
			continue
		}
		res["device_id"] = dev.DeviceID
		res["id"] = dev.ID
		results = append(results, res)
	}
	code := http.StatusOK
	if hasFail {
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{
		"data": gin.H{"results": results, "missing_ids": missing, "total": len(devs)},
	})
}

// POST /api/admin/personal/devices/batch/status  {ids:[...]|all:true, status:"frozen"|"offline"}
func (h *PersonalHandler) handleBatchSetDeviceStatus(c *gin.Context) {
	var req struct {
		batchDeviceReq
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	switch req.Status {
	case string(personal.DeviceFrozen), string(personal.DeviceOffline):
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status 必须为 frozen 或 offline"})
		return
	}
	devs, missing, ok := h.resolveBatchTargets(c)
	if !ok {
		return
	}
	results := make([]map[string]any, 0, len(devs))
	hasFail := false
	for _, dev := range devs {
		entry := map[string]any{"device_id": dev.DeviceID, "id": dev.ID}
		if err := h.pdb.SetDeviceStatus(c.Request.Context(), dev.ID, req.Status); err != nil {
			h.batchDeviceError(c, dev, err)
			entry["ok"] = false
			entry["error"] = err.Error()
			hasFail = true
		} else {
			entry["ok"] = true
		}
		results = append(results, entry)
	}
	code := http.StatusOK
	if hasFail {
		code = http.StatusMultiStatus
	}
	c.JSON(code, gin.H{"data": gin.H{"results": results, "missing_ids": missing, "total": len(devs)}})
}

// ========== 动态收款码管理 ==========

// handleListQrcodes 获取所有设备最新收款码
func (h *PersonalHandler) handleListQrcodes(c *gin.Context) {
	qs, err := h.pdb.ListQrcodes(c.Request.Context(), 100)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": qs})
}

// handleDeleteQrcode 移除单条收款码记录（设备退役/换绑后手动清理用）
func (h *PersonalHandler) handleDeleteQrcode(c *gin.Context) {
	deviceID := c.Param("deviceId")
	platform := c.Param("platform")
	userID, _ := strconv.Atoi(c.Param("userId"))
	if deviceID == "" || (platform != "alipay" && platform != "wechat") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if err := h.pdb.DeleteQrcode(c.Request.Context(), deviceID, platform, userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Info("admin", "已移除收款码记录", "device_id", deviceID, "platform", platform, "user_id", userID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ensureAccountForQR 确保捕获到的收款码有对应的 pay_accounts 记录。
// 如果账户池中无匹配该码内容的个人账户，自动创建并绑定到设备，使支付通道可用。
// space: 0=主空间, 999=双开(XSpace)。返回 account_id、是否新建、error。
func (h *PersonalHandler) ensureAccountForQR(ctx context.Context, dev *personal.DeviceEncrypted, platform, qrContent string, space int) (int64, bool, error) {
	if h.accountDB == nil || h.pool == nil || qrContent == "" {
		return 0, false, nil
	}
	return EnsureAccountForQR(ctx, h.pdb, h.pool, h.accountDB, dev, platform, qrContent, space)
}

// EnsureAccountForQR 全局可复用版（admin handler 与 main.go 监听端上报共用）。
// 依据：收款码内容匹配账户池 → 无则自动创建 personal.* 账户并绑定设备。
// space: 0=主空间, 999=双开(XSpace)。同一设备+平台+空间 只保留一个账户：
// 复用已绑定账户（主空间/双开按账户名后缀区分），动态码每次上报不新建重复账户。
func EnsureAccountForQR(ctx context.Context, pdb store.PersonalStore, pool *account.AccountPool, accountDB *store.AccountDB, dev *personal.DeviceEncrypted, platform, qrContent string, space int) (int64, bool, error) {
	if accountDB == nil || pool == nil || qrContent == "" || dev == nil {
		return 0, false, nil
	}
	prefix := "personal.alipay"
	if platform == "wechat" {
		prefix = "personal.wechat"
	}
	// 已有匹配账户 → 复用（同一设备+平台+空间，不管码内容是否变化）
	// 匹配顺序：① 收款码内容完全一致（最可靠）→ ② 空间标记（账户名含"双开"）。
	// 创建时必须写入空间标记，否则下次上报匹配不到 → 重复建户（历史 bug 的根源）。
	var reuse *account.Account
	for _, id := range store.DeviceAccountIDs(ctx, pdb, dev.ID, dev.AccountID) {
		acct := pool.GetByID(id)
		if acct == nil || !strings.HasPrefix(acct.Channel, prefix) {
			continue
		}
		if acct.QRContent == qrContent {
			reuse = acct
			break
		}
	}
	if reuse == nil {
		for _, id := range store.DeviceAccountIDs(ctx, pdb, dev.ID, dev.AccountID) {
			acct := pool.GetByID(id)
			if acct == nil || !strings.HasPrefix(acct.Channel, prefix) {
				continue
			}
			if space == 0 && !strings.Contains(acct.Name, "双开") {
				reuse = acct
				break
			}
			if space == 999 && strings.Contains(acct.Name, "双开") {
				reuse = acct
				break
			}
		}
	}
	if reuse != nil {
		// 收款码内容变化 → 更新到账户（保持单一账户跟随最新码）
		if reuse.QRContent != qrContent {
			// 资金安全防线：新码必须合法（与下单路由同规则），非法内容绝不覆盖账户
			// （脏数据一旦写入，后续下单全部路由到坏码 → 支付全挂，历史教训 2026-08-29）
			if reason := account.InvalidQRContent(reuse.Channel, qrContent); reason != "" {
				logger.Error("admin", "收款码内容非法，拒绝更新账户（脏数据防线）",
					"device", dev.DeviceID, "platform", platform, "account_id", reuse.ID,
					"reason", reason, "content", capture.TruncateStr(qrContent, 30))
				return reuse.ID, false, nil
			}
			qrEnc, err := account.Encrypt([]byte(qrContent))
			if err != nil {
				return 0, false, fmt.Errorf("加密收款码内容失败: %w", err)
			}
			if err := accountDB.UpdateAccount(ctx, &account.AccountEncrypted{ID: reuse.ID, Channel: reuse.Channel, Name: reuse.Name,
				AppID: reuse.AppID, MchID: reuse.MchID, QRContentEnc: qrEnc, Status: string(reuse.Status),
				Weight: reuse.Weight, Priority: reuse.Priority, AutoBan: reuse.AutoBan, DailyLimit: reuse.DailyLimit,
				DailyUsed: reuse.DailyUsed, Group: reuse.Group, IsProd: reuse.IsProd, Remark: reuse.Remark}); err != nil {
				return 0, false, fmt.Errorf("更新账户收款码失败: %w", err)
			}
			reuse.QRContent = qrContent
			// GetByID 返回副本，必须写回池；否则池内旧码一直残留（重启前匹配全用旧码）
			pool.Update(reuse)
			logger.Info("admin", "账户收款码已更新", "device", dev.DeviceID, "platform", platform, "account_id", reuse.ID)
		}
		return reuse.ID, false, nil
	}
	// 加密收款码内容
	qrContentEnc, err := account.Encrypt([]byte(qrContent))
	if err != nil {
		return 0, false, fmt.Errorf("加密收款码内容失败: %w", err)
	}
	// 生成账户名：设备名 + 平台 + 空间标记（主空间/双开都标注，管理后台一眼可区分）
	devName := dev.Name
	if devName == "" {
		// 设备未命名时用设备 ID 前 8 位兜底，避免多设备混淆
		devName = dev.DeviceID
		if len(devName) > 8 {
			devName = devName[:8]
		}
	}
	acctName := devName
	if platform == "wechat" {
		acctName += " 微信自动"
	} else {
		acctName += " 支付宝自动"
	}
	if space == 999 {
		acctName += "(双开)"
	} else {
		acctName += "(主空间)"
	}
	// 资金安全防线：新建账户的码也必须合法（与更新路径同规则），脏数据不建账户
	if reason := account.InvalidQRContent(prefix, qrContent); reason != "" {
		logger.Error("admin", "收款码内容非法，拒绝创建账户（脏数据防线）",
			"device", dev.DeviceID, "platform", platform, "user", space, "reason", reason,
			"content", capture.TruncateStr(qrContent, 30))
		return 0, false, nil
	}
	// 创建账户记录
	newID, err := accountDB.CreateAccount(ctx, &account.AccountEncrypted{
		Channel:      prefix,
		Name:         acctName,
		QRContentEnc: qrContentEnc,
		Status:       "normal",
		IsProd:       true,
	})
	if err != nil {
		return 0, false, fmt.Errorf("创建个人账户失败: %w", err)
	}
	// 添加账户到内存池（使支付通道立即可见）
	pool.Add(&account.Account{
		ID:        newID,
		Channel:   prefix,
		Name:      acctName,
		QRContent: qrContent,
		Status:    account.StatusNormal,
		IsProd:    true,
	})
	// 绑定到设备
	if err := pdb.AddDeviceAccount(ctx, dev.ID, newID); err != nil {
		return newID, true, fmt.Errorf("绑定账户到设备失败: %w", err)
	}
	logger.Info("admin", "已自动创建个人账户", "device", dev.DeviceID, "platform", platform,
		"account_id", newID, "name", acctName)
	return newID, true, nil
}
