// 引导向导（USB 真机集成自动化）
//
// 目标：把「插 USB → 开 USB 调试 → 绑设备」全流程从手动操作变成 web 前端逐步引导。
// 2026-08-30 重构：监听端 APP 弃用，向导收敛为「绑定设备即完成」；
// 设备上线/账户解冻由 DeviceWatcher 按 adb 可达性驱动（不再依赖心跳/完成度门槛）。
//
// 三步接口（全部在 authMiddleware 组内）：
//   - GET  /personal/usb-detect             前端轮询 USB 设备插入状态
//   - POST /personal/devices/wizard-start   绑定设备（幂等，断点续传）
//   - POST /personal/devices/:id/wizard-wifi 配置 WiFi 备用通道（USB 断线自动切换）
//   - POST /personal/devices/:id/wizard-verify 验证 adb 连通 + 返回保活引导（wizard 收尾）
//
// 完成度推进：
//
//	wizard-start → setup_progress=["usb_connected","bound"]
package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"paygateway/internal/account"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
)

// handleUSBCheck 列出当前已连 USB 真机中「未绑定本平台」的设备。
// GET /api/admin/personal/usb-detect
//
// 前端 2-3s 轮询，识别新插入的设备（state 从 unauthorized → device 表示手机端已授权）。
// 响应：{"data":{"devices":[{"serial":"4f2bdc30","state":"device","model":"MI 8","bound":false},...]}}
func (h *PersonalHandler) handleUSBCheck(c *gin.Context) {
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "adb 管理器未配置，USB 检测不可用"})
		return
	}
	devs, err := h.adbMgr.ListDevices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备检测失败: " + err.Error()})
		return
	}
	// 已绑定 serial 集合（匹配 adb_host，USB 纯 serial 与 WiFi host:port 皆算）
	bound := h.boundSerials(c.Request.Context())
	out := make([]gin.H, 0, len(devs))
	for _, d := range devs {
		_, isBound := bound[d.Serial]
		out = append(out, gin.H{
			"serial": d.Serial, "state": d.State, "model": d.Model, "kind": d.Kind, "bound": isBound,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"devices": out}})
}

// boundSerials 返回当前已绑定设备的 adb serial 集合（累加 USB 主通道 + WiFi 备用通道）
func (h *PersonalHandler) boundSerials(ctx context.Context) map[string]int64 {
	out := make(map[string]int64)
	devs, err := h.pdb.ListDevices(ctx)
	if err != nil {
		return out
	}
	for _, d := range devs {
		if d.AdbHost != "" {
			out[d.AdbHost] = d.ID
		}
		if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
			out[fmt.Sprintf("%s:%d", d.AdbWifiHost, d.AdbWifiPort)] = d.ID
		}
	}
	return out
}

func containsColon(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}

// wizardStartReq wizard-start 请求体
type wizardStartReq struct {
	Serial    string `json:"serial"`     // USB serial（必填）
	Name      string `json:"name"`       // 设备备注名（必填）
	AccountID int64  `json:"account_id"` // 收款账户 ID（必填）
}

// handleWizardStart 引导第 3 步：绑定设备（创建设备记录 + 写 setup 进度）。
// POST /api/admin/personal/devices/wizard-start
//
// 幂等 + 断点续传：
//   - serial 未绑定 → 创建设备，setup_progress=["usb_connected","bound"]
//   - serial 已绑定 → 返回已有设备 + 当前进度（前端从断点继续，不重复创建）
//
// 响应：{"data":{"ok":true,"id":7,"device_id":"...","setup_progress":["usb_connected","bound"]}}
func (h *PersonalHandler) handleWizardStart(c *gin.Context) {
	var req wizardStartReq
	if err := c.ShouldBindJSON(&req); err != nil || req.Serial == "" || req.Name == "" || req.AccountID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误：需要 serial + name + account_id"})
		return
	}
	// serial 可为 USB 直连设备号（不含 ":"）或 WiFi host:port（含 ":"，须已 adb connect 过）
	if req.Serial == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误：需要 serial"})
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
	// 幂等：serial 已绑定 → 断点续传
	devs, err := h.pdb.ListDevices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	for _, d := range devs {
		if d.AdbHost == req.Serial {
			steps := personal.ParseSetupProgress(d.SetupProgress)
			// 已绑定即视为已接入（上线判定由 DeviceWatcher 按 adb 可达性驱动）
			if personal.HasSetupStep(steps, personal.SetupStepBound) {
				c.JSON(http.StatusOK, gin.H{"data": gin.H{
					"ok": true, "id": d.ID, "device_id": d.DeviceID,
					"setup_progress": steps, "already_complete": true,
				}})
				return
			}
			c.JSON(http.StatusOK, gin.H{"data": gin.H{
				"ok": true, "id": d.ID, "device_id": d.DeviceID,
				"setup_progress": steps, "resumed": true,
			}})
			return
		}
	}
	// 新建：生成设备密钥 + 加密 + 写入（绑定逻辑内联，密钥仅此处返回一次）
	secret, err := personal.NewDeviceSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成设备密钥失败: " + err.Error()})
		return
	}
	secretEnc, err := account.Encrypt([]byte(secret))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "加密设备密钥失败: " + err.Error()})
		return
	}
	provider := providerFromChannelName(acct.Channel)
	deviceID := personal.NewDeviceID()
	steps := []string{personal.SetupStepUSBConnected, personal.SetupStepBound}
	d := &personal.DeviceEncrypted{
		DeviceID: deviceID, Name: req.Name, AccountID: acct.ID,
		Provider: provider, SecretEnc: secretEnc, Status: string(personal.DeviceOffline),
		AdbHost: req.Serial,
		// 完成度门槛：新建即标记 usb_connected + bound，账户后续由心跳/删除 handler 守恒冻结
		SetupProgress: personal.FormatSetupProgress(steps),
	}
	id, err := h.pdb.CreateDevice(c.Request.Context(), d)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建设备失败: " + err.Error()})
		return
	}
	// 账户冻结守恒：新建半配置设备 → 该账户若无其他在线设备则冻结
	// （设备 adb 可达后由 DeviceWatcher 自动解冻）
	if !h.accountHasConnectedDevice(c.Request.Context(), acct.ID) {
		if a := h.pool.GetByID(acct.ID); a != nil && a.Status == account.StatusNormal {
			h.pool.FreezeByOffline(acct.ID)
			logger.Info("admin", "账户已冻结（wizard 新建设备未完成 setup）", "account_id", acct.ID, "name", a.Name)
		}
	}
	logger.Info("admin", "向导设备已绑定", "device_id", deviceID, "name", req.Name, "serial", req.Serial, "account", acct.ID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ok": true, "id": id, "device_id": deviceID,
		"setup_progress": steps,
	}})
}

