// paygateway 支付网关 alloc（从 main.go 拆分，均衡分配算法）
package main

import (
	"context"
	"fmt"
	"paygateway/internal/account"
	"paygateway/internal/ledgermon"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"paygateway/internal/pool"
	"paygateway/internal/store"
	"time"
)

func decryptDeviceSecret(encHex string) (string, error) {
	if encHex == "" {
		return "", fmt.Errorf("设备密钥为空")
	}
	data, err := account.Decrypt(encHex)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// autoRegisterDevice 首连自动注册设备（极端情况：仅有固定域名 + 可访问公网即可接入）。
// 监听端首次启动未配置时自动生成 device_id + secret，心跳携带 register_secret 完成注册。
// 返回注册后的设备（含解密密钥）。
func (a *App) resolveLedgerDevices(ctx context.Context) ([]ledgermon.MonitoredDevice, error) {
	devs, err := a.personalDB.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	adb := adbMgr()
	var out []ledgermon.MonitoredDevice
	for _, d := range devs {
		serial := deviceADBSerial(d)
		if serial == "" {
			continue
		}
		// 实时 adb 可达判定（含自动 adb connect 重连；~1s，reconcile 30s 周期可接受）
		if !adb.Online(ctx, serial) {
			continue
		}
		for _, id := range store.DeviceAccountIDs(ctx, a.personalDB, d.ID, d.AccountID) {
			acct := a.accountPool.GetByID(id)
			if acct == nil {
				continue
			}
			provider, ok := ledgerProviderOf(acct.Channel)
			if !ok {
				continue // 非个人免签通道（沙箱/企业）无账本页可监听
			}
			out = append(out, ledgermon.MonitoredDevice{
				DeviceID:  d.DeviceID,
				Serial:    serial,
				Provider:  provider,
				AccountID: id,
				UserID:    ledgerUserSpaceOf(ctx, a.personalDB, d.DeviceID, provider, acct.QRContent),
			})
		}
	}
	return out, nil
}

// deviceADBSerial 解析设备 adb serial（USB host[:port] 或 WiFi host:port），不可达返回空串。
func deviceADBSerial(d *personal.DeviceEncrypted) string {
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

// accountDeviceOf 账户 → 设备 ID 映射（WAIT_PAY 池设备维度子池用）。
// 遍历全部设备找账户归属（主账户或附加账户）；一对多时取首个（业务上账户唯一绑定一台收款设备）。
// 返回 (deviceID, found)。低频（下单时调用一次），全量遍历可接受。
func accountDeviceOf(ctx context.Context, pdb store.PersonalStore, accountID int64) (string, bool) {
	devs, err := pdb.ListDevices(ctx)
	if err != nil {
		logger.Warn("main", "设备列表查询失败（订单归入兜底桶，Reconcile 兜底）", "err", err)
		return "", false
	}
	for _, d := range devs {
		for _, id := range store.DeviceAccountIDs(ctx, pdb, d.ID, d.AccountID) {
			if id == accountID {
				return d.DeviceID, true
			}
		}
	}
	return "", false
}

// devCandidate 均衡分配候选设备（含负载与健康度，供两层评分）
type devCandidate struct {
	deviceID      string
	serial        string
	load          int     // 设备 WAIT_PAY 待确认总数（设备负载）
	health        float64 // 设备 poll 成功率（0~1）
	acctIDs       []int64 // 设备绑定账户（主+附加）
	loadOverRatio bool    // 负载进入缓冲带（ratio×cap ≤ load < cap）：评分降权
}

// deviceLoadCapOf 计算设备订单承载上限：配置优先；0=按公式自动算。
// 公式：(MAX窗口 - 支付操作预留) / (单次poll × 每账户最低配额1) × 排队修正。
// 上限即"在用户可接受确认延迟内设备能扛的订单数"（PLAN-DYNAMIC-CLOSE-ALLOC §4.5）。
func deviceLoadCapOf(cfg *Config, dynMax time.Duration) int {
	if cfg.DeviceLoadCap > 0 {
		return cfg.DeviceLoadCap
	}
	// 与 main 的退化逻辑保持一致：动态窗口配置非法（MIN≤0/MAX≤0/MIN≥MAX）→ 静态窗口语义，不启用超限过滤
	if dynMax <= 0 || cfg.OrderCloseMin <= 0 || cfg.OrderCloseMax <= 0 || cfg.OrderCloseMin >= cfg.OrderCloseMax {
		return 0
	}
	payReserve := 60 * time.Second
	pollCost := time.Duration(cfg.DevicePollCostMS) * time.Millisecond
	if pollCost <= 0 {
		pollCost = 4 * time.Second
	}
	n := int((dynMax - payReserve) / (pollCost * 1))
	if n < 1 {
		n = 1
	}
	return n
}

// balanceSelectAccount 均衡分配：先选设备（最闲最健康），再选设备内账户（最闲最健康）。
// 设备评分 = health × 1/(load+1)；账户评分 = 健康 × 1/(pool.Count+1)。
// 设备超限（load ≥ cap）→ 排除；全部超限 → 返回 nil（下单层报错 + 告警）。
// channelName 形如 "personal.alipay"/"personal.wechat"（账户池通道过滤）。
func balanceSelectAccount(ctx context.Context, pdb store.PersonalStore,
	pool *account.AccountPool, wp *pool.WaitPayPool, lm *ledgermon.Manager,
	cfg *Config, channelName string) *account.Account {
	if wp == nil || pool == nil {
		return nil
	}
	devs, err := pdb.ListDevices(ctx)
	if err != nil {
		logger.Error("main", "均衡分配：设备列表查询失败", "err", err)
		return nil
	}

	// 健康度快照（ledgermon 未启动 → 全部 0.9 中性）
	health := map[string]float64{}
	if lm != nil {
		health = lm.DevicePollHealth()
	}

	// 收集在线设备候选（adb 可达）
	var cands []devCandidate
	for _, d := range devs {
		serial := deviceADBSerial(d)
		if serial == "" || !adbMgr().Online(ctx, serial) {
			continue
		}
		ids := store.DeviceAccountIDs(ctx, pdb, d.ID, d.AccountID)
		if len(ids) == 0 {
			continue
		}
		st := wp.DeviceSnapshot(d.DeviceID)
		load := 0
		if st != nil {
			load = st.Total
		}
		h := health[d.DeviceID]
		if h <= 0 {
			h = 0.9
		}
		cands = append(cands, devCandidate{deviceID: d.DeviceID, serial: serial, load: load, health: h, acctIDs: ids})
	}
	if len(cands) == 0 {
		return nil
	}

	// 设备层：缓冲带降权（load ≥ ratio×cap → 降权）→ 超限排除（load ≥ cap）→ 评分排序
	dynMax := time.Duration(cfg.OrderCloseMax) * time.Minute
	capN := deviceLoadCapOf(cfg, dynMax)
	softCap := 0
	if capN > 0 && cfg.DeviceOverloadRatio > 0 && cfg.DeviceOverloadRatio < 1 {
		softCap = int(float64(capN) * cfg.DeviceOverloadRatio)
	}
	usable := cands[:0]
	for _, c := range cands {
		if capN > 0 && c.load >= capN {
			logger.Warn("main", "均衡分配：设备负载超限，跳过",
				"device_id", c.deviceID, "load", c.load, "cap", capN)
			continue
		}
		if softCap > 0 && c.load >= softCap {
			c.loadOverRatio = true // 缓冲带：评分降权（最闲设备优先时让位）
		}
		usable = append(usable, c)
	}
	if len(usable) == 0 {
		logger.Error("main", "均衡分配：全部设备负载超限，建议增加收款设备",
			"loads", fmt.Sprintf("%+v", cands))
		return nil
	}
	// 设备评分 argmax（最闲最健康；缓冲带内降权；评分相等 → load 小优先，再按 deviceID 字典序确定性）
	best := usable[0]
	bestScore := deviceScore(best)
	for _, c := range usable[1:] {
		s := deviceScore(c)
		if s > bestScore || (s == bestScore && (c.load < best.load || (c.load == best.load && c.deviceID < best.deviceID))) {
			best, bestScore = c, s
		}
	}

	// 账户层：设备内选最闲最健康账户
	allowed := make(map[int64]bool, len(best.acctIDs))
	for _, id := range best.acctIDs {
		allowed[id] = true
	}
	return pool.SelectFromWeighted(channelName, "default", allowed,
		func(acct *account.Account) float64 {
			return accountScore(acct, wp)
		})
}

// deviceScore 设备评分：health × 1/(load+1)。
// 缓冲带内（loadOverRatio）额外 ×0.5 降权：让最闲设备优先，避免临界抖动。
func deviceScore(c devCandidate) float64 {
	s := c.health / float64(c.load+1)
	if c.loadOverRatio {
		s *= 0.5
	}
	return s
}

// accountScore 账户评分：健康（状态/日限额/失败）× 1/(账户负载+1)
// 只罚短板，不叠加长处（PLAN-DYNAMIC-CLOSE-ALLOC §5.3）
func accountScore(acct *account.Account, wp *pool.WaitPayPool) float64 {
	if acct == nil {
		return 0
	}
	health := 1.0
	if acct.DailyLimit > 0 {
		health *= 1 - float64(acct.DailyUsed)/float64(acct.DailyLimit) // 日限额余量
		if health < 0 {
			health = 0
		}
	}
	switch {
	case acct.FailCount >= 3:
		health *= 0.2
	case acct.FailCount >= 1:
		health *= 0.5
	}
	load := 1
	if wp != nil {
		load = wp.Count(acct.ID) + 1
	}
	return health / float64(load)
}

// deviceSerialFromDevice 解析设备 adb serial（内存态 Device 版本，镜像 deviceADBSerial 逻辑）。
// 探测链路用（dispatchLedgerRows 拿到的是 personalDeviceCtx 的 *personal.Device）。
