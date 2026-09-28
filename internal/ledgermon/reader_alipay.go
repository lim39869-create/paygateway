package ledgermon

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"paygateway/internal/adbmgr"
	"paygateway/internal/admin/capture"
)

// 支付宝常量（与 capture 包一致；独立声明避免导出 capture 内部常量）
const (
	alipayPkg          = "com.eg.android.AlipayGphone"
	alipayLaunchAct    = ".AlipayLogin"
	alipayLedgerAct    = "XRiverActivity$App" // 稳定的完整收款记录页（2026-08-27 真机实测）
	alipayLedgerActStr = "XRiverActivity$App"
)

// LedgerReader 账本页读取器接口（按通道分派）
type LedgerReader interface {
	// EnsureForeground 确保账本页在前台（免点击恢复，失败返回错误 → 本轮跳过）。
	// 下单实时回调路径：只走热启动（task lock），绝不冷启动。
	EnsureForeground(ctx context.Context) error
	// Read 读取当前前台账本页可见行。页面无变化返回 (nil, nil)（变化检测跳过解析）。
	Read(ctx context.Context) ([]capture.AlipayLedgerRow, error)
}

// Warmer 可选接口：预热专用导航（task lock 热启动优先 → 失败走冷启动坐标导航建账本页）。
// 预热阶段（warmupOnce）调用——账本页任务栈不存在时必须用冷启动建立；
// 实时回调（pollOnce）只用 EnsureForeground（纯热启动，绝不冷启动）。
type Warmer interface {
	Warmup(ctx context.Context) error
}

// FreshReader 可选接口：实现方支持免下拉刷新读取。冷启动导航刚完成时账本页
// 就是最新数据，无需下拉刷新（省 1.5s 且避免刷新动画扰动）。
type FreshReader interface {
	ReadFresh(ctx context.Context) ([]capture.AlipayLedgerRow, error)
}

// TaskLocker 可选接口：实现方支持锁定最近任务（防一键清理移除任务栈，
// 保证热启动能恢复账本页，避免重复冷启动导航）。无此能力时预热跳过锁定。
type TaskLocker interface {
	LockTasks(ctx context.Context) (int, error)
}

// AlipayReader 支付宝账本页 dump 文本通道读取器。
//
// 链路：uiautomator dump（系统级通道，不受 FLAG_SECURE 限制，实测可读出账本行）
// → 复用 capture.ReadAlipayLedger 解析（付款方|HH:MM:SS|+金额）。
// 变化检测：dump XML 文本哈希对比上一帧（页面无变化 → 不解析直接返回空）。
//
// 关键：账本页是 WebView 容器（NebulaX），数据不自动刷新——新收款不会自动出现在
// 页面里，必须下拉刷新（pull-to-refresh）触发数据重载后才能 dump 到新行。
type AlipayReader struct {
	adb          *adbmgr.Manager
	serial       string
	userId       int // 0=主空间，999=双开分身
	lastDumpHash uint64
}

// 下拉刷新手势参数（实测账本页 WebView 需下拉才能重载数据）：
// 从列表区中部 (540,600) 向下滑到 (540,1700)，触发 pull-to-refresh。
const (
	refreshSwipeX   = 540
	refreshSwipeY0  = 600
	refreshSwipeY1  = 1700
	refreshSwipeMs  = 400
	refreshLoadWait = 1500 * time.Millisecond // 刷新动画 + 数据重载等待
)

// NewAlipayReader 创建支付宝读取器。userId>0 时以该 user 空间启动（双开）。
func NewAlipayReader(adb *adbmgr.Manager, serial string, userId int) *AlipayReader {
	return &AlipayReader{adb: adb, serial: serial, userId: userId}
}

// refresh 下拉刷新账本页（WebView 数据重载）。失败不致命：下一轮重试。
func (r *AlipayReader) refresh(ctx context.Context) error {
	err := r.adb.SwipeRef(ctx, r.serial, refreshSwipeX, refreshSwipeY0, refreshSwipeX, refreshSwipeY1, refreshSwipeMs)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(refreshLoadWait):
	}
	return nil
}

// EnsureForeground 确保支付宝完整收款记录页(XRiverTransActivity)在前台。
//
// 下单实时回调路径（2026-08-28 重构）：只走热启动，绝不冷启动。
//  1. 前台检测（dumpsys 三要素：包名+Activity+用户空间，~100ms）：已在账本页 → 零开销返回
//  2. 热启动（~260ms）：am task lock 直接唤醒 lite01 账本页任务栈到前台
//     ——am start .AlipayLogin 只恢复主任务栈（栈顶首页/总览），稳定账本页
//     XRiverActivity$App 在 lite01 亲和栈，am start 永远到不了它。task lock 按
//     taskId 精确唤醒账本页栈，与微信 taskLock(wechatTaskID) 同机制。
//  3. 进程被杀/任务栈丢失/唤醒失败 → 返回错误本轮跳过（不读错页面）。
//     冷启动导航(CaptureAlipayLedger)只在预热阶段执行（设备上线 warmupOnce），
//     绝不出现在下单实时回调——17.7s 冷启动会错过付款窗口。
func (r *AlipayReader) EnsureForeground(ctx context.Context) error {
	// ① 已在账本页（三要素确认，防双开/异应用误判）
	if r.isLedgerForeground(ctx) {
		return nil
	}
	// ② 进程被杀 → 任务栈已丢，task lock 无栈可唤醒，本轮跳过（不冷启动）
	if alive, _ := r.adb.ProcessAlive(ctx, r.serial, alipayPkg); !alive {
		return fmt.Errorf("支付宝进程不存在，热启动无法恢复（需重新预热）")
	}
	// ③ 热启动：am task lock 唤醒账本页任务栈（微信同款，~260ms）
	taskID, terr := r.adb.FindLedgerTaskID(ctx, r.serial, alipayPkg, alipayLedgerActStr, r.userId)
	if terr != nil {
		return fmt.Errorf("账本页任务栈未找到，热启动无法恢复（需重新预热）: %w", terr)
	}
	if lerr := r.adb.TaskLock(ctx, r.serial, taskID); lerr != nil {
		return fmt.Errorf("task lock 唤醒账本页任务栈失败: %w", lerr)
	}
	// 唤醒成功后 PINNED 锁定前台，确认到账本页立即释放（否则拦截后续下拉刷新）
	defer func() { _ = r.adb.UnpinTask(ctx, r.serial) }()
	if !r.waitLedgerForeground(ctx, 2*time.Second) {
		return fmt.Errorf("task lock 唤醒后未确认账本页前台")
	}
	return nil
}