// handleWizardWifiBackup 自动配置 WiFi 备用通道（引导向导第 4 步前）。
// POST /api/admin/personal/devices/:id/wizard-wifi
//
// 全自动，无需用户填写任何 WiFi 信息：
//  1. 设备必须 USB 直连（AdbHost 为纯 serial）——WiFi 备用通道的开启依赖 USB 先授权
//  2. adb tcpip 5555：开启设备无线 adb（进入 TCP/IP 监听模式）
//  3. 自动获取设备 WiFi 局域网 IP（wlan0/eth0/getprop 多路尝试）
//  4. 组合 <ip>:5555 写入 adb_wifi_host/adb_wifi_port（设备 USB 断开后自动切换保活）
//
// 幂等：备用通道已配置且设备已在 TCP 监听 → 直接返回现有配置不重复执行。
// 失败不阻塞主流程（返回 ok=false + warning），USB 直连本身仍可用。
func (h *PersonalHandler) handleWizardWifiBackup(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "adb 管理器未配置"})
		return
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	// 仅 USB 直连设备可自动配置（依赖 USB 通道的 adb 权限）
	if dev.AdbHost == "" || containsColon(dev.AdbHost) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅 USB 直连设备可自动配置 WiFi 备用通道"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	serial := dev.AdbHost
	// 幂等：已有备用通道且设备在线（TCP 监听已开）→ 直接返回
	if dev.AdbWifiHost != "" && dev.AdbWifiPort > 0 {
		wifiSerial := fmt.Sprintf("%s:%d", dev.AdbWifiHost, dev.AdbWifiPort)
		if h.adbMgr.Online(ctx, wifiSerial) {
			logger.Info("admin", "WiFi 备用通道已生效，跳过配置", "device", dev.DeviceID, "wifi", wifiSerial)
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "wifi_host": dev.AdbWifiHost, "wifi_port": dev.AdbWifiPort}})
			return
		}
	}
	// 1. 开启无线 adb
	if err := h.adbMgr.TCPIP(ctx, serial, 5555); err != nil {
		logger.Warn("admin", "开启无线 adb 失败", "device", dev.DeviceID, "err", err)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "warning": "开启无线 adb 失败：" + err.Error()}})
		return
	}
	// adb tcpip 会重启设备端 adbd（切到 TCP 监听模式），USB adb 通道短暂中断。
	// 立即查 IP 会失败（adbd 未就绪），等待 2s 让其恢复后再获取 IP。
	time.Sleep(2 * time.Second)
	// 2. 获取 WiFi IP（重试 2 次，覆盖 adbd 重启窗口）
	var ip string
	for attempt := 0; attempt < 3; attempt++ {
		ip = h.adbMgr.WifiIP(ctx, serial)
		if ip != "" {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Second)
		}
	}
	if ip == "" {
		logger.Warn("admin", "获取设备 WiFi IP 失败（设备可能未连 WiFi）", "device", dev.DeviceID)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "warning": "未获取到设备 WiFi IP（请确认手机已连接 WiFi）"}})
		return
	}
	// 3. 写入备用通道
	wifiPort := 5555
	if err := h.pdb.SetDeviceAdb(c.Request.Context(), id, dev.AdbHost, dev.AdbPort, ip, wifiPort); err != nil {
		logger.Warn("admin", "写入 WiFi 备用通道失败", "err", err, "id", id)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "写入 WiFi 备用通道失败: " + err.Error()})
		return
	}
	logger.Info("admin", "WiFi 备用通道已自动配置", "device", dev.DeviceID, "serial", serial, "wifi", fmt.Sprintf("%s:%d", ip, wifiPort))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "wifi_host": ip, "wifi_port": wifiPort}})
}

