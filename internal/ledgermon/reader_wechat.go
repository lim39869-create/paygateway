package ledgermon

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"paygateway/internal/adbmgr"
	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
)

// 微信常量（与 capture 包独立声明）
const (
	wechatPkg       = "com.tencent.mm"
	wechatLedgerAct = "AppBrandUI00" // 收款小账本小程序容器（PLAN-ADB-LEDGER.md §2.2 实测）
)

// WechatReader 微信收款记录页（AppBrandUI00 小程序容器）读取器。
//
// 与支付宝同逻辑（2026-08-28 实现）：
//   - 热启动：am task lock 唤醒账本页任务栈（微信 am start 直拉小程序被校验拒绝，
//     task lock 是唯一放行路径，task id 动态需 dumpsys 解析——PLAN-ADB-LEDGER.md §2.3）
//   - 冷启动：capture.CaptureWechatLedger 坐标导航（微信首页→我→服务→收付款→
//     二维码收款→收款小账本→收款记录）
//   - 任务卡片锁定：复用 capture.LockPaymentTasks("微信")
//   - 读取：OCR 通道（Phase 2 接入）；当前 EnsureForeground 保前台 + Read 占位空读
type WechatReader struct {
	adb          *adbmgr.Manager
	serial       string
	userId       int // 0=主空间，999=双开分身
	lastDumpHash uint64
}

// NewWechatReader 创建微信读取器。userId>0 时以该 user 空间启动（双开）。
func NewWechatReader(adb *adbmgr.Manager, serial string, userId int) *WechatReader {
	return &WechatReader{adb: adb, serial: serial, userId: userId}
}

// EnsureForeground 确保微信收款记录页(AppBrandUI00)在前台。
// 下单实时回调路径：只走热启动（task lock），绝不冷启动（与支付宝 EnsureForeground 同策略）。
// 冷启动导航(CaptureWechatLedger)只在预热阶段执行。
func (r *WechatReader) EnsureForeground(ctx context.Context) error {
	// ① 已在账本页（三要素确认）
	if r.isLedgerForeground(ctx) {
		return nil
	}
	// ② 进程被杀 → 任务栈已丢，task lock 无栈可唤醒，本轮跳过（不冷启动）
	if alive, _ := r.adb.ProcessAlive(ctx, r.serial, wechatPkg); !alive {
		return fmt.Errorf("微信进程不存在，热启动无法恢复（需重新预热）")
	}
	// ③ 热启动：am task lock 唤醒账本页任务栈（task id 动态解析）
	taskID, terr := r.adb.FindLedgerTaskID(ctx, r.serial, wechatPkg, wechatLedgerAct, r.userId)
	if terr != nil {
		return fmt.Errorf("微信账本页任务栈未找到，热启动无法恢复（需重新预热）: %w", terr)
	}
	if lerr := r.adb.TaskLock(ctx, r.serial, taskID); lerr != nil {
		return fmt.Errorf("task lock 唤醒微信账本页任务栈失败: %w", lerr)
	}
	// 唤醒成功后 PINNED 锁定前台，确认到账本页立即释放（否则拦截后续读取）
	defer func() { _ = r.adb.UnpinTask(ctx, r.serial) }()
	if !r.waitLedgerForeground(ctx, 2*time.Second) {
		return fmt.Errorf("task lock 唤醒后未确认微信账本页前台")
	}
	return nil
}

// Warmup 预热专用导航（Warmer 接口）：task lock 热启动优先，微信账本页任务栈
// （AppBrandUI00）不存在（进程被杀/首次启动/被清理）时走冷启动坐标导航建账本页。
// 与支付宝 Warmup 同逻辑。仅预热阶段调用。
func (r *WechatReader) Warmup(ctx context.Context) error {
	// 已在账本页 → 零开销
	if r.isLedgerForeground(ctx) {
		return nil
	}
	// task lock 热启动优先
	if taskID, terr := r.adb.FindLedgerTaskID(ctx, r.serial, wechatPkg, wechatLedgerAct, r.userId); terr == nil {
		if lerr := r.adb.TaskLock(ctx, r.serial, taskID); lerr == nil {
			if r.waitLedgerForeground(ctx, 2*time.Second) {
				_ = r.adb.UnpinTask(ctx, r.serial)
				return nil
			}
			_ = r.adb.UnpinTask(ctx, r.serial)
		}
	}
	// 冷启动坐标导航建账本页（仅预热阶段，绝不用于实时回调）
	return capture.CaptureWechatLedger(ctx, r.adb, r.serial, r.userId)
}

