// 微信收款码自动捕获（服务器 adb 驱动）。
//
// 链路（与支付宝不同）：微信收付款页 FLAG_SECURE 下 adb 截图返回空 → 无法直接解码；
// 改为 导航到「个人收款码」页 → 点「保存收款码」→ 图片落相册（不受 SECURE 限制）→
// 拉取最新图片 → 解码 → 入库。
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

// wxNavPt 导航坐标点（1080x2400 手机参考系，TapRef 自动缩放实际分辨率）。
// 仅保留手机坐标系——平板坐标系及其形态判定逻辑已整体移除（引入不可控问题）。
type wxNavPt struct {
	X, Y int
}

// wechatQrTapSeq 微信收款码导航序列（前三步跨首页→服务页，1080x2400 真机标定）：
//
//	微信首页 → 「我」tab → 「服务」 → 「收付款」
var wechatQrTapSeq = []wxNavPt{
	{945, 2300}, // 「我」tab
	{540, 700},  // 「服务」
	{300, 490},  // 「收付款」绿横幅
}

// wechatQrTapService 「服务」步——该步偶发 PINNED 固定态下 tap 失效，需解锁点击再固定
var wechatQrTapService = wechatQrTapSeq[1]

// tapNav 点击导航点：手机参考系坐标 → TapRef 自动缩放到实际生效分辨率。
func tapNav(ctx context.Context, adb *adbmgr.Manager, serial string, p wxNavPt) error {
	return adb.TapRef(ctx, serial, p.X, p.Y)
}

// wechatQrTapDelay 每步点击后的等待。微信页面切换动画 + adb 通道网络延迟 + 设备
// 性能波动非线性（2026-08-28 实测），必须给足余量；3s 与支付宝关键步骤一致。
const wechatQrTapDelay = 3000 * time.Millisecond

// wechatQrEntry 收付款总页内「二维码收款」入口（手机 632 下半屏，实测 (540,1830)，
// 该格下方为「面对面红包」，上方为转账等格）
var wechatQrEntry = wxNavPt{540, 1830}

// wechatQrPersonalTab 收款码页内「个人收款码」tab（手机参考 (270,335)）
var wechatQrPersonalTab = wxNavPt{270, 335}

// wechatQrSaveBtn 「个人收款码」页「保存收款码」按钮（手机 OCR 中心 (483,1315)，
// 点击后已实测落图 mm_facetoface_collect_qrcode_<ts>.png）
var wechatQrSaveBtn = wxNavPt{483, 1315}

// WechatQrDir 微信收款码图片落盘目录（主空间 user 0；双开空间路径由调用方拼接 user id）
const WechatQrDir = "/storage/emulated/0/Pictures/WeiXin"

// WechatQrPrefix 微信收款码图片文件名前缀（保存收款码写入）
const WechatQrPrefix = "mm_facetoface_collect_qrcode_"

