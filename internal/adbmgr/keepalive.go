package adbmgr

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paygateway/internal/logger"
)

// keepalive 保活命令集：为支付通道 app（微信/支付宝）配置 Android/MIUI 后台存活保障。
//
// 背景（2026-08-30 红米k60/MIUI14 实测）：
//   - 网关通过 adb 控制真机上的微信/支付宝读取账本/出收款码，账本页必须常驻后台；
//   - MIUI 是"杀后台大户"（省电策略/墓碑冻结），但标准 adb 命令可覆盖其中一部分；
//   - 本文件只封装"实测有效"的命令，避免给运维虚假安全感。
//
// 实测有效的命令（全部无 root）：
//   - deviceidle whitelist +<pkg>        加入电池优化白名单（dumpsys deviceidle 验证通过）
//   - appops RUN_IN_BACKGROUND allow     允许后台运行
//   - appops RUN_ANY_IN_BACKGROUND allow 允许任意后台运行（实测微信/支付宝均已生效）
//
// 实测无效 / 被 MIUI 接管（不封装，避免误导）：
//   - am set-standby-bucket active       返回 bucket=5（restricted），MIUI 覆盖标准机制
//   - appops MIUIOP:1000x set            Unknown operation string（MIUI 私有 op 不暴露 shell）
//   - BOOT_COMPLETED / FOREGROUND_SERVICE appops key（Android 13 无此 op 名）
//   - MIUI whetstone 冻结豁免（miui.whetstone.power 服务无 shell 入口，需 UI 设"无限制"省电）
//
// 补充：锁任务（LockPaymentTasks，见 alipay_lock.go）用 uiautomator 长按任务卡片实现，
// 是一键清理后仍保留的关键手段，与本命令集互补。
// 局限：锁任务/白名单都防不住 MIUI 墓碑冻结（进程冻结后热唤醒回主进程）——这是 UI 层
// "省电策略→无限制"才能解决的（无 root 无 adb 入口），cached_apps_freezer 只关掉原生层。

// 支付通道应用包名（保活目标；与 ledgermon reader 各自声明的常量一致）
const (
	WechatPkg = "com.tencent.mm"
	AlipayPkg = "com.eg.android.AlipayGphone"
)

// KeepAliveApply 对单个包应用全部实测有效的保活命令。
// 返回实际应用了哪些命令（供日志/展示）。幂等：重复调用无害。
func (m *Manager) KeepAliveApply(ctx context.Context, serial, pkg string) ([]string, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return nil, err
	}
	applied := make([]string, 0, 3)

	// 1. 电池优化白名单（deviceidle）
	if _, err := m.adb(ctx, 10*time.Second, serial, "shell", "dumpsys", "deviceidle", "whitelist", "+"+pkg); err != nil {
		return applied, fmt.Errorf("deviceidle 白名单 %s: %w", pkg, err)
	}
	applied = append(applied, "deviceidle-whitelist")

	// 2. 允许后台运行
	for _, op := range []string{"RUN_IN_BACKGROUND", "RUN_ANY_IN_BACKGROUND"} {
		if _, err := m.adb(ctx, 10*time.Second, serial, "shell", "cmd", "appops", "set", pkg, op, "allow"); err != nil {
			return applied, fmt.Errorf("appops %s %s: %w", pkg, op, err)
		}
		applied = append(applied, "appops-"+strings.ToLower(op))
	}

	logger.Info("adbmgr", "已应用保活命令", "serial", serial, "pkg", pkg, "applied", strings.Join(applied, ","))
	return applied, nil
}

// KeepAliveDeviceFreezer 禁用设备级 Cached Apps Freezer（AOSP 缓存应用冻结器）。
// Android 12+ 系统会在内存压力下冻结后台缓存进程（isFrozen=true），冻结进程热唤醒
// 可能丢任务栈状态。实测设置后 use_freezer=false（生效）。设备级设置，幂等。
func (m *Manager) KeepAliveDeviceFreezer(ctx context.Context, serial string) error {
	if err := m.Connect(ctx, serial); err != nil {
		return err
	}
	if _, err := m.adb(ctx, 10*time.Second, serial, "shell", "settings", "put", "global", "cached_apps_freezer", "disabled"); err != nil {
		return fmt.Errorf("禁用 cached_apps_freezer: %w", err)
	}
	logger.Info("adbmgr", "已禁用设备缓存应用冻结器", "serial", serial)
	return nil
}

// KeepAliveStatus 查询某包当前的保活状态（供运维/前端展示，只读）。
// 返回 map：deviceidle 白名单 / RUN_IN_BACKGROUND / RUN_ANY_IN_BACKGROUND 的 allow 状态。
func (m *Manager) KeepAliveStatus(ctx context.Context, serial, pkg string) (map[string]bool, error) {
	if err := m.Connect(ctx, serial); err != nil {
		return nil, err
	}
	st := map[string]bool{}

	// deviceidle 白名单：dumpsys deviceidle whitelist 含 "user,<pkg>,<uid>" 行即已加入
	out, err := m.adb(ctx, 10*time.Second, serial, "shell", "dumpsys", "deviceidle", "whitelist")
	if err == nil {
		st["deviceidle"] = strings.Contains(out, "user,"+pkg+",")
	}

	// appops 状态：RUN_IN_BACKGROUND / RUN_ANY_IN_BACKGROUND 行含 ": allow"
	out, err = m.adb(ctx, 10*time.Second, serial, "shell", "cmd", "appops", "get", pkg)
	if err == nil {
		for _, op := range []string{"RUN_IN_BACKGROUND", "RUN_ANY_IN_BACKGROUND"} {
			st[strings.ToLower(op)] = opLineAllow(out, op)
		}
	}
	return st, nil
}

// opLineAllow 在 appops get 输出中查找 "<op>: allow" 行。
func opLineAllow(out, op string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, op+":") {
			return strings.Contains(line, "allow")
		}
	}
	return false
}