// Read 读取当前前台微信账本页可见行。
// 链路：下拉刷新重载数据 → ensureAccessibility 确保绑定 → uiautomator dump → 解析。
// 页面无变化返回 (nil, nil)。
func (r *WechatReader) Read(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	if err := r.refresh(ctx); err != nil {
		return nil, err
	}
	return r.readDump(ctx)
}

// ReadFresh 免刷新读取（冷启动导航刚完成即最新数据）。
// 与 Read 同路径（readDump 内 ensureAccessibility 紧贴 dump 前瞬态重绑）。
func (r *WechatReader) ReadFresh(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	return r.readDump(ctx)
}

// refresh 下拉刷新微信账本页重载数据（小程序 WebView 收款记录需刷新才见最新行）。
// 刷新只操作页面，不触碰无障碍——绑定由 readDump 内 ensureAccessibility 在 dump
// 前紧贴重建（2026-09-13 真机实证：刷新→等3s→激活→立即 dump 成功读出账单行）。
func (r *WechatReader) refresh(ctx context.Context) error {
	if err := r.adb.SwipeRef(ctx, r.serial, refreshSwipeX, refreshSwipeY0, refreshSwipeX, refreshSwipeY1, refreshSwipeMs); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(refreshLoadWait):
	}
	return nil
}

// readDump 确保无障碍绑定并 dump 当前微信账本页，解析可见行，带变化检测。
//
// 关键时序（红米 K60 真机实证）：MIUI MiuiBooster 即使豁免也会在数秒内回收
// 无障碍服务绑定（Bound 非空→空）。因此绑定必须【距 DumpUI 最近】——
// ensureAccessibility 紧贴 DumpUI 调用；下拉刷新等页面操作排在它之前，
// 刷新结束到 dump 之间不夹任何长动作，dump 即落在绑定窗口内
// （实证：刷新→等3s→激活→立即 dump 成功读出账单行；激活→滚动→惰待 dump 则空）。
func (r *WechatReader) readDump(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	// 紧贴 dump 前瞬态重绑 + 确认绑定（绑定被回收则重新激活，穷尽绑定窗口）
	if err := r.ensureAccessibility(ctx); err != nil {
		return nil, err
	}
	xml, err := r.adb.DumpUI(ctx, r.serial)
	if err != nil {
		return nil, err
	}
	h := fnv.New64a()
	h.Write([]byte(xml))
	sum := h.Sum64()
	if sum == r.lastDumpHash {
		return nil, nil // 页面无变化
	}
	r.lastDumpHash = sum
	rows, rerr := capture.ReadWechatLedger(xml)
	if rerr != nil {
		return nil, rerr
	}
	// 资金安全：退化空 dump（无障碍未填充→空树）会被误判为'0 行'→建立空基线，
	// 后续真实历史行变 fresh 错误入账。退化空 dump 返回错误，跳过本轮不建基线。
	if len(rows) == 0 && capture.DegenerateDump(xml) {
		return nil, fmt.Errorf("账本页 dump 为空/未渲染（可能无障碍被回收或页面未就绪），拒绝建立空基线")
	}
	return rows, nil
}