// CaptureWechatQrcode 核心：导航到微信个人收款码页 → 点「保存收款码」→ 从相册拉取最新图片 → 解码。
// 返回 图片本地路径（调用方负责 os.Remove）+ 文件名 + 解码内容。
// userId>0 时以该系统用户启动微信（双开/分身空间），相册路径同步切换。
func CaptureWechatQrcode(ctx context.Context, adb *adbmgr.Manager, serial string, userId int) (localPath, fileName, content string, err error) {
	// 1. 唤醒屏幕 + 强制重启微信到干净首页（指定双开 user；force-stop 避免旧页面残留）
	if err = wakeHome(ctx, adb, serial); err != nil {
		return "", "", "", fmt.Errorf("唤醒屏幕失败: %w", err)
	}
	// 显式 LauncherUI activity：MAIN/LAUNCHER 类别对双开 user(999) 实测启动不可靠
	if err = adb.RestartAppUser(ctx, serial, "com.tencent.mm", ".ui.LauncherUI", userId); err != nil {
		return "", "", "", fmt.Errorf("启动微信失败: %w", err)
	}
	time.Sleep(5 * time.Second) // 微信冷启动 + 首页加载（低端机/网络慢时需更久）

	// 前台守卫：微信未到前台绝不开始盲点坐标（重启失败时坐标会落进桌面/其他 app）
	if err = awaitForeground(ctx, adb, serial, "com.tencent.mm", foregroundWait); err != nil {
		return "", "", "", fmt.Errorf("启动微信失败: %w", err)
	}

	// 2. 依次点击导航到收付款总页
	for i, step := range wechatQrTapSeq {
		if err = tapNav(ctx, adb, serial, step); err != nil {
			return "", "", "", fmt.Errorf("点击第 %d 步失败: %w", i+1, err)
		}
		time.Sleep(wechatQrTapDelay)
	}
	time.Sleep(500 * time.Millisecond)

	// 3. 收付款总页 → 点「二维码收款」进入收款码页（漏此步停留总页）
	if err = tapNav(ctx, adb, serial, wechatQrEntry); err != nil {
		return "", "", "", fmt.Errorf("点击二维码收款入口失败: %w", err)
	}
	time.Sleep(wechatQrTapDelay)
	// 必须确认已进入收款码页（WalletCollectQrCodeUI）；否则坐标导航已偏移，后续点击全落空
	if err = requireWechatCollectPage(ctx, adb, serial); err != nil {
		return "", "", "", err
	}

	// 4. 直接点「保存收款码」→ 检查相册是否新增（商家/经营态该位置无按钮不会误点，个人态点击即保存）。
	//    相册未新增 → 说明在经营态 → 点「个人收款码」tab 切回个人态 → 再点保存。
	dir := WechatQrDir
	if userId > 0 {
		dir = fmt.Sprintf("/storage/emulated/%d/Pictures/WeiXin", userId)
	}
	// 相册基线：点击前已有的收款码图（防止把旧图当成新保存的）
	base := listWechatQrFiles(ctx, adb, serial, dir)

	// trySave 点「保存收款码」并轮询等待相册新增（落盘延迟在慢网络/低端机上可达 10s+，固定 sleep 不可靠）
	trySave := func() bool {
		if terr := tapNav(ctx, adb, serial, wechatQrSaveBtn); terr != nil {
			return false
		}
		return pollForNewFile(ctx, base, 20*time.Second, func() []string {
			return listWechatQrFiles(ctx, adb, serial, dir)
		})
	}
	// 第一次保存失败 → 说明不在个人态（如经营收款码态无保存按钮）。
	// 循环「点个人收款码 tab → 再点保存」最多 3 轮：切态点击可能被吞/未生效，单次补救不稳定。
	if !trySave() {
		for round := 1; round <= 3; round++ {
			if err = tapNav(ctx, adb, serial, wechatQrPersonalTab); err != nil {
				return "", "", "", fmt.Errorf("点击个人收款码 tab 失败: %w", err)
			}
			time.Sleep(wechatQrTapDelay)
			if trySave() {
				break
			}
			if round == 3 {
				return "", "", "", fmt.Errorf("多轮切回个人态后保存收款码仍失败（可能不在收款码页或按钮位置偏移）")
			}
		}
	}

	// 5. 拉取相册最新微信收款码图片（按文件名倒序取第一张）
	files, lerr := adb.ListFiles(ctx, serial, dir)
	if lerr != nil {
		return "", "", "", fmt.Errorf("读取相册失败: %w", lerr)
	}
	latest := ""
	for _, f := range files {
		if strings.HasPrefix(f, WechatQrPrefix) && strings.HasSuffix(f, ".png") {
			latest = f
			break
		}
	}
	if latest == "" {
		return "", "", "", fmt.Errorf("相册中未找到微信收款码图片（保存收款码未落盘）")
	}
	local, perr := adb.PullFile(ctx, serial, dir+"/"+latest)
	if perr != nil {
		return "", "", "", fmt.Errorf("拉取收款码图片失败: %w", perr)
	}
	imgData, rerr := os.ReadFile(local)
	if rerr != nil {
		os.Remove(local)
		return "", "", "", fmt.Errorf("读取图片失败: %w", rerr)
	}
	content, derr := DecodeQRContent(imgData)
	if derr != nil {
		os.Remove(local)
		return "", "", "", fmt.Errorf("收款码图片未识别到二维码: %w", derr)
	}
	return local, latest, content, nil
}