// Warmup 预热专用导航（Warmer 接口）：task lock 热启动优先，账本页任务栈不存在
// （进程被杀/首次启动/被清理）时走冷启动坐标导航建账本页。仅预热阶段调用。
func (r *AlipayReader) Warmup(ctx context.Context) error {
	// 已在账本页 → 零开销
	if r.isLedgerForeground(ctx) {
		return nil
	}
	// task lock 热启动优先
	if taskID, terr := r.adb.FindLedgerTaskID(ctx, r.serial, alipayPkg, alipayLedgerActStr, r.userId); terr == nil {
		if lerr := r.adb.TaskLock(ctx, r.serial, taskID); lerr == nil {
			if r.waitLedgerForeground(ctx, 2*time.Second) {
				_ = r.adb.UnpinTask(ctx, r.serial)
				return nil
			}
			_ = r.adb.UnpinTask(ctx, r.serial)
		}
	}
	// 冷启动坐标导航建账本页（仅预热阶段，绝不用于实时回调）
	return capture.CaptureAlipayLedger(ctx, r.adb, r.serial, r.userId)
}

// isLedgerOverview 前台是否为收款总览界面（XRiverActivity，非稳定页）。
// 热启动恢复到主任务栈栈顶时是总览界面（稳定页在 lite01 栈），需据此补点进入稳定页。
// 注意：ForegroundAppDetail 返回的活动名带 dumpsys ActivityRecord 的尾部 '}'（如
// "...XRiverActivity}"），不能精确 == 匹配，必须子串匹配；同时排除稳定页 "$App" 后缀。
func (r *AlipayReader) isLedgerOverview(ctx context.Context) bool {
	pkg, act, uid, err := r.adb.ForegroundAppDetail(ctx, r.serial)
	return err == nil && pkg == alipayPkg &&
		strings.Contains(act, "XRiverActivity") &&
		!strings.Contains(act, alipayLedgerActStr) && uid == r.userId
}

// isLedgerForeground 前台是否已是本账户账本页（包名 + Activity + 用户空间三要素全匹配）。
// 双开(user999)与主空间(user0)的账本页 Activity 同名，必须按 userID 区分，
// 否则 user0 的监听循环会把 user999 的账本页误判为已就绪 → 下拉刷新读到的全是错误账户数据。
func (r *AlipayReader) isLedgerForeground(ctx context.Context) bool {
	pkg, act, uid, err := r.adb.ForegroundAppDetail(ctx, r.serial)
	return err == nil && pkg == alipayPkg &&
		strings.Contains(act, alipayLedgerActStr) && uid == r.userId
}

// waitLedgerForeground 轮询等待账本页出现在前台（最长 timeout）
func (r *AlipayReader) waitLedgerForeground(ctx context.Context, timeout time.Duration) bool {
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

// Read dump 当前账本页并解析可见行（带下拉刷新：WebView 数据不自动更新，
// 新收款必须触发重载后才能被 dump 读到）。页面无变化 → 返回 (nil, nil)。
func (r *AlipayReader) Read(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	if err := r.refresh(ctx); err != nil {
		return nil, err
	}
	return r.readDump(ctx)
}

// ReadFresh 免下拉刷新读取（FreshReader 接口实现）。冷启动导航刚完成时账本页
// 就是最新数据，下拉刷新是多余开销（省 1.5s 且避免刷新动画扰动页面判定）。
func (r *AlipayReader) ReadFresh(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	return r.readDump(ctx)
}

// readDump dump 当前账本页并解析可见行，带变化检测。
func (r *AlipayReader) readDump(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
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
	rows, rerr := capture.ReadAlipayLedger(xml)
	if rerr != nil {
		return nil, rerr
	}
	// 资金安全：退化空 dump（无障碍未填充→空树）误判为'0 行'→建空基线→真实历史行
	// 后续被当 fresh 错误入账。退化空 dump 返回错误，跳过本轮不建基线（同微信 readDump）。
	if len(rows) == 0 && capture.DegenerateDump(xml) {
		return nil, fmt.Errorf("账本页 dump 为空/未渲染（可能无障碍被回收或页面未就绪），拒绝建立空基线")
	}
	return rows, nil
}

// LockTasks 锁定最近任务中支付应用主任务卡片（TaskLocker 接口实现）。
// 锁定后一键清理不会移除账本页任务栈，热启动可恢复账本页（避免重复冷启动导航）。
// 每应用只锁一张主任务栈（锁多个同应用进程会触发 MIUI 保护性杀进程）。
// 锁定完成后退回最近任务界面，不调用账户级导航（避免误触发冷启动跳转）。
func (r *AlipayReader) LockTasks(ctx context.Context) (int, error) {
	return capture.LockPaymentTasks(ctx, r.adb, r.serial, "支付宝", "微信")
}