// ensureAccessibility 确保系统无障碍已开启（微信 XWeb 小程序 dump 的前置条件）。
//
// 三项必要步骤（2026-09-11 真机平板实测根因，红米 K60 亦适用）：
//  1. MIUI 省电豁免：MiuiBooster 省电守护会周期性回收 com.miui.accessibility（VoiceAccess）
//     服务进程，随后 BatterySaverPolicy 把 accessibility_enabled 清回 0（实测 ~45-50s 落回）。
//     必须在启用前先给无障碍包授予后台运行豁免（appops RUN_IN_BACKGROUND allow + 白名单），
//     否则即便 enabled=1 也会被周期性清零。豁免幂等。
//  2. 写有效服务：enabled_accessibility_services 必须写入**真实安装**的组件串。若沿用
//     已被污染的旧值（系统常清成 null），再次写回会导致 AccessibilityManager 无法绑定 →
//     连带清空 enabled。此处固定用设备实测可绑定的 MIUI VoiceAccess。
//  3. settings put secure accessibility_enabled 1 开启。
func (r *WechatReader) ensureAccessibility(ctx context.Context) error {
	if err := r.miuiBoostExempt(ctx); err != nil {
		logger.Warn("ledgermon", "MIUI 省电豁免失败（无障碍仍可能被周期性清零）",
			"device", r.serial, "err", err)
	}
	// 已真正绑定则直接复用（幂等）。不能只看 accessibility_enabled==1 提前返回——
	// 系统可能 enabled=1 但服务未绑定（污染值/MiuiBooster 清服务），此时 dump 仍退化。
	if r.accessibilityBound(ctx) {
		return nil
	}
	// 固定写设备实测可绑定的 MIUI VoiceAccess（不用旧值，防污染值连带清空）。
	// 必须用【全路径】组件串：简写 ".voiceaccess.VoiceAccessAccessibilityService"
	// 实测 AccessibilityManager 无法绑定（Bound/Enabled services 空），服务形同虚设。
	svc := "com.miui.accessibility/com.miui.accessibility.voiceaccess.VoiceAccessAccessibilityService"
	if _, err := r.adb.Shell(ctx, 10*time.Second, r.serial, "settings put secure enabled_accessibility_services "+svc); err != nil {
		logger.Warn("ledgermon", "设置无障碍服务失败", "device", r.serial, "err", err)
	}
	if _, err := r.adb.Shell(ctx, 10*time.Second, r.serial, "settings put secure accessibility_enabled 1"); err != nil {
		return fmt.Errorf("开启无障碍失败: %w", err)
	}
	// 等系统激活并绑定无障碍服务（XWeb 节点树构建需要时间；MiuiBooster 窗口内）。
	// 轮询确认已真正绑定（Bound services 非空）——enabled=1 但未绑定时 dump 仍退化空。
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if r.accessibilityBound(ctx) {
			logger.Info("ledgermon", "已开启系统无障碍（微信 dump 解锁）", "device", r.serial)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	return fmt.Errorf("系统无障碍服务未能绑定（enabled=1 但 Bound services 空），微信 dump 将退化")
}

// accessibilityBound 查询无障碍服务是否真正绑定（dumpsys accessibility Bound services 非空）。
// enabled=1 只表示开关打开，必须服务进程挂上（Bound）才暴露 XWeb 节点树。
func (r *WechatReader) accessibilityBound(ctx context.Context) bool {
	out, err := r.adb.Shell(ctx, 6*time.Second, r.serial, "dumpsys accessibility")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// Bound services:{Service[label=语音控制,...]} —— 有实际服务实例即视为已绑定
		if strings.HasPrefix(line, "Bound services:") && strings.Contains(line, "Service[") {
			return true
		}
	}
	return false
}

// miuiBoostExempt MIUI 省电/性能守护豁免：让无障碍服务包不被周期性回收。
// 实测（红米 K60/平板）：不打这个豁免，com.miui.accessibility 约 45-50s 被
// MiuiBooster 回收 → accessibility_enabled 被清 0 → 微信 dump 回到空节点树。
// 幂等：重复执行无害。豁免成功后 enabled 实测稳定保持 120s+。
func (r *WechatReader) miuiBoostExempt(ctx context.Context) error {
	const pkg = "com.miui.accessibility"
	// appops 后台运行豁免（MiuiBooster 依赖 RUN_IN_BACKGROUND 判定回收）
	if _, err := r.adb.Shell(ctx, 10*time.Second, r.serial, "cmd appops set "+pkg+" RUN_IN_BACKGROUND allow"); err != nil {
		return err
	}
	// Doze 白名单（可选，防深度空闲回收）
	_, _ = r.adb.Shell(ctx, 10*time.Second, r.serial, "dumpsys deviceidle whitelist +"+pkg)
	return nil
}

// LockTasks 锁定微信任务卡片（防一键清理移除任务栈，保证热启动可恢复）。
func (r *WechatReader) LockTasks(ctx context.Context) (int, error) {
	return capture.LockPaymentTasks(ctx, r.adb, r.serial, "微信")
}

// isLedgerForeground 前台是否为微信收款记录页（AppBrandUI00 小程序容器 + 用户空间）。
func (r *WechatReader) isLedgerForeground(ctx context.Context) bool {
	pkg, act, uid, err := r.adb.ForegroundAppDetail(ctx, r.serial)
	return err == nil && pkg == wechatPkg &&
		strings.Contains(act, wechatLedgerAct) && uid == r.userId
}

// waitLedgerForeground 轮询等待账本页出现在前台（最长 timeout）
func (r *WechatReader) waitLedgerForeground(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.isLedgerForeground(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(800 * time.Millisecond):
		}
	}
	return false
}
