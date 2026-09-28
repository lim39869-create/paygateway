// 支付宝最近任务锁定（MIUI/HyperOS 防一键清理）。
//
// MIUI 的"锁定最近任务"是纯客户端 UI 特性：状态存于 Recents 进程内存，
// 无广播/binder/shell 命令（am task lock 是 kiosk 模式，非此机制）。
// 唯一稳定可自动化路径 = uiautomator 定位任务卡片 → 长按 → 点「锁定任务」。
// 2026-08-28 真机实测完整闭环：加锁/解锁可逆，一键清理后锁定任务保留。
package capture

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paygateway/internal/adbmgr"
	"paygateway/internal/logger"
)

// recentsActivity MIUI/HyperOS 最近任务界面组件（am start 直达，比 KEYCODE_APP_SWITCH
// 稳定——后者在部分机型会打开桌面多屏视图而非最近任务）。
const recentsActivity = "com.miui.home/.recents.RecentsActivity"

// lockTaskBtnText 长按任务卡片弹出菜单中的按钮文本（国内版 MIUI 用 TextView，无 resource id）。
// 已锁定任务的菜单显示「解除锁定」，未锁定显示「锁定任务」，二者可区分。
const lockTaskBtnText = "锁定任务"

// LockPaymentTasks 锁定最近任务中指定支付应用的主任务卡片（每应用最多锁一张），
// 防止一键清理移除任务栈。一个应用锁定它的主进程任务栈即可——锁定多个同应用进程
// 会触发 MIUI 保护性杀进程（系统判定异常）。
// 流程：直达 RecentsActivity → dump 定位未加锁的匹配应用卡片 → 逐张长按→点锁定。
// 锁定状态存于内存（设备重启失效），调用方应在每次预热/恢复账本页后调用。
func LockPaymentTasks(ctx context.Context, adb *adbmgr.Manager, serial string, appNames ...string) (int, error) {
	locked := 0
	// 最多 3 轮：每轮锁一张未加锁卡片，锁定后列表刷新，重进最近任务避免坐标漂移
	for attempt := 0; attempt < 3; attempt++ {
		unlocked, err := findUnlockedCards(ctx, adb, serial, appNames)
		if err != nil {
			return locked, err
		}
		if len(unlocked) == 0 {
			return locked, nil
		}
		// 只锁第一张未加锁的卡片（每应用只锁主进程任务栈，足够防清理）
		if err := lockOneCard(ctx, adb, serial, unlocked[0]); err != nil {
			logger.Warn("capture", "锁定支付应用任务卡片失败",
				"err", err, "serial", serial)
			continue
		}
		locked++
		// 重新进最近任务列表以避免坐标漂移
		_, _ = adb.Shell(ctx, 3*time.Second, serial, "input keyevent KEYCODE_BACK")
		time.Sleep(500 * time.Millisecond)
	}
	return locked, nil
}

// findCards 进入最近任务并返回所有匹配应用名卡片的 bounds（无论锁定状态）。
func findCards(ctx context.Context, adb *adbmgr.Manager, serial string, appNames []string) ([]string, error) {
	if _, err := adb.Shell(ctx, 8*time.Second, serial, "am start -n "+recentsActivity); err != nil {
		return nil, fmt.Errorf("打开最近任务失败: %w", err)
	}
	time.Sleep(2500 * time.Millisecond)
	xmlText, err := adb.DumpUI(ctx, serial)
	if err != nil {
		return nil, fmt.Errorf("dump 最近任务失败: %w", err)
	}
	root, err := parseUINodeTree(xmlText)
	if err != nil {
		return nil, fmt.Errorf("解析最近任务 UI 失败: %w", err)
	}
	var out []string
	walkUINodes(root, func(n *uiNode) {
		if matchAnyName(n.Desc, appNames) && n.Bounds != "" {
			out = append(out, n.Bounds)
		}
	})
	return out, nil
}

// matchAnyName 判断 content-desc 是否包含任一应用名（如"支付宝,未加锁"匹配"支付宝"）。
func matchAnyName(desc string, appNames []string) bool {
	for _, name := range appNames {
		if strings.Contains(desc, name) {
			return true
		}
	}
	return false
}