// RequireWechatCollectPage 确认微信前台已到达收款码页（WalletCollectQrCodeUI）。
// 带重试（页面切换动画 + 加载），最多尝试 5 次×1.5s。用于导航步骤间的页面就绪校验，
// 防止坐标偏移后盲目继续点击。
func requireWechatCollectPage(ctx context.Context, adb *adbmgr.Manager, serial string) error {
	for i := 0; i < 5; i++ {
		fg, err := adb.ForegroundApp(ctx, serial)
		// 双开 user 前台字符串形如 "u999 com.tencent.mm/...activity"，用 Contains 匹配
		if err == nil && strings.Contains(fg, "com.tencent.mm/") {
			if strings.Contains(fg, "WalletCollectQrCodeUI") {
				return nil
			}
			// 前台仍是微信但不是收款页：再等一次（可能动画中）
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(1500 * time.Millisecond):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	return fmt.Errorf("未进入微信收款码页（当前前台 %q），坐标导航可能已偏移", func() string {
		fg, _ := adb.ForegroundApp(ctx, serial)
		return fg
	}())
}

// CleanupWechatGallery 清理指定 user 空间微信相册里的旧收款码图（保留最新 QrcodeKeepOnDevice 张）。
func CleanupWechatGallery(ctx context.Context, adb *adbmgr.Manager, serial string, userId int) (deleted int) {
	dir := WechatQrDir
	if userId > 0 {
		dir = fmt.Sprintf("/storage/emulated/%d/Pictures/WeiXin", userId)
	}
	files, err := adb.ListFiles(ctx, serial, dir)
	if err != nil {
		return 0 // 目录不存在/不可读 → 无码可清
	}
	var qrFiles []string
	for _, f := range files {
		if strings.HasPrefix(f, WechatQrPrefix) && strings.HasSuffix(f, ".png") {
			qrFiles = append(qrFiles, f)
		}
	}
	if len(qrFiles) <= QrcodeKeepOnDevice {
		return 0
	}
	for _, f := range qrFiles[QrcodeKeepOnDevice:] { // ListFiles 按 -t 倒序，越靠后越旧
		if err := adb.Rm(ctx, serial, dir+"/"+f); err != nil {
			continue
		}
		deleted++
	}
	return deleted
}

// listWechatQrFiles 列出微信收款码图片文件名（mm_facetoface_collect_qrcode_*.png，按 -t 倒序）。
func listWechatQrFiles(ctx context.Context, adb *adbmgr.Manager, serial, dir string) []string {
	files, err := adb.ListFiles(ctx, serial, dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f, WechatQrPrefix) && strings.HasSuffix(f, ".png") {
			out = append(out, f)
		}
	}
	return out
}

// 微信账本页冷启动导航坐标（PLAN-ADB-LEDGER.md §2.2 实测）：
//   - wechatLedgerEntry   收款码页(WalletCollectQrCodeUI)内「收款小账本」整行按钮
//     （手机 x 居中 (540,1745)，整行可点区域以 x=540 居中点击最可靠，与「服务」步同理）。
//     点击后进入收款小账本
//   - wechatLedgerRecords 小账本内「收款记录」按钮（统计卡片内蓝色小按钮，手机 y≈748），
//     点击后进入收款记录页(AppBrandUI00 小程序容器)
var wechatLedgerEntry = wxNavPt{540, 1745}
var wechatLedgerRecords = wxNavPt{540, 748}

// CaptureWechatLedger 冷启动导航到微信稳定的收款记录页（AppBrandUI00 小程序容器）。
// 链路：复用 CaptureWechatQrcode 前半段（首页→我→服务→收付款→二维码收款页）
// → 收款码页点「收款小账本」→ 小账本点「收款记录」→ 进入收款记录页。
// 供预热阶段调用；下单实时回调只走 task lock 热启动（EnsureForeground）。
// userId>0 时以该 user 启动（双开/分身空间）。
func CaptureWechatLedger(ctx context.Context, adb *adbmgr.Manager, serial string, userId int) error {
	if err := wakeHome(ctx, adb, serial); err != nil {
		return fmt.Errorf("唤醒屏幕失败: %w", err)
	}
	if err := adb.RestartAppUser(ctx, serial, "com.tencent.mm", ".ui.LauncherUI", userId); err != nil {
		return fmt.Errorf("启动微信失败: %w", err)
	}
	time.Sleep(5 * time.Second) // 冷启动 + 首页加载

	// 前台守卫（同 CaptureWechatQrcode）：微信未到前台绝不开始盲点坐标
	if err := awaitForeground(ctx, adb, serial, "com.tencent.mm", foregroundWait); err != nil {
		return fmt.Errorf("启动微信失败: %w", err)
	}

	// 屏幕固定保险：导航期间其他 app 无法抢占前台（同 CaptureAlipayLedger）
	if taskID, perr := adb.PinCurrentTask(ctx, serial); perr == nil {
		defer func() { _ = adb.UnpinTask(ctx, serial) }()
		_ = taskID
	} else {
		logger.Warn("capture", "屏幕固定失败（导航将无保护执行，可能被其他 app 打断）",
			"err", perr, "serial", serial)
	}

	// 微信首页 → 我 → 服务 → 收付款（复用收款码导航坐标序列）
	for _, p := range wechatQrTapSeq {
		var err error
		if p == wechatQrTapService {
			// 「服务」步偶发 PINNED 固定态下 tap 失效（2026-08-28 实测）：
			// 先释放固定 → 点击 → 重新固定（与支付宝总览补点同 bug 处理）
			err = tapPinned(ctx, adb, serial, p.X, p.Y)
		} else {
			err = adb.TapRef(ctx, serial, p.X, p.Y)
		}
		if err != nil {
			return fmt.Errorf("微信导航点击失败: %w", err)
		}
		time.Sleep(wechatQrTapDelay)
	}
	// 收付款 → 二维码收款（进入收款码页，PINNED 固定态下 tap 偶发失效——先解锁再点）。
	if err := tapPinned(ctx, adb, serial, wechatQrEntry.X, wechatQrEntry.Y); err != nil {
		return fmt.Errorf("点击二维码收款失败: %w", err)
	}
	if err := requireWechatCollectPage(ctx, adb, serial); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond)

	// 收款码页（WalletCollectQrCodeUI）→ 收款小账本 → 收款记录 → 收款记录页(AppBrandUI00)。
	// 关键（2026-08-29 真机验证）：PINNED 固定态会**拦截小程序新 task 启动**——「收款小账本」
	// 是独立小程序容器（新 task t22284），固定时点击无响应/被拦。必须在此**解除固定**，
	// 后续点击全部在解锁态执行（不再重新固定）。
	if err := adb.UnpinTask(ctx, serial); err != nil {
		logger.Warn("capture", "微信导航：进入收款码页后释放固定失败（继续尝试点击）",
			"err", err, "serial", serial)
	}
	time.Sleep(500 * time.Millisecond)

	// 收款码页默认可能是「经营收款码」态（约 50%，页面布局不同「收款小账本」入口点击无效）
	// 或「个人收款码」态（入口可点）。不可依赖单次点击——每次点「收款小账本」后用前台
	// 是否进入小账本容器（AppBrandUI00）校验：进入=个人态成功；未进=经营态点不动，
	// 点「个人收款码」tab 切回个人态再续试。每轮失败都重新点 tab 兜底（对齐
	// CaptureWechatQrcode 的多轮切态机制），最多 3 轮内必达个人态（与实测一致）。
	const ledgerRounds = 3
	var lastErr error
	for round := 0; round < ledgerRounds; round++ {
		if round > 0 {
			// 非首次：上一轮未进入账本容器 → 疑经营态，点「个人收款码」tab 切回个人态
			if err := tapNav(ctx, adb, serial, wechatQrPersonalTab); err != nil {
				return fmt.Errorf("点击个人收款码 tab 失败: %w", err)
			}
			time.Sleep(wechatQrTapDelay) // 切态动画 + 页面重载
		}
		// 点「收款小账本」→ 校验是否进入容器（个人态成功标准）
		if err := tapNav(ctx, adb, serial, wechatLedgerEntry); err != nil {
			return fmt.Errorf("点击收款小账本失败: %w", err)
		}
		time.Sleep(3000 * time.Millisecond) // 小账本加载（小程序冷启动较慢）
		if err := requireWechatLedgerPage(ctx, adb, serial); err != nil {
			lastErr = err
			continue // 未进入容器 → 下轮切个人态再试
		}
		// 已进入小账本容器 → 点「收款记录」进账本页
		if err := tapNav(ctx, adb, serial, wechatLedgerRecords); err != nil {
			return fmt.Errorf("点击收款记录失败: %w", err)
		}
		time.Sleep(3000 * time.Millisecond)
		if err := requireWechatLedgerPage(ctx, adb, serial); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("进入微信收款记录页失败（含切个人收款码态重试）: %w", lastErr)
}

// tapPinned PINNED 固定态下的安全点击：先解锁（UnpinTask）→ 点击 → 重新固定。
// PINNED 固定态下 input tap 偶发失效（2026-08-28 实测微信「服务」步/支付宝总览界面），
// 解锁后同坐标点击立即生效。重新固定失败不致命。微信/支付宝导航通用。
func tapPinned(ctx context.Context, adb *adbmgr.Manager, serial string, x, y int) error {
	if err := adb.UnpinTask(ctx, serial); err != nil {
		logger.Warn("capture", "释放屏幕固定失败（继续尝试点击）",
			"err", err, "serial", serial)
	}
	time.Sleep(300 * time.Millisecond)
	if err := adb.TapRef(ctx, serial, x, y); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := adb.PinCurrentTask(ctx, serial); err != nil {
		logger.Warn("capture", "重新固定屏幕失败（导航将无保护执行）",
			"err", err, "serial", serial)
	}
	return nil
}

// requireWechatLedgerPage 确认微信前台已到达收款记录页（AppBrandUI00 小程序容器）。
// 带重试（小程序页面加载），最多尝试 6 次×1.5s。
func requireWechatLedgerPage(ctx context.Context, adb *adbmgr.Manager, serial string) error {
	for i := 0; i < 6; i++ {
		fg, err := adb.ForegroundApp(ctx, serial)
		if err == nil && strings.Contains(fg, "AppBrandUI00") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
	fg, _ := adb.ForegroundApp(ctx, serial)
	return fmt.Errorf("未进入微信收款记录页（当前前台 %q）", fg)
}
