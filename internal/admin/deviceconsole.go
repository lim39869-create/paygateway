package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// 设备控制台：web 前端可视化操作 adb 触达的安卓设备（USB 真机 / adb over WiFi；
// 微信 / 支付宝）
//
// 能力（按需渲染，非完整云手机）：
//   - 截图：查看设备当前屏幕（渲染微信 / 支付宝界面）
//   - 操作：点击 / 滑动 / 输入文本 / 按键（完成 app 登录等）
//   - 启动 app：拉起微信 / 支付宝
//   - 手动预热：导航到账本页并锁定任务（DeviceActor 命令，账本监听专用）
//
// 前置条件：设备已配置 adb_host / adb_wifi_host（USB 真机留空 adb_port 即视作 USB 直连）。
// 本控制台以 adb 为协议基线，USB 直连（纯 serial）与 adb over WiFi（TCP adb）均适用。

// deviceSerial 从设备记录构造 adb serial（USB 优先，WiFi 备用）
//   - adb_host 无端口 → USB 直连 serial（如 "4f2bdc30"，真机直连）
//   - adb_host 有端口 → TCP 地址（如 "192.168.1.119:5555"，adb over WiFi）
//   - adb_wifi_host 配置且 USB 通道不可用 → WiFi 备用
func (h *PersonalHandler) deviceSerial(d *personal.DeviceEncrypted) (string, error) {
	if d.AdbHost != "" {
		if d.AdbPort > 0 {
			return fmt.Sprintf("%s:%d", d.AdbHost, d.AdbPort), nil
		}
		return d.AdbHost, nil
	}
	if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
		return fmt.Sprintf("%s:%d", d.AdbWifiHost, d.AdbWifiPort), nil
	}
	return "", fmt.Errorf("设备未配置 adb 连接（adb_host / adb_wifi_host 为空）")
}

// getDeviceWithAdb 按 id 取设备并校验 adb 可用（USB 优先，失败自动切 WiFi 备用）
func (h *PersonalHandler) getDeviceWithAdb(c *gin.Context) (*personal.DeviceEncrypted, string, bool) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return nil, "", false
	}
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "设备控制台未启用（adb 管理器未配置）"})
		return nil, "", false
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return nil, "", false
	}
	serial, all, ok := h.pickOnlineSerial(c.Request.Context(), dev)
	if !ok {
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备离线：所有 adb 通道不可用（" + strings.Join(all, " / ") + "）"})
		return nil, "", false
	}
	return dev, serial, true
}

// pickOnlineSerial 取设备所有候选 serial（USB 主通道优先、WiFi 备用次之）并返回首个 online 的。
func (h *PersonalHandler) pickOnlineSerial(ctx context.Context, dev *personal.DeviceEncrypted) (serial string, allSerials []string, ok bool) {
	all := make([]string, 0, 2)
	if dev.AdbHost != "" {
		if dev.AdbPort > 0 {
			all = append(all, fmt.Sprintf("%s:%d", dev.AdbHost, dev.AdbPort))
		} else {
			all = append(all, dev.AdbHost)
		}
	}
	if dev.AdbWifiHost != "" && dev.AdbWifiPort > 0 {
		all = append(all, fmt.Sprintf("%s:%d", dev.AdbWifiHost, dev.AdbWifiPort))
	}
	if len(all) == 0 {
		return "", all, false
	}
	for _, s := range all {
		if h.adbMgr.Online(ctx, s) {
			return s, all, true
		}
	}
	return "", all, false
}