// findUnlockedCards 进入最近任务并返回所有未加锁的匹配应用名卡片的 bounds。
func findUnlockedCards(ctx context.Context, adb *adbmgr.Manager, serial string, appNames []string) ([]string, error) {
	if _, err := adb.Shell(ctx, 8*time.Second, serial, "am start -n "+recentsActivity); err != nil {
		return nil, fmt.Errorf("打开最近任务失败: %w", err)
	}
	time.Sleep(2500 * time.Millisecond)
	xmlText, err := adb.DumpUI(ctx, serial)
	if err != nil {
		return nil, fmt.Errorf("dump 最近任务失败: %w", err)
	}
	root, err := parseUINodeTree(xmlText)
	if err != nil {
		return nil, fmt.Errorf("解析最近任务 UI 失败: %w", err)
	}
	var out []string
	walkUINodes(root, func(n *uiNode) {
		// 任务卡片 content-desc 形如 "支付宝,未加锁"（"打开支付宝应用信息。"等
		// 信息按钮也含应用名但无锁定状态后缀，必须排除，否则长按会点到按钮）
		if matchAnyName(n.Desc, appNames) && strings.Contains(n.Desc, "未加锁") && n.Bounds != "" {
			out = append(out, n.Bounds)
		}
	})
	return out, nil
}

// walkUINodes 深度优先遍历 UI 节点树，对每个节点调用 fn。
func walkUINodes(n *uiNode, fn func(*uiNode)) {
	if n == nil {
		return
	}
	fn(n)
	for _, k := range n.Kids {
		walkUINodes(k, fn)
	}
}

// lockOneCard 长按任务卡片 → 点「锁定任务」。bounds 形如 "[l,t][r,b]"。
func lockOneCard(ctx context.Context, adb *adbmgr.Manager, serial, bounds string) error {
	cx, cy, err := parseBoundsCenter(bounds)
	if err != nil {
		return err
	}
	// 长按 = swipe 同点 + 长 duration（input tap 无法表达长按，实测 swipe 900ms 有效）
	if err := adb.Swipe(ctx, serial, cx, cy, cx, cy, 900); err != nil {
		return fmt.Errorf("长按任务卡片失败: %w", err)
	}
	time.Sleep(1500 * time.Millisecond)
	xmlText, err := adb.DumpUI(ctx, serial)
	if err != nil {
		return fmt.Errorf("dump 锁定菜单失败: %w", err)
	}
	btn, err := findButtonBounds(xmlText, lockTaskBtnText)
	if err != nil {
		return err
	}
	if btn == "" {
		return fmt.Errorf("未找到锁定按钮（长按菜单未弹出，HyperOS 已知卡死问题）")
	}
	bx, by, err := parseBoundsCenter(btn)
	if err != nil {
		return err
	}
	if err := adb.Tap(ctx, serial, bx, by); err != nil {
		return fmt.Errorf("点击锁定任务失败: %w", err)
	}
	time.Sleep(1500 * time.Millisecond)
	return nil
}

// findButtonBounds 在 dump XML 中找匹配按钮的 bounds。
// 菜单按钮可能是 TextView（text 属性）或 ImageView（content-desc 属性，
// 如 "锁定任务" 是 com.miui.home:id/menu_item_lock ImageView），需两者都匹配。
func findButtonBounds(xmlText, want string) (string, error) {
	root, err := parseUINodeTree(xmlText)
	if err != nil {
		return "", err
	}
	var found string
	walkUINodes(root, func(n *uiNode) {
		if found == "" && n.Bounds != "" && (strings.Contains(n.Text, want) || strings.Contains(n.Desc, want)) {
			found = n.Bounds
		}
	})
	return found, nil
}

// parseBoundsCenter 解析 "[l,t][r,b]" 为中心坐标。
func parseBoundsCenter(bounds string) (x, y int, err error) {
	var l, t, r, b int
	if _, err := fmt.Sscanf(bounds, "[%d,%d][%d,%d]", &l, &t, &r, &b); err != nil {
		return 0, 0, fmt.Errorf("解析 bounds %q 失败: %w", bounds, err)
	}
	return (l + r) / 2, (t + b) / 2, nil
}