// handleWizardVerify 引导最后一步：验证 adb 连通性 + 返回保活引导提示。
// POST /api/admin/personal/devices/:id/wizard-verify
//
// 2026-08-30 重构：监听端 APP 弃用，向导不再下发配置/拉起 App；
// 设备上线由 DeviceWatcher 按 adb 可达性驱动。本步骤只做 adb 连通验证，
// 并返回 MIUI 省电策略"无限制"的保活引导（墓碑冻结无 adb 入口，需 UI 设置）。
func (h *PersonalHandler) handleWizardVerify(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "adb 管理器未配置"})
		return
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}
	serial, ok := h.pickSerialForWizard(c.Request.Context(), dev)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备离线：USB 未连接或未授权（请确认手机已开 USB 调试并允许）"})
		return
	}
	resp := gin.H{"ok": true, "serial": serial}
	// 保活引导提示（MIUI 墓碑冻结/杀后台需 UI 层"省电策略→无限制"才能豁免，
	// 无 root 无 adb 入口——实测 2026-08-30 红米k60/MIUI14）。
	// 微信/支付宝需无限制，否则后台被系统冻结会掉线。
	resp["keepalive_hint"] = fmt.Sprintf(
		"请在手机上完成保活设置（共 %d 个应用）：设置→应用设置→应用管理→%s→省电策略→选择「无限制」。"+
			"不设置的话系统可能冻结后台进程导致掉线。", keepaliveHintAppsCount(dev.Provider), keepaliveHintApps(dev.Provider))
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// keepaliveHintApps 保活引导的应用显示名（支付通道 APP，按 provider 区分）。
func keepaliveHintApps(provider string) string {
	return "「" + keepaliveChannelApp(provider) + "」"
}

// keepaliveHintAppsCount 保活引导的应用数量（当前恒为 1：支付通道 APP）。
func keepaliveHintAppsCount(provider string) int {
	return 1
}

// keepaliveChannelApp 支付通道 APP 显示名（与包名映射，用于引导文案）。
func keepaliveChannelApp(provider string) string {
	switch provider {
	case "wechat":
		return "微信"
	case "alipay":
		return "支付宝"
	default:
		return ""
	}
}

// pickSerialForWizard 取设备可用的 adb serial 并校验在线（向导专用）。
// AdbHost 可为 USB 纯 serial 或 WiFi host:port；没有或不可用时回退 WiFi 备用通道。
func (h *PersonalHandler) pickSerialForWizard(ctx context.Context, dev *personal.DeviceEncrypted) (string, bool) {
	candidates := make([]string, 0, 2)
	if dev.AdbHost != "" {
		candidates = append(candidates, dev.AdbHost)
	}
	if dev.AdbWifiHost != "" && dev.AdbWifiPort > 0 {
		candidates = append(candidates, fmt.Sprintf("%s:%d", dev.AdbWifiHost, dev.AdbWifiPort))
	}
	for _, s := range candidates {
		if h.adbMgr.Online(ctx, s) {
			return s, true
		}
	}
	return "", false
}

// handleWifiReconnect 设备断网/断 USB 后重连 WiFi 备用通道（adb connect 重建链路）。
// 心跳走公网隧道可能已恢复（status=online），但 scrcpy 画面流的 adb 连接会在断网时失效，
// 需要重新 adb connect 才能恢复实时画面。成功返回可用的 serial 供前端重连，
// USB 主通道若在线则优先返回 USB serial，否则尝试重连 WiFi。
func (h *PersonalHandler) handleWifiReconnect(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "设备 id 无效"})
		return
	}
	ctx := c.Request.Context()
	dev, err := h.pdb.GetDeviceByID(ctx, id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}

	if dev.AdbHost != "" && h.adbMgr.Online(ctx, dev.AdbHost) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"serial": dev.AdbHost, "channel": "usb"}})
		return
	}

	if dev.AdbWifiHost != "" && dev.AdbWifiPort > 0 {
		serial := fmt.Sprintf("%s:%d", dev.AdbWifiHost, dev.AdbWifiPort)
		if err := h.adbMgr.Connect(ctx, serial); err == nil && h.adbMgr.Online(ctx, serial) {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"serial": serial, "channel": "wifi"}})
			return
		}
	}

	c.JSON(http.StatusBadGateway, gin.H{"error": "设备离线：USB 未连接且 WiFi 重连失败，请确认设备在线并已开 USB 调试"})
}
