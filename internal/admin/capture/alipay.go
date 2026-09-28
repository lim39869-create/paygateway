// 支付宝收款码服务端 adb 捕获。
//
// 链路（微信式「保存到相册」方案，实测更稳定）：启动支付宝 → 点「收付款」→ 点「收钱」进入
// 个人收款码页(PayeeQRActivity) → 点「保存图片」→ 图片落系统相册 DCIM/Alipay（不受
// FLAG_SECURE 限制）→ 拉取最新 → 解码。不再依赖截图链路（截图方案易卡在收付款总页/付款码页）。
package capture

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"paygateway/internal/adbmgr"
	"paygateway/internal/logger"
)

// alipayPackage 支付宝主包名（主空间 user 0 与分身 user 999 同包名；
// 本设备主空间支付宝被禁用 enabled=0，实际使用 user 999 分身）
const alipayPackage = "com.eg.android.AlipayGphone"

// alipayLaunchActivity 支付宝启动 activity（resolve-activity 实测）
const alipayLaunchActivity = ".AlipayLogin"

// alipayHomePayEntry 支付宝首页「收付款」入口（手机 OCR (405,382)）
var alipayHomePayEntry = wxNavPt{405, 382}

// alipayCollectMoneyBtn 「收付款」页(向商家付款)内的「收钱」按钮（手机 (225,1688)，
// 2026-09-12 uiautomator dump 实测标定）。
// 必须点它才能进入个人收款码页(PayeeQRActivity)；漏掉会停留在付款码页。
var alipayCollectMoneyBtn = wxNavPt{225, 1688}

// alipaySaveImgBtn 个人收款码页(PayeeQRActivity)「保存图片」按钮（手机 (540,1202)，
// 2026-09-12 uiautomator dump 实测标定）。
// 点击后支付宝把收款码写入系统相册 DCIM/Alipay/，不受 FLAG_SECURE 限制。
var alipaySaveImgBtn = wxNavPt{540, 1202}

// alipayPersonalTab 「个人收钱」tab（手机 (252,290)）。
// 收款码页有「个人收钱」/「经营收钱」双态：经营态需开通且无个人收款码，
// 点此 tab 确保切回个人收钱态（与微信个人/经营收款码同机制）。
var alipayPersonalTab = wxNavPt{252, 290}

// AlipayQrDir 支付宝收款码图片落盘目录（user 0 主空间；双开 user 由调用方拼接）。
const AlipayQrDir = "/storage/emulated/0/DCIM/Alipay"