// handleDeviceScreenshot 设备截图（按需渲染：前端点开设备才拉取）
// GET /api/admin/personal/devices/:id/screenshot → PNG
func (h *PersonalHandler) handleDeviceScreenshot(c *gin.Context) {
	dev, serial, ok := h.getDeviceWithAdb(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	img, err := h.adbMgr.Screenshot(ctx, serial)
	if err != nil {
		logger.Warn("admin", "设备截图失败", "device", dev.DeviceID, "serial", serial, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备截图失败: " + err.Error()})
		return
	}
	c.Header("Content-Type", "image/png")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "image/png", img)
}

// handleDeviceAction 设备操作（点击/滑动/输入/按键/启动 app/刷新前台）
// POST /api/admin/personal/devices/:id/action  {"action":"tap|swipe|input|key|start|front", ...}
func (h *PersonalHandler) handleDeviceAction(c *gin.Context) {
	dev, serial, ok := h.getDeviceWithAdb(c)
	if !ok {
		return
	}
	var req struct {
		Action   string `json:"action"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
		X2       int    `json:"x2"`
		Y2       int    `json:"y2"`
		Duration int    `json:"duration"` // swipe 毫秒
		Text     string `json:"text"`     // input 文本
		Keycode  int    `json:"keycode"`  // key 按键
		Package  string `json:"package"`  // start 包名
		Activity string `json:"activity"` // start activity（可选）
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	var err error
	switch req.Action {
	case "tap":
		err = h.adbMgr.Tap(ctx, serial, req.X, req.Y)
	case "swipe":
		err = h.adbMgr.Swipe(ctx, serial, req.X, req.Y, req.X2, req.Y2, req.Duration)
	case "input":
		if req.Text == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "input 需要 text"})
			return
		}
		err = h.adbMgr.InputText(ctx, serial, req.Text)
	case "key":
		if req.Keycode == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key 需要 keycode（4=返回 3=主页 66=回车 28=删除）"})
			return
		}
		err = h.adbMgr.KeyEvent(ctx, serial, req.Keycode)
	case "start":
		if req.Package == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "start 需要 package"})
			return
		}
		err = h.adbMgr.StartApp(ctx, serial, req.Package, req.Activity)
	case "front":
		var pkg string
		pkg, err = h.adbMgr.ForegroundApp(ctx, serial)
		if err == nil {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "foreground": pkg}})
			return
		}
	case "warmup":
		// 手动预热：向设备 DeviceActor 投递 ManualWarmupCmd（冷启动导航到账本页 + 锁定任务）。
		// provider 参数指定通道：alipay（默认，设备全部支付宝账户主空间+双开）
		// 或 wechat（设备全部微信账户）；provider 为空 = 全部通道全部账户。
		// DeviceActor 模型：手动预热是队列中的一个命令，与自动预热/轮询天然互斥。
		// 冷启动导航等待波动（网络/UI 渲染非线性），微信小账本为小程序加载更慢——
		// 每步 3s + 重试，多账户累计可能超 120s，给足 240s 余量。
		ctx, cancel2 := context.WithTimeout(c.Request.Context(), 240*time.Second)
		defer cancel2()
		providers := map[string]bool{}
		switch req.Package {
		case "wechat":
			providers["wechat"] = true
		case "alipay":
			providers["alipay"] = true
		default:
			providers["alipay"] = true
			providers["wechat"] = true
		}
		if h.ledgerWarmup == nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "账本监听未启用（服务端未配置 adb）"})
			return
		}
		if werr := h.ledgerWarmup(ctx, dev.DeviceID, providers); werr != nil {
			logger.Warn("admin", "手动预热失败", "device", dev.DeviceID, "err", werr)
			c.JSON(http.StatusBadGateway, gin.H{"error": "手动预热失败: " + werr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"ok": true, "note": "账本页已就绪，任务卡片已锁定",
		}})
		return
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知 action: " + req.Action})
		return
	}
	if err != nil {
		logger.Warn("admin", "设备操作失败", "device", dev.DeviceID, "action", req.Action, "err", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "设备操作失败: " + err.Error()})
		return
	}
	logger.Info("admin", "设备操作成功", "device", dev.DeviceID, "serial", serial, "action", req.Action)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// deviceAlipayUserIDs 解析设备全部支付宝账户的用户空间（0=主空间，999=双开）：
// 遍历设备绑定账户中的 alipay 账户，取其收款码与该账户 QRContent 比对（与
// cmd/gateway resolveLedgerDevices 的 ledgerUserSpaceOf 同逻辑），去重返回 userID 列表。
func (h *PersonalHandler) deviceProviderUserIDs(c *gin.Context, dev *personal.DeviceEncrypted, channel string) ([]int, error) {
	ctx := c.Request.Context()
	ids := store.DeviceAccountIDs(ctx, h.pdb, dev.ID, dev.AccountID)
	platform := "alipay"
	if channel == "personal.wechat" {
		platform = "wechat"
	}
	var out []int
	seen := map[int]bool{}
	for _, aid := range ids {
		acct := h.pool.GetByID(aid)
		if acct == nil || acct.Channel != channel {
			continue
		}
		if acct.QRContent == "" {
			return nil, fmt.Errorf("账户 %d 未配置收款码（无法判定用户空间）", aid)
		}
		for _, space := range []int{999, 0} {
			qr, err := h.pdb.GetLatestQrcodeByPlatformUser(ctx, dev.DeviceID, platform, space)
			if err == nil && qr != nil && qr.QRContent == acct.QRContent && !seen[space] {
				seen[space] = true
				out = append(out, space)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("设备未绑定 %s 账户", platform)
	}
	return out, nil
}

// handleRestartDevice 重启设备（设备离线自愈 / 用户手动触发）
//
// POST /api/admin/personal/devices/:id/restart
//
// 2026-08-30 重构：监听端 APP 弃用，恢复链路收敛为纯 adb 可达性自愈——
// 轮询 adb.Online 直到任一 serial 可用（每 5s×9 = 45s），45s 内恢复即成功；
// 始终不可达返回 ok=false + warning。不会直接修改设备 status，
// 设备恢复后由 DeviceWatcher 按 adb 可达性自动判定上线。
func (h *PersonalHandler) handleRestartDevice(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	if h.adbMgr == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "adb 管理器未配置，重启功能不可用"})
		return
	}
	dev, err := h.pdb.GetDeviceByID(c.Request.Context(), id)
	if err != nil || dev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "设备不存在"})
		return
	}

	result, rerr := h.Restart(c.Request.Context(), dev)
	if rerr != nil && result == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": rerr.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// AttemptRestart 按 id 重启设备（实现 reconciler.Recoverer 接口）。
// 用于 DeviceWatcher 自动自愈；限频在内部已处理。
func (h *PersonalHandler) AttemptRestart(ctx context.Context, id int64) error {
	if h.adbMgr == nil {
		return fmt.Errorf("adb 管理器未配置")
	}
	dev, err := h.pdb.GetDeviceByID(ctx, id)
	if err != nil {
		return fmt.Errorf("查询设备失败: %w", err)
	}
	if dev == nil {
		return fmt.Errorf("设备 %d 不存在", id)
	}
	if _, rerr := h.Restart(ctx, dev); rerr != nil {
		return rerr
	}
	return nil
}

// Restart 公开重启入口，供 admin HTTP handler 和 DeviceWatcher 自动自愈复用。
// 限频由本方法负责（in-memory map + TTL），调用方不必关心防抖。
// 返回的 map 结构稳定可序列化为 JSON 给 HTTP 响应；err 仅在「无法开始重启流程」时返回，
// 例：adb 管理器未配置 / serial 候选空（HTTP 侧据此返回 400）。
//
// 2026-08-30 重构：监听端 APP 弃用，重启链路收敛为纯 adb 可达性自愈——
// 轮询 adb.Online 直到任一 serial 可用（每 5s×9 = 45s）；45s 内恢复即成功，
// 始终不可达则返回 ok=false + warning（由调用方/DeviceWatcher 按告警策略处理）。
func (h *PersonalHandler) Restart(ctx context.Context, dev *personal.DeviceEncrypted) (map[string]any, error) {
	if h.adbMgr == nil {
		return nil, fmt.Errorf("adb 管理器未配置")
	}
	serials := h.deviceSerials(dev)
	if len(serials) == 0 {
		return nil, fmt.Errorf("设备未配置 adb 连接（adb_host / adb_wifi_host 为空）")
	}

	// 限频：5 分钟内同设备只允许一次重启尝试；recoverer 可由 watcher 频繁触发。
	if !h.restartLimiter.allow(dev.ID, 5*time.Minute) {
		return map[string]any{
			"ok":        true,
			"serials":   serials,
			"throttled": true,
			"hint":      "距上次重启尝试不足 5 分钟，已忽略本次请求",
		}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp := map[string]any{
		"ok":       true,
		"serials":  serials,
		"stages":   []map[string]any{},
		"warnings": []string{},
	}
	stages := []map[string]any{}
	warnings := []string{}

	// 阶段 1：轮询 adb.Online（每 5s/次，共 9 次）
	serial := ""
	var online bool
	var polls int
	for polls = 0; polls < 9; polls++ {
		for _, s := range serials {
			if h.adbMgr.Online(cctx, s) {
				serial = s
				online = true
				break
			}
		}
		if online {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if online {
		resp["online_recovered"] = true
		resp["waited_seconds"] = (polls + 1) * 5
		stages = append(stages, map[string]any{"stage": "adb探测", "online": true, "serial": serial})
	} else {
		resp["ok"] = false
		resp["waited_seconds"] = 45
		warnings = append(warnings, "45s 内 adb 未恢复在线（USB/WiFi 通道均不可达）")
	}

	resp["stages"] = stages
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	logger.Info("admin", "设备重启指令执行完成", "device", dev.DeviceID, "ok", resp["ok"],
		"online_recovered", resp["online_recovered"], "stages", len(stages))
	return resp, nil
}

// restartLimiter 限频器：每设备 5min/次的重启尝试上限；进程重启即清空，可接受。
type restartLimiter struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

func newRestartLimiter() *restartLimiter {
	return &restartLimiter{last: make(map[int64]time.Time)}
}

// allow 返回 true 表示放行，false 表示被限频。
func (r *restartLimiter) allow(id int64, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.last[id]; ok && time.Since(t) < window {
		return false
	}
	r.last[id] = time.Now()
	return true
}

// deviceSerials 返回该设备的所有 adb serial 候选（USB 主通道 + WiFi 备用通道）。
// 用于本机指令在通道间依次尝试；与 getDeviceWithAdb 一致。
func (h *PersonalHandler) deviceSerials(d *personal.DeviceEncrypted) []string {
	out := make([]string, 0, 2)
	if d.AdbHost != "" {
		if d.AdbPort > 0 {
			out = append(out, fmt.Sprintf("%s:%d", d.AdbHost, d.AdbPort))
		} else {
			out = append(out, d.AdbHost)
		}
	}
	if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
		out = append(out, fmt.Sprintf("%s:%d", d.AdbWifiHost, d.AdbWifiPort))
	}
	return out
}
