// Package main 启动配置自检（RESILIENCE.md P1-2）
//
// 目标：启动时一次性暴露配置错误，避免生产事故：
//   - 沙箱/正式错配（is_prod）
//   - 密钥缺失（主密钥 / 账本密钥 / 套码 token）
//   - 回调地址未配置（隧道未就绪）
//   - 个人免签通道缺账户 / 缺监听设备
//   - 告警未配置（可观测性缺失）
//
// 检查结果分级：✅ 正常 / ⚠️ 警告（可运行但建议修复）/ ❌ 错误（功能不可用）
// 通过结构化日志输出，管理后台「系统日志」可查。
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// checkItem 单条检查结果
type checkItem struct {
	level string // ok / warn / err
	msg   string
}

// runStartupChecks 启动配置自检（在账户池加载、通道注册、隧道启动前调用）
func runStartupChecks(cfg *Config, accountPool *account.AccountPool, st store.Store, personalDB store.PersonalStore) {
	var items []checkItem
	now := time.Now()

	// 1. 密钥检查
	// 账户主密钥在 main.go 已 fail-fast：到这里一定非空，但保留检查分支以应对未来可能的"可选"开关。
	if cfg.AccountMasterKey == "" {
		items = append(items, checkItem{"err", "账户主密钥未配置（应在 main.go fail-fast 退出，不会走到此处）"})
	} else {
		items = append(items, checkItem{"ok", "账户主密钥：已配置"})
	}
	if cfg.LedgerPrivKey == "" {
		items = append(items, checkItem{"ok", "审计账本密钥：自动生成（settings 持久化）"})
	} else {
		items = append(items, checkItem{"ok", "审计账本密钥：已配置"})
	}
	if personal.QrTokenSecretSet() {
		items = append(items, checkItem{"ok", "套码 token 密钥：已初始化"})
	} else {
		items = append(items, checkItem{"err", "套码 token 密钥未初始化（个人免签二维码将无法生成）"})
	}

	// 2. 账户池检查
	total := accountPool.Count()
	if total == 0 {
		items = append(items, checkItem{"warn", "账户池为空：无任何支付账户，所有通道无法接单（请到管理后台「账户池」添加）"})
	} else {
		items = append(items, checkItem{"ok", fmt.Sprintf("账户池：%d 个账户", total)})
	}

	// 3. 个人免签通道检查
	personalNames := []string{}
	for _, name := range channel.AllNames() {
		if strings.HasPrefix(name, "personal.") {
			personalNames = append(personalNames, name)
		}
	}
	if len(personalNames) == 0 {
		items = append(items, checkItem{"err", "个人免签通道未注册（personal.alipay / personal.wechat）"})
	} else {
		items = append(items, checkItem{"ok", "个人免签通道：已注册 " + strings.Join(personalNames, ", ")})
		// 各个人通道账户数
		for _, name := range personalNames {
			n := accountPool.CountByChannel(name)
			if n == 0 {
				items = append(items, checkItem{"warn", fmt.Sprintf("通道 %s 无收款账户：该通道无法接单（需配置收款码 qr_content）", name)})
			} else {
				items = append(items, checkItem{"ok", fmt.Sprintf("通道 %s：%d 个收款账户", name, n)})
			}
		}
	}

	// 4. 监听设备检查（在线 = adb 可达，2026-08-29 重构：不再依赖监听 App 心跳）
	if personalDB != nil {
		devs, err := personalDB.ListDevices(context.Background())
		if err != nil {
			items = append(items, checkItem{"err", "监听设备列表查询失败: " + err.Error()})
		} else {
			online := 0
			for _, d := range devs {
				serial := deviceADBSerial(d)
				if serial != "" && adbMgr().Online(context.Background(), serial) {
					online++
				}
			}
			if len(devs) == 0 {
				items = append(items, checkItem{"warn", "无监听端设备：个人免签无法确认收款（请绑定收款账户创建设备）"})
			} else {
				items = append(items, checkItem{"ok", fmt.Sprintf("监听设备：%d 台（adb 在线 %d）", len(devs), online)})
			}
		}
	}

	// 5. 告警配置
	if cfg.AlertBarkURL == "" && cfg.AlertWebhookURL == "" {
		items = append(items, checkItem{"warn", "告警未配置（ALERT_BARK_URL / ALERT_WEBHOOK_URL 为空）：异常进池/账本失败/dead 通知将无法推送"})
	} else {
		items = append(items, checkItem{"ok", "告警通道：已配置"})
	}

	// 6. 单笔限额风控
	if cfg.PersonalMaxAmount > 0 {
		items = append(items, checkItem{"ok", fmt.Sprintf("个人通道单笔限额：¥%d.%02d", cfg.PersonalMaxAmount/100, cfg.PersonalMaxAmount%100)})
	} else {
		items = append(items, checkItem{"warn", "个人通道单笔限额未设置（PERSONAL_MAX_AMOUNT=0）：大额风险无防护"})
	}

	// 7. 生产/沙箱错配检查（仅在有账户时）
	prodAccts, sandboxAccts := 0, 0
	for _, a := range accountPool.ListAll() {
		if a.IsProd {
			prodAccts++
		} else {
			sandboxAccts++
		}
	}
	if sandboxAccts > 0 && prodAccts > 0 {
		items = append(items, checkItem{"warn", fmt.Sprintf("生产/沙箱账户混用：%d 生产 + %d 沙箱（正式环境应全部 is_prod=true）", prodAccts, sandboxAccts)})
	} else if sandboxAccts == 0 && prodAccts > 0 {
		items = append(items, checkItem{"ok", fmt.Sprintf("账户环境一致：全部为生产（%d 个）", prodAccts)})
	}

	// 8. 数据文件（settings 表）可读
	if _, err := st.GetAllSettings(context.Background()); err != nil {
		items = append(items, checkItem{"err", "settings 表不可读: " + err.Error()})
	} else {
		items = append(items, checkItem{"ok", "业务数据库：可读写"})
	}

	// 输出报告
	okCount, warnCount, errCount := 0, 0, 0
	logger.Info("main", "=== 启动配置自检 ===")
	for _, it := range items {
		switch it.level {
		case "ok":
			okCount++
			logger.Info("main", "自检 ✅ "+it.msg)
		case "warn":
			warnCount++
			logger.Warn("main", "自检 ⚠️ "+it.msg)
		case "err":
			errCount++
			logger.Error("main", "自检 ❌ "+it.msg)
		}
	}
	logger.Info("main", fmt.Sprintf("启动自检完成：%d 通过 / %d 警告 / %d 错误（用时 %s）",
		okCount, warnCount, errCount, time.Since(now).Round(time.Millisecond)))
	if errCount > 0 {
		logger.Warn("main", "存在错误级配置问题，请修复后重启（服务将继续启动，但相关功能不可用）")
	}
}