// CaptureAlipayQrcode 核心：启动支付宝 → 点「收付款」→ 点「收钱」进收款码页 → 点「保存图片」
// → 从相册拉取最新图片 → 解码。
// 返回解码内容。userId>0 时以该 user 启动（双开/分身空间），相册路径同步切换。
func CaptureAlipayQrcode(ctx context.Context, adb *adbmgr.Manager, serial string, userId int) (content string, err error) {
	// 1. 唤醒屏幕 + 强制重启支付宝到干净首页（指定双开 user）
	if err = wakeHome(ctx, adb, serial); err != nil {
		return "", fmt.Errorf("唤醒屏幕失败: %w", err)
	}
	if err = adb.RestartAppUser(ctx, serial, alipayPackage, alipayLaunchActivity, userId); err != nil {
		return "", fmt.Errorf("启动支付宝失败: %w", err)
	}
	time.Sleep(5 * time.Second) // 支付宝冷启动 + 首页加载（低端机/网络慢时需更久）

	// 前台守卫：支付宝未到前台绝不开始盲点坐标（重启失败/弹窗拦截时坐标会落进错误 app）
	if err = awaitForeground(ctx, adb, serial, alipayPackage, foregroundWait); err != nil {
		return "", fmt.Errorf("启动支付宝失败: %w", err)
	}

	// 2. 点首页「收付款」进入收付款总页（向商家付款页）
	if err = tapNav(ctx, adb, serial, alipayHomePayEntry); err != nil {
		return "", fmt.Errorf("点击收付款入口失败: %w", err)
	}
	time.Sleep(2500 * time.Millisecond)

	// 3. 点「收钱」进入个人收款码页（漏此步停留在付款码页，无收款码）
	if err = tapNav(ctx, adb, serial, alipayCollectMoneyBtn); err != nil {
		return "", fmt.Errorf("点击收钱按钮失败: %w", err)
	}
	// 必须确认已进入收款码页（PayeeQRActivity）；否则坐标已偏移，后续点击全落空
	if err = requireAlipayPayeePage(ctx, adb, serial); err != nil {
		return "", err
	}

	// 4. 直接点「保存图片」→ 检查相册是否新增（经营态该位置无按钮不会误点，个人态点击即保存）。
	//    相册未新增 → 说明在经营态 → 点「个人收钱」tab 切回个人态 → 再点保存。
	dir := AlipayQrDir
	if userId > 0 {
		dir = fmt.Sprintf("/storage/emulated/%d/DCIM/Alipay", userId)
	}
	// 相册基线：点击前已有的收款码图（防止把旧图当成新保存的）
	base := listAlipayImages(ctx, adb, serial, dir)

	// trySave 点「保存图片」并轮询等待相册新增（落盘延迟在慢网络/低端机上可达 10s+，固定 sleep 不可靠）
	trySave := func() bool {
		if terr := tapNav(ctx, adb, serial, alipaySaveImgBtn); terr != nil {
			return false
		}
		return pollForNewFile(ctx, base, 20*time.Second, func() []string {
			return listAlipayImages(ctx, adb, serial, dir)
		})
	}
	// 第一次保存失败 → 说明不在个人态（如经营引导态无保存按钮）。
	// 循环「点个人收钱 tab → 再点保存」最多 3 轮：切态点击可能被吞/未生效，
	// 单次补救不稳定（实测需多轮才能落到个人态并保存成功）。
	if !trySave() {
		for round := 1; round <= 3; round++ {
			if err = tapNav(ctx, adb, serial, alipayPersonalTab); err != nil {
				return "", fmt.Errorf("点击个人收钱 tab 失败: %w", err)
			}
			time.Sleep(2500 * time.Millisecond)
			if trySave() {
				break
			}
			if round == 3 {
				return "", fmt.Errorf("多轮切回个人态后保存图片仍失败（可能不在收款码页或按钮位置偏移）")
			}
		}
	}

	// 5. 拉取相册最新支付宝收款码图片（按文件名倒序取第一张）
	files, lerr := adb.ListFiles(ctx, serial, dir)
	if lerr != nil {
		return "", fmt.Errorf("读取相册失败: %w", lerr)
	}
	if len(files) == 0 {
		return "", fmt.Errorf("支付宝相册为空（保存图片未落盘）")
	}
	latest := files[0] // ListFiles 按 -t 倒序，第一个最新
	local, perr := adb.PullFile(ctx, serial, dir+"/"+latest)
	if perr != nil {
		return "", fmt.Errorf("拉取收款码图片失败: %w", perr)
	}
	imgData, rerr := os.ReadFile(local)
	if rerr != nil {
		os.Remove(local)
		return "", fmt.Errorf("读取图片失败: %w", rerr)
	}
	content, derr := DecodeQRContent(imgData)
	if derr != nil {
		os.Remove(local)
		return "", fmt.Errorf("收款码图片未识别到二维码: %w", derr)
	}
	return content, nil
}

// requireAlipayPayeePage 确认支付宝前台已到达收款码页（PayeeQRActivity）。
// 带重试（最多 6 次×1.5s）。若点「收钱」后仍停在收付款页，重试点击一次再校验。
func requireAlipayPayeePage(ctx context.Context, adb *adbmgr.Manager, serial string) error {
	for i := 0; i < 6; i++ {
		fg, err := adb.ForegroundApp(ctx, serial)
		// 双开 user 前台字符串形如 "u999 com.eg.android.AlipayGphone/...PayeeQRActivity"，用 Contains 匹配
		if err == nil && strings.Contains(fg, "PayeeQRActivity") {
			return nil
		}
		if i > 0 && i%3 == 0 {
			// 页面加载慢导致第一次点击被吞：重试点「收钱」
			if terr := tapNav(ctx, adb, serial, alipayCollectMoneyBtn); terr != nil {
				return fmt.Errorf("重试点击收钱失败: %w", terr)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	fg, _ := adb.ForegroundApp(ctx, serial)
	return fmt.Errorf("未进入支付宝收款码页（当前前台 %q）", fg)
}

// alipayRecordSummary 收款码页(PayeeQRActivity)下方「收款记录」入口
// （2026-09-12 uiautomator dump 实测：收款码页「收款记录」中心 (251,1401)）。
// 注意：点击后进入的是【脆弱界面】(XRiverTransActivity)，仅 24h 内记录且不稳定——
// 需按 CaptureAlipayLedger 全链路（脆弱→灰区→总览→稳定）继续导航。
var alipayRecordSummary = wxNavPt{251, 1401}

// 脆弱界面→稳定界面导航段（2026-09-12 uiautomator dump 实测标定）：
//   - alipayLedgerFragileGray 脆弱界面(XRiverTransActivity)「收款记录」标题上方约 1 公分处
//     （灰色区域），点击后收起脆弱界面回到收款总览界面(XRiverActivity)。实测脆弱界面
//     「收款记录」标题中心 (540,426)，上方约 1 公分 ≈ (540,262)。
//   - alipayLedgerOverviewEntry 收款总览界面(XRiverActivity)内的「收款记录」按钮
//     （uiautomator dump 实测中心 (203,583)），点击后进入稳定的完整收款记录页
//     (XRiverActivity$App01，历史全部记录，支持下拉刷新）。
var alipayLedgerFragileGray = wxNavPt{540, 262}
var alipayLedgerOverviewEntry = wxNavPt{203, 583}

// CaptureAlipayLedger 导航到支付宝稳定的完整收款记录页（监听界面，PLAN-EVENT-MONITOR §2.3）。
//
// 链路（2026-08-27 真机反馈修正）：启动支付宝 → 点「收付款」→ 点「收钱」进收款码页(PayeeQRActivity)
// → 点「收款记录」→ 进入【脆弱界面】(XRiverTransActivity，仅 24h 内记录，不稳定，可能显示成别的)
// → 点脆弱界面上方灰色区域 → 回到收款总览界面(XRiverActivity) → 收款总览界面点「收款记录」
// → 进入【稳定界面】(XRiverActivity$App01，历史全部记录，支持下拉刷新)。
//
// 关键：不能把 XRiverTransActivity 当作"已到达"判据——那是脆弱界面，误判成功会导致
// 读到不完整的 24h 内记录。必须走完「脆弱→灰区→总览→稳定」全链路。userId>0 时以该 user 启动。
func CaptureAlipayLedger(ctx context.Context, adb *adbmgr.Manager, serial string, userId int) error {
	if err := wakeHome(ctx, adb, serial); err != nil {
		return fmt.Errorf("唤醒屏幕失败: %w", err)
	}
	if err := adb.RestartAppUser(ctx, serial, alipayPackage, alipayLaunchActivity, userId); err != nil {
		return fmt.Errorf("启动支付宝失败: %w", err)
	}
	time.Sleep(5 * time.Second) // 冷启动 + 首页加载

	// 前台守卫（同 CaptureAlipayQrcode）：支付宝未到前台绝不开始盲点坐标
	if err := awaitForeground(ctx, adb, serial, alipayPackage, foregroundWait); err != nil {
		return fmt.Errorf("启动支付宝失败: %w", err)
	}

	// 屏幕固定保险：固定支付宝前台任务，导航期间其他 app（监听 app/系统弹窗）
	// 无法抢占前台（实测 am start 被拦截 error 101），保证坐标导航不被打断。
	// 导航结束（含失败路径）defer 解锁，恢复自由切换。
	if taskID, perr := adb.PinCurrentTask(ctx, serial); perr == nil {
		defer func() { _ = adb.UnpinTask(ctx, serial) }()
		_ = taskID
	} else {
		logger.Warn("capture", "屏幕固定失败（导航将无保护执行，可能被其他 app 打断）",
			"err", perr, "serial", serial)
	}

	// 广告弹窗防御：首页布局就绪判定 = dump 出现「收付款」。广告弹窗遮挡时
	// 入口不可见 → 点一次收付款位置关闭弹窗回首页，再给足时间等首页出现。
	// 简单高效：只做一轮判断，不循环轮询（导航可靠性靠固定等待，不靠状态判断）。
	xml, _ := adb.DumpUI(ctx, serial)
	if !strings.Contains(xml, "收付款") {
		if terr := tapPinned(ctx, adb, serial, alipayHomePayEntry.X, alipayHomePayEntry.Y); terr != nil {
			return fmt.Errorf("点击收付款入口（广告弹窗防御）失败: %w", terr)
		}
		time.Sleep(3 * time.Second)
	}

	if err := tapPinned(ctx, adb, serial, alipayHomePayEntry.X, alipayHomePayEntry.Y); err != nil {
		return fmt.Errorf("点击收付款入口失败: %w", err)
	}
	time.Sleep(3 * time.Second) // 收付款页加载（低端机/网络慢需更久）

	if err := tapPinned(ctx, adb, serial, alipayCollectMoneyBtn.X, alipayCollectMoneyBtn.Y); err != nil {
		return fmt.Errorf("点击收钱按钮失败: %w", err)
	}
	if err := requireAlipayPayeePage(ctx, adb, serial); err != nil {
		return err
	}
	// 经营态处理：收款码页默认可能是「经营收钱」态（页面布局不同，「收款记录」入口位置
	// 与个人态不一致）。进账本前先点「个人收钱」tab 切回个人态，保证后续账本入口坐标有效。
	// 与 CaptureAlipayQrcode 的经营态补救同机制，但这里主动先切（幂等：个人态点它无副作用）。
	if err := tapPinned(ctx, adb, serial, alipayPersonalTab.X, alipayPersonalTab.Y); err != nil {
		return fmt.Errorf("点击个人收钱 tab 失败: %w", err)
	}
	time.Sleep(1500 * time.Millisecond) // 切态动画

	// ① 收款码页点「收款记录」→ 进入脆弱界面（一定脆弱，无需 if 判断）
	if err := tapPinned(ctx, adb, serial, alipayRecordSummary.X, alipayRecordSummary.Y); err != nil {
		return fmt.Errorf("点击收款记录入口失败: %w", err)
	}
	time.Sleep(2500 * time.Millisecond) // 脆弱界面加载（WebView 渲染较慢）
	// ② 脆弱界面点上方灰色区域 → 回到收款总览界面
	if err := tapPinned(ctx, adb, serial, alipayLedgerFragileGray.X, alipayLedgerFragileGray.Y); err != nil {
		return fmt.Errorf("点击脆弱界面灰色区域失败: %w", err)
	}
	// ③ 从总览界面点「收款记录」→ 进入稳定界面
	return NavigateOverviewToLedger(ctx, adb, serial)
}

// NavigateOverviewToLedger 从收款总览界面点「收款记录」进入稳定账本页。
// 供冷启动全链路（CaptureAlipayLedger）与热启动补点（EnsureForeground 恢复时
// 前台在总览界面）共用。
func NavigateOverviewToLedger(ctx context.Context, adb *adbmgr.Manager, serial string) error {
	// 屏幕固定（PINNED）模式下 input tap/swipe 注入事件会被拦截（2026-08-28 实测：
	// 固定态点击无效，解锁后同坐标立即生效）——必须先释放固定，否则「收款记录」
	// 点击不生效卡在总览界面。解锁后 defer 仍会幂等解锁。
	if err := adb.UnpinTask(ctx, serial); err != nil {
		logger.Warn("capture", "释放屏幕固定失败（继续尝试点击）",
			"err", err, "serial", serial)
	}
	// 总览界面含横幅加载动画，固定等待 2s 让布局稳定（2026-08-28 实测）。
	// 不做 dump 文本检测（「今日收款」）作为点击前置——dump 时序不可靠会误判
	// 未就绪导致断链（2026-08-28 实测：预热走到此步因检测失败而中断，界面实际已就绪）。
	time.Sleep(2 * time.Second)
	// 点击「收款记录」进入稳定账本页；以活动名确认到达，失败重试（最多 3 次）。
	var lastErr error
	for i := 0; i < 3; i++ {
		if err := tapNav(ctx, adb, serial, alipayLedgerOverviewEntry); err != nil {
			return fmt.Errorf("点击总览界面收款记录失败: %w", err)
		}
		if err := requireAlipayLedgerPage(ctx, adb, serial); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return fmt.Errorf("点击收款记录后未到达稳定账本页: %w", lastErr)
}

// waitOverviewReady 轮询等待收款总览界面加载完成（dump 出现「今日收款」文本）。
// 总览界面有顶部横幅加载动画，固定 sleep 可能在布局未稳定时进入下一步。
// 用 dump 文本判断是用户认可的"快速首页判定"同款机制，非 Activity 状态判断。
func waitOverviewReady(ctx context.Context, adb *adbmgr.Manager, serial string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		xml, err := adb.DumpUI(ctx, serial)
		if err == nil && strings.Contains(xml, "今日收款") {
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

// requireAlipayLedgerPage 确认支付宝前台已到达稳定的完整收款记录页。
// 判据 = XRiverActivity$App01（稳定界面）。注意不能匹配 XRiverTransActivity——
// 那是脆弱界面（24h 内记录，不稳定），误判会导致读到不完整数据。
func requireAlipayLedgerPage(ctx context.Context, adb *adbmgr.Manager, serial string) error {
	for i := 0; i < 6; i++ {
		fg, err := adb.ForegroundApp(ctx, serial)
		if err == nil && strings.Contains(fg, "XRiverActivity$App") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	fg, _ := adb.ForegroundApp(ctx, serial)
	return fmt.Errorf("未进入支付宝稳定收款记录页（当前前台 %q）", fg)
}

// CleanupAlipayGallery 清理指定 user 空间支付宝相册里的旧收款码图（保留最新 QrcodeKeepOnDevice 张）。
// 与微信相册清理同策略：防止频繁捕获打满存储。
func CleanupAlipayGallery(ctx context.Context, adb *adbmgr.Manager, serial string, userId int) (deleted int) {
	dir := AlipayQrDir
	if userId > 0 {
		dir = fmt.Sprintf("/storage/emulated/%d/DCIM/Alipay", userId)
	}
	files, err := adb.ListFiles(ctx, serial, dir)
	if err != nil || len(files) <= QrcodeKeepOnDevice {
		return 0 // 目录不存在/不可读 或 未超保留数 → 无码可清
	}
	for _, f := range files[QrcodeKeepOnDevice:] { // ListFiles 按 -t 倒序，越靠后越旧
		if err := adb.Rm(ctx, serial, dir+"/"+f); err != nil {
			continue
		}
		deleted++
	}
	return deleted
}

// listAlipayImages 列出支付宝相册图片文件名（按 -t 倒序）。
func listAlipayImages(ctx context.Context, adb *adbmgr.Manager, serial, dir string) []string {
	files, err := adb.ListFiles(ctx, serial, dir)
	if err != nil {
		return nil
	}
	return files
}
