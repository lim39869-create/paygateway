// paygateway 支付网关入口：main() 装配 + App 容器。
// 配置/通道/路由/分配/账本匹配已拆分到同包文件：
//
//	config.go      配置加载与环境变量
//	channels.go    通道注册/设备管理/中间件
//	handlers_epay.go 易支付下单/回调/查单
//	handlers_qr.go   QR 系接口/指纹
//	alloc.go        均衡分配算法
//	devices.go      设备/渠道映射
//	ledger.go       账本监听/匹配引擎
//	notify.go       QR real/通知处理
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/admin"
	"paygateway/internal/alert"
	"paygateway/internal/channel"
	alipaych "paygateway/internal/channel/alipay"
	personalch "paygateway/internal/channel/personal"
	wechatch "paygateway/internal/channel/wechat"
	"paygateway/internal/ledger"
	"paygateway/internal/ledgermon"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/notify"
	"paygateway/internal/notifywatcher"
	"paygateway/internal/order"
	"paygateway/internal/personal"
	"paygateway/internal/pool"
	"paygateway/internal/ratelimit"
	"paygateway/internal/reconciler"
	"paygateway/internal/scrcpy"
	"paygateway/internal/store"
	"paygateway/internal/tunnel"
	"paygateway/internal/upstream"

	"github.com/gin-gonic/gin"
)

// ========== 配置 ==========

type App struct {
	cfg          *Config
	store        store.Store
	ledger       *ledger.Ledger
	orders       *order.Service
	notifyW      *notify.Worker
	upstreamPool *upstream.UpstreamPool // 上游池（多商户验签/签名路由）
	confirmSched *reconciler.Scheduler  // 订单确认调度器（主动查单兜底）

	// 个人免签通道
	personalDB     store.PersonalStore
	personalAlipay *personalch.Channel
	personalWechat *personalch.Channel
	accountPool    *account.AccountPool  // 账户池（离线联动冻结/解冻）
	accountDB      *store.AccountDB      // 账户存储（收款码上报时自动创建个人账户）
	ipLimiter      *personal.RateLimiter // 下单限流：每 IP 每分钟下单上限（防脚本刷单）
	alertC         *alert.Client         // 告警系统（nil=未配置）
	getTun         func() *tunnel.Tunnel // 当前隧道（管理后台展示用；nil=无隧道）
	ledgermon      *ledgermon.Manager    // 账本页服务端 adb 监听编排（docs/PLAN-ADB-LEDGER-IMPL.md）
	waitPayPool    *pool.WaitPayPool     // WAIT_PAY 内存订单池（公平调度 + 备注码/金额匹配数据源）
	alipayProber   alipayNoteProber      // 支付宝备注码探测（nil=默认 adb 探测；测试注入桩用）
}

// bgRecover 后台 goroutine 的统一 panic 防护：记录 panic + 栈，避免整个进程崩溃。
// 用法：每个 go func() 第一行 defer bgRecover(component)。
func bgRecover(component string) {
	if r := recover(); r != nil {
		logger.Error("main", "后台任务 panic（已恢复，进程继续运行）",
			"component", component, "panic", r, "stack", string(debug.Stack()))
	}
}

func main() {
	// 加载 .env 文件
	loadEnv(".env")

	cfg := loadConfig()

	// 1. 日志
	logger.Init(logger.Options{
		Level:     cfg.LogLevel,
		LogFile:   cfg.LogFile,
		MaxSizeMB: 50, MaxBackups: 10, MaxAgeDays: 30,
	})
	defer logger.Close()

	logger.Info("main", "=== 支付网关启动 ===")
	logger.Info("main", "监听地址", "addr", cfg.ListenAddr)
	logger.Info("main", "数据库", "file", cfg.DBFile)

	// 2. 存储（SQLite）
	st, err := store.NewSQLite(cfg.DBFile)
	if err != nil {
		logger.Error("main", "数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 2.5 从 DB 加载运行时设置，覆盖 .env 配置
	applySettings(st, cfg)

	// 2.6 独立账本数据库（ledger.db，0600 权限，只增不改）
	ledgerDB, err := store.NewLedgerDB("data/ledger.db")
	if err != nil {
		logger.Error("main", "账本数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer ledgerDB.Close()

	// 3. 审计账本（日志落库交给 store 实现 logger.DBWriter）
	// 私钥持久化：.env > settings 表 > 首次生成并保存到 settings（重启复用，不再每次换新导致验签失败）
	ledgerKey, err := resolveLedgerKey(st, cfg)
	if err != nil {
		logger.Error("main", "审计密钥初始化失败", "err", err)
		os.Exit(1)
	}
	cfg.LedgerPrivKey = ledgerKey
	lg, err := ledger.New(ledgerDB, ledgerKey)
	if err != nil {
		logger.Error("main", "账本初始化失败", "err", err)
		os.Exit(1)
	}
	// 启用日志写库（管理界面查询）：日志写入独立日志库 logs.db，与订单主库分离，支持长期保存
	logDB, err := store.NewLogDB("data/logs.db")
	if err != nil {
		logger.Error("main", "日志数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer logDB.Close()
	logger.SetDBWriter(logDB)

	// 3.5 账户池（独立 accounts.db + 内存缓存）
	accountDB, err := store.NewAccountDB("data/accounts.db")
	if err != nil {
		logger.Error("main", "账户数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer accountDB.Close()

	// 账户加密主密钥：必须显式通过 .env 提供。
	// 不自动生成并落盘到 settings 表——主密钥与密文同库会让"拿到 accounts.db 就能解出全部账户密钥"，
	// 与审计密钥/套码 token 密钥等签名用途密钥的安全等级不同，不应等同处理。
	// 缺失直接 fail-fast，避免 accoutns.db / upstreams.db 已有密文时被新随机密钥静默解密失败、服务起不来反而更难排查。
	accountMasterKey := cfg.AccountMasterKey
	if accountMasterKey == "" {
		logger.Error("main", "ACCOUNT_MASTER_KEY 未配置：拒绝启动。请在 .env 中设置 ACCOUNT_MASTER_KEY（首次部署可用 `openssl rand -hex 32` 生成），更换会导致已有账户/上游密文无法解密")
		os.Exit(1)
	}

	// 创建账户池 + 处理器（唯一访问 accounts.db 的组件）
	accountPool := account.NewPool()
	accountHandler, err := admin.NewAccountHandler(accountDB, accountPool, accountMasterKey)
	if err != nil {
		logger.Error("main", "账户处理器初始化失败", "err", err)
		os.Exit(1)
	}

	// 从 accounts.db 加载已有账户，解密后初始化内存池
	if err := accountHandler.InitFromDB(context.Background()); err != nil {
		logger.Error("main", "加载账户池失败", "err", err)
		os.Exit(1)
	}

	// 3.6 上游池（独立 upstreams.db + 内存缓存，多商户）
	upstreamDB, err := store.NewUpstreamDB("data/upstreams.db")
	if err != nil {
		logger.Error("main", "上游数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer upstreamDB.Close()

	upstreamPool := upstream.NewPool()
	upstreamHandler, err := admin.NewUpstreamHandler(upstreamDB, upstreamPool, accountMasterKey)
	if err != nil {
		logger.Error("main", "上游处理器初始化失败", "err", err)
		os.Exit(1)
	}
	// 从 upstreams.db 加载已有上游，解密后初始化内存池
	// 严格模式：上游只能通过管理后台配置，不从 .env / settings 加载
	if err := upstreamHandler.InitFromDB(context.Background()); err != nil {
		logger.Error("main", "加载上游池失败", "err", err)
		os.Exit(1)
	}
	if upstreamPool.Count() == 0 {
		logger.Warn("main", "上游池为空：请到管理后台「上游对接」页配置至少一个上游（pid + 密钥），否则所有下单将被拒绝")
	}

	// 3.7 个人免签通道存储（独立 personal.db：设备/推送流水/无订单收款）
	personalDB, err := store.NewPersonalDB("data/personal.db")
	if err != nil {
		logger.Error("main", "个人通道数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer personalDB.Close()
	// 自动迁移收款码表（加 user_id 列支持双开空间）
	if err := personalDB.EnsureQrcodeSchema(context.Background()); err != nil {
		logger.Error("main", "收款码表迁移失败", "err", err)
		os.Exit(1)
	}

	// 套码 token 签名密钥：.env > settings 表 > 首次生成并持久化（重启复用）
	qrTokenSecret := cfg.QrTokenSecret
	if qrTokenSecret == "" {
		if v, err := st.GetSetting(context.Background(), "qr_token_secret"); err == nil && v != "" {
			qrTokenSecret = v
		}
	}
	if qrTokenSecret == "" {
		key, err := account.GenerateMasterKey() // 32 字节 hex，复用生成器
		if err != nil {
			logger.Error("main", "生成套码 token 密钥失败", "err", err)
			os.Exit(1)
		}
		qrTokenSecret = key
		if err := st.SetSetting(context.Background(), "qr_token_secret", qrTokenSecret); err != nil {
			logger.Warn("main", "保存套码 token 密钥失败（重启将重新生成，旧二维码失效）", "err", err)
		}
		logger.Info("main", "已生成新的套码 token 密钥并持久化到 settings")
	}
	if err := personal.InitQrToken(qrTokenSecret); err != nil {
		logger.Error("main", "套码 token 密钥初始化失败", "err", err)
		os.Exit(1)
	}

	// 4. 注册通道（始终注册，账户数据从池中取；管理后台「账户池」配置密钥）
	personalAlipay, personalWechat := regChannels(cfg, accountPool, st, personalDB)
	applyChannelRuntimeSettings(st) // 通道注册后加载启停 + 支付模式设置

	// 4.1 启动配置自检（RESILIENCE P1-2）
	runStartupChecks(cfg, accountPool, st, personalDB)

	// 5. 订单服务 + 回调 worker
	orderSvc := order.New(st, lg, accountPool, upstreamPool)
	orderSvc.SetCloseWindow(cfg.OrderCloseMinutes)
	// 备注码释放回调（docs/MIANQIAN.md §6.4）：订单终态 SUCCESS/CLOSED 时释放备注码，
	// 避免仅靠 5 分钟 TTL 惰性回收；通道名 → 对应 personal 通道实例
	orderSvc.SetNoteCodeReleaser(func(channelName, noteCode string) {
		var ch *personalch.Channel
		switch channelName {
		case "personal.alipay":
			ch = personalAlipay
		case "personal.wechat":
			ch = personalWechat
		}
		if ch == nil {
			return
		}
		ch.ReleaseNoteCode(noteCode)
	})
	// 5.0.1 WAIT_PAY 内存订单池（公平调度数据源，docs/DESIGN-FAIR-SCHEDULER.md）：
	// 下单/入账/关单/刷新/换通道/复活 全入口同步；DB 是真相，池是索引。
	// 设备维度子池 + 动态关单窗口（docs/PLAN-DYNAMIC-CLOSE-ALLOC.md）：
	//   - deviceOf：账户 → 设备映射（personal.db 设备绑定），多设备隔离、互不干扰
	//   - winCalc：动态关单窗口 = clamp(设备一轮耗时×1.5+60s, MIN, MAX)，负载驱动
	waitPayPool := pool.NewWaitPayPool(cfg.OrderCloseMinutes)
	// 动态窗口配置校验：MIN/MAX 非法（0 或 MIN>MAX）→ 退化为静态窗口（OrderCloseMinutes）
	dynMin := time.Duration(cfg.OrderCloseMin) * time.Minute
	dynMax := time.Duration(cfg.OrderCloseMax) * time.Minute
	if cfg.OrderCloseMin <= 0 || cfg.OrderCloseMax <= 0 || dynMin >= dynMax {
		dynMin, dynMax = 0, 0
		logger.Warn("main", "动态关单窗口配置非法，退化为静态窗口",
			"min_minutes", cfg.OrderCloseMin, "max_minutes", cfg.OrderCloseMax)
	}
	if personalDB != nil {
		waitPayPool.SetDeviceOf(func(accountID int64) (string, bool) {
			return accountDeviceOf(context.Background(), personalDB, accountID)
		})
	}
	if dynMin > 0 && dynMax > 0 {
		// 动态关单窗口计算器：设备负载驱动，物理锚点见设计文档 §4.1。
		// 设备一轮耗时 = 活跃账户数 × 配额(3) × 单次 poll 成本（默认 4s，可配置）。
		pollCostMS := cfg.DevicePollCostMS
		if pollCostMS <= 0 {
			pollCostMS = 4000
		}
		const (
			baseQuota    = 3   // 调度器 BaseQuota（与 ledgermon.DefaultSchedulerConfig 一致）
			queueFactor  = 1.5 // 最坏排队位置系数
			payReserve   = 60 * time.Second
			activeFactor = float64(baseQuota) / 1000
		)
		waitPayPool.SetWindowCalc(func(activeAccounts, total int) time.Duration {
			_ = total // 负载维度（活跃账户数已隐含；保留签名供未来按订单数加权）
			est := time.Duration(float64(activeAccounts) * activeFactor * float64(pollCostMS) * queueFactor)
			w := est + payReserve
			if w < dynMin {
				w = dynMin
			}
			if w > dynMax {
				w = dynMax
			}
			return w
		})
	}
	// 通知栏快速通道监听器（延迟到 6 节构造；此处先声明变量，供 hooks 闭包唤醒）。
	var nw *notifywatcher.Manager
	orderSvc.SetWaitPayHooks(&order.WaitPayHooks{
		Add: func(o *model.Order) {
			waitPayPool.Add(o)
			// 事件驱动：新订单进池 → 唤醒通知栏监听（池空休眠态立即恢复轮询）
			if nw != nil {
				nw.Wake()
			}
		},
		Upsert: waitPayPool.Upsert,
		Remove: waitPayPool.Remove,
		// 动态关单窗口：扫描器用池中记录的 CloseAt 判断是否真正到期（未到则跳过）
		CloseAtOf: waitPayPool.CloseAtOf,
	})
	notifyW := notify.NewWorker(st, cfg.NotifyAllowPrivateIPs)
	logger.Info("main", "回调 worker SSRF 防护状态", "allow_private_notify", cfg.NotifyAllowPrivateIPs)

	// 5.0 告警系统（RESILIENCE P1-3）：进池 / 账本失败 / dead 通知
	alertC := alert.New(alert.Options{BarkURL: cfg.AlertBarkURL, WebhookURL: cfg.AlertWebhookURL})
	if alertC != nil {
		orderSvc.SetAlert(alertC)
		notifyW.SetAlert(alertC)
		logger.Info("main", "告警系统已启用",
			"bark", cfg.AlertBarkURL != "", "webhook", cfg.AlertWebhookURL != "")
	} else {
		logger.Warn("main", "告警系统未配置（ALERT_BARK_URL / ALERT_WEBHOOK_URL 为空）")
	}

	// 5.1 确认调度器（主动查单兜底）：注入通道查单函数（遍历通道注册表）
	queryFunc := func(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error) {
		var lastErr error
		for _, name := range channel.Names() {
			ch, ok := channel.Get(name)
			if !ok {
				continue
			}
			st, err := ch.QueryOrder(ctx, outTradeNo)
			if err != nil {
				lastErr = err
				continue
			}
			return st, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("无可查询通道")
		}
		return nil, lastErr
	}
	confirmSched := reconciler.NewScheduler(st, orderSvc, queryFunc, nil)

	// 个人通道监听端检查（ConfirmByManual 通道关单前调用）：
	// 设备 adb 不可达 → 订单进异常池人工核实（用户可能已付款但未监听到），绝不直接关单。
	// 在线判定 = 实时 adb 可达性（2026-08-29 重构：不再依赖监听 App 心跳/DB status）。
	// 该函数同时注入 confirmSched（事件驱动主路径）与 orderSvc（DB 扫描兜底路径），
	// 保证两条关单路径的安全检查完全一致（PLAN-DYNAMIC-CLOSE-ALLOC §4.3）。
	var personalCheckFn func(ctx context.Context, o *model.Order) (bool, string)
	if personalDB != nil {
		personalCheckFn = func(ctx context.Context, o *model.Order) (bool, string) {
			// 订单已绑定设备：adb 实时可达判定
			if o.DeviceID != "" {
				enc, err := personalDB.GetDevice(ctx, o.DeviceID)
				if err != nil {
					return true, "设备查询失败: " + err.Error()
				}
				if enc == nil {
					return true, "设备不存在: " + o.DeviceID
				}
				serial := deviceADBSerial(enc)
				if serial == "" {
					return true, "设备未配置 adb 通道"
				}
				if !adbMgr().Online(ctx, serial) {
					return true, "设备 adb 不可达"
				}
				return false, ""
			}
			// 订单未绑定设备：按 account_id 反查承载该账户的设备（一台设备承载
			// 多账户多通道 device_accounts 多对多，不能按 personal_devices.provider
			// 单值过滤——跨通道订单会误判设备离线致关单永久卡异常池，2026-09-13 实测）
			if o.AccountID > 0 {
				devIDs, err := personalDB.DeviceIDsOfAccount(ctx, o.AccountID)
				if err != nil {
					return true, "账户设备反查失败: " + err.Error()
				}
				for _, devID := range devIDs {
					enc, err := personalDB.GetDeviceByID(ctx, devID)
					if err != nil || enc == nil {
						continue
					}
					serial := deviceADBSerial(enc)
					if serial != "" && adbMgr().Online(ctx, serial) {
						return false, ""
					}
				}
			}
			// 账户未反查到在线设备 → 按 provider 匹配任意在线设备兜底
			devs, err := personalDB.ListDevices(ctx)
			if err != nil {
				return true, "设备列表查询失败"
			}
			provider := channel.ProviderFromName(o.Channel)
			for _, d := range devs {
				if d.Provider != provider {
					continue
				}
				serial := deviceADBSerial(d)
				if serial != "" && adbMgr().Online(ctx, serial) {
					return false, ""
				}
			}
			return true, "无 adb 可达设备（provider=" + provider + "）"
		}
		confirmSched.SetPersonalCheck(personalCheckFn)
		// 漏单修复：CloseExpiredOrders 对个人通道也走同一安全检查（此前直接关单绕过离线检查）
		orderSvc.SetPersonalCheck(personalCheckFn)
	}

	app := &App{cfg: cfg, store: st, ledger: lg, orders: orderSvc, notifyW: notifyW, upstreamPool: upstreamPool, confirmSched: confirmSched,
		personalDB: personalDB, personalAlipay: personalAlipay, personalWechat: personalWechat, accountPool: accountPool, accountDB: accountDB,
		ipLimiter:   personal.NewRateLimiter(cfg.MaxOrdersPerIPPerMin, time.Minute, 100000),
		waitPayPool: waitPayPool}

	// tun 全局隧道状态（同时用于 admin / watcher 重启逻辑取隧道 URL）。
	// 在 watcher 启动前声明，确保 watcher 的 recoverer 闭包能引用到。
	var tun *tunnel.Tunnel
	var tunMu sync.RWMutex
	setTun := func(t *tunnel.Tunnel) { tunMu.Lock(); tun = t; tunMu.Unlock() }
	getTun := func() *tunnel.Tunnel { tunMu.RLock(); defer tunMu.RUnlock(); return tun }
	app.getTun = getTun

	// 6. 启动回调 worker + 确认调度器 + 设备离线检测（后台）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifyW.Start(ctx)
	go confirmSched.Start(ctx)
	// 账本页服务端 adb 监听（docs/PLAN-ADB-LEDGER-IMPL.md Phase 1 支付宝闭环）：
	// 服务端直连手机读收款记录页 → 增量检测 → 复用 /personal/push 匹配入账链路。
	// 依赖 personal.db（设备+账户绑定）+ adb 管理器（WiFi adb 5555 直连/USB）。
	if personalDB != nil && adbMgr() != nil {
		// 订单驱动：账户有待支付订单才轮询（导航+下拉刷新+读取），订单终结自动暂停。
		// 避免持续占用手机前台与频繁刷新支付宝（2026-08-27 真机反馈修正）。
		// 资金安全：DB 查询错误视为"仍有待支付订单"（失败开放），保持轮询不误停。
		hasPending := func(ctx context.Context, accountID int64) bool {
			if accountID <= 0 {
				return false
			}
			orders, err := st.ListWaitPayByAccount(ctx, accountID)
			if err != nil {
				logger.Warn("main", "待支付订单查询失败（按仍有订单处理，保持轮询）",
					"err", err, "account_id", accountID)
				return true
			}
			return len(orders) > 0
		}
		app.ledgermon = ledgermon.NewManager(
			adbMgr(),
			app.resolveLedgerDevices,
			app.newLedgerReader,
			app.dispatchLedgerRows,
			hasPending,
			cfg.LedgerActiveInterval,
			personalDB,
			waitPayPool, // 公平调度模式：DeviceScheduler 按内存池快照统一决策轮询
		)
		app.ledgermon.Start(ctx)
		logger.Info("main", "账本页 adb 监听已启动（DeviceActor 模型，公平时间占比调度）",
			"poll", cfg.LedgerActiveInterval)
		// WAIT_PAY 池启动重建（DB 全量待支付订单）+ 周期对账（30s 防漂移）
		if orders, err := st.ListWaitPayAll(ctx); err != nil {
			logger.Error("main", "WAIT_PAY 池重建失败（调度将按空池运行，Reconcile 兜底）", "err", err)
		} else {
			waitPayPool.Rebuild(ctx, orders)
			logger.Info("main", "WAIT_PAY 池重建完成", "orders", len(orders))
		}
		go waitPayPool.ReconcileLoop(ctx, 30*time.Second, func() ([]*model.Order, error) {
			return st.ListWaitPayAll(context.Background())
		})
		// 通知栏快速通道（NotifyWatcher）：独立 goroutine + 独立 adbMgr 实例（Manager.mu 独立，
		// 与 ledgermon DeviceActor 串行队列天然并行，实测 dumpsys notification ∥ dump 无冲突）。
		// 事件驱动：池有 WAIT_PAY 订单 → 高频轮询；池空 → 休眠（新订单进池经 hooks Wake 唤醒）。
		// 窗口锚点用通知 when（见 handleNotify），快速通道比账本页快 ~15s。
		nw = notifywatcher.New(
			adbMgr(), // 独立实例：与 ledgermon 持有的实例 mutex 隔离
			func(ctx context.Context) ([]notifywatcher.Device, error) {
				devs, err := personalDB.ListDevices(ctx)
				if err != nil {
					return nil, err
				}
				var out []notifywatcher.Device
				for _, d := range devs {
					serial := deviceADBSerial(d)
					if serial == "" {
						continue
					}
					if !adbMgr().Online(ctx, serial) {
						continue
					}
					out = append(out, notifywatcher.Device{DeviceID: d.DeviceID, Serial: serial})
				}
				return out, nil
			},
			func(ctx context.Context) bool {
				return waitPayPool.Snapshot().Total > 0
			},
			app.handleNotify,
			personalDB,
			notifywatcher.Config{
				PollEvery:    cfg.NotifyPollInterval,
				IdleEvery:    cfg.NotifyIdleInterval,
				ShellTimeout: 3 * time.Second,
			},
		)
		go nw.Start(ctx)
		logger.Info("main", "通知栏快速通道监听已启动",
			"poll", cfg.NotifyPollInterval, "idle", cfg.NotifyIdleInterval)
	}
	// 监听端离线检测：60s 无心跳 → 设备 OFFLINE → 冻结账户 → WAIT_PAY 订单进异常池
	// 注入 recoverer：handleOffline 起后台 goroutine 调 AttemptRestartByID
	// 自动尝试 docker restart + am start，限频 5min/次（personalHandler 内部管理）。
	// 控制面状态机：注入 adb prober；nil 时状态机静默关闭。
	// 2026-08-29 重构：监听 App 已弃用，adb 反向隧道（relay）全部移除。
	var pushHandler *admin.PersonalHandler
	if personalDB != nil {
		// 设备在线判定注入（2026-08-29 重构）：个人通道选号/可服务账户判定以
		// adb 可达性为准（实时探测 + 自动 connect）。
		adbOnline := func(ctx context.Context, serial string) bool {
			return adbMgr().Online(ctx, serial)
		}
		personalAlipay.SetAdbOnline(adbOnline)
		personalWechat.SetAdbOnline(adbOnline)
		// 均衡分配器注入（docs/PLAN-DYNAMIC-CLOSE-ALLOC.md §5）：
		// 两层分配——先选设备（最闲最健康，超限排除），再选设备内账户（最闲最健康）。
		// 负载数据源：waitPayPool（设备/账户负载）+ app.ledgermon（设备 poll 健康度）。
		// provider 显式传入：支付宝/微信各自注册自己的分配器（SelectFromWeighted 按通道过滤）。
		balancerFor := func(provider channel.Provider) func(ctx context.Context) *account.Account {
			return func(ctx context.Context) *account.Account {
				return balanceSelectAccount(ctx, personalDB, accountPool, waitPayPool, app.ledgermon, cfg, "personal."+string(provider))
			}
		}
		personalAlipay.SetBalancedSelect(balancerFor(channel.ProviderAlipay))
		personalWechat.SetBalancedSelect(balancerFor(channel.ProviderWechat))

		var watcherRecoverer reconciler.Recoverer
		adbProber := reconciler.AsAdbProber(adbMgr())
		if adbMgr() != nil {
			// 提前构造一份 personalHandler 用作 watcher 的 recoverer；
			// adminSrv 后面会用同一参数构造自己的实例，二者独立的 restartLimiter 可接受。
			rh := admin.NewPersonalHandler(personalDB, accountPool, accountDB, adbMgr())
			watcherRecoverer = rh
			pushHandler = rh
		}
		go reconciler.NewDeviceWatcher(personalDB, accountPool, st, orderSvc, nil, watcherRecoverer, adbProber).Start(ctx)
	}

	// 6.5 自动关闭超时未支付订单（定时扫描）
	// 候选集 = max(商家固定窗口 ORDER_CLOSE_MINUTES, 个人动态窗口上限 MAX) + 裕量。
	// 具体订单是否到期由 CloseExpiredOrders 二次判断：
	//   - 个人免签：池内动态 CloseAt（设备负载驱动，未到跳过）
	//   - 商家通道：ORDER_CLOSE_MINUTES 固定窗口（isExpiredWindow，不受动态窗口影响）
	if cfg.OrderCloseMinutes > 0 {
		go func() {
			defer bgRecover("close_scanner")
			interval := time.Duration(cfg.OrderCloseInterval) * time.Second
			if interval <= 0 {
				interval = 60 * time.Second
			}
			// 候选集 = min(商家固定窗口, 个人动态窗口下限)。
			// 个人订单在 age≥dynMin 时进入候选集，由池内 CloseAtOf 按各自动态窗口精确过滤；
			// 商家订单在 age≥dynMin 时进入候选集，由 isExpiredWindow(ORDER_CLOSE_MINUTES) 过滤。
			// 若用 max() 则个人订单要等到 30min 才进候选集，动态窗口（5~10min）完全失效。
			timeout := time.Duration(cfg.OrderCloseMinutes) * time.Minute
			if dynMin > 0 && dynMin < timeout {
				timeout = dynMin
			}
			logger.Info("main", "超时订单自动关闭已启动",
				"candidate_window", timeout.String(), "interval_sec", int(interval/time.Second),
				"merchant_close_minutes", cfg.OrderCloseMinutes,
				"dynamic", dynMin > 0, "personal_min", cfg.OrderCloseMin, "personal_max", cfg.OrderCloseMax)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					before := time.Now().Add(-timeout)
					if _, err := orderSvc.CloseExpiredOrders(ctx, before); err != nil {
						logger.Error("order", "自动关闭扫描失败", "err", err)
					}
				}
			}
		}()
	}

	// 6.5b 收款码定期清理（DB 过期码 + 设备端相册旧码，防存储打满）
	if pushHandler != nil {
		go func() {
			defer bgRecover("qrcode_cleanup")
			interval := 60 * time.Minute
			logger.Info("main", "收款码定期清理已启动", "interval_minutes", int(interval/time.Minute))
			// 启动时先清一次
			pushHandler.CleanupQrcodes(ctx)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					pushHandler.CleanupQrcodes(ctx)
				}
			}
		}()
	}

	// 6.5c 收款码自动续期（每设备每平台一条 active 码，缺失/过期才补捕；静态码长期有效几乎不触发）
	if pushHandler != nil {
		go func() {
			defer bgRecover("qrcode_renew")
			interval := 60 * time.Minute
			logger.Info("main", "收款码自动续期已启动", "interval_minutes", int(interval/time.Minute))
			// 启动时先补一轮（确保已有有效码）
			pushHandler.RenewQrcodes(ctx)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					pushHandler.RenewQrcodes(ctx)
				}
			}
		}()
	}

	// 6.5d 服务端账本读取轮询已废弃（用户决策：账本导航兜底延迟高/自动化困难，
	// 实时到账完全依赖监听端通知推送；如需查账可用账本校验 API 手动触发）。

	// 6.7 账本校验函数（调度器 + API 共用）
	doVerify := func() *admin.VerifyLedgerResult {
		res, err := lg.Verify(context.Background())
		r := &admin.VerifyLedgerResult{CheckedAt: time.Now().Format(time.RFC3339)}
		if err != nil {
			r.Errors = []string{err.Error()}
			logger.Error("ledger", "账本校验失败", "err", err)
		} else {
			r.Valid = res.Valid
			r.TotalEntries = res.TotalEntries
			r.Errors = res.Errors
			r.SnapshotOK = res.SnapshotOK
			r.Duration = time.Since(res.StartTime).Round(time.Millisecond).String()
			if res.Valid {
				logger.Info("ledger", "账本完整 ✅", "count", res.TotalEntries, "snapshot_ok", res.SnapshotOK, "duration", r.Duration)
			} else {
				logger.Error("ledger", "账本校验失败 ❌", "errors", res.Errors)
			}
		}
		return r
	}

	// 6.8 账本定时任务：每日 00:00 快照 + 校验
	go func() {
		defer bgRecover("ledger_daily")
		// 先创建今日快照（刷新锚点到当前链尾），再校验 ——
		// 避免"先校验"读到昨日快照锚点与今日新增条目不匹配而误报 ERROR
		today := time.Now().Format("2006-01-02")
		if err := lg.CreateSnapshot(ctx, today); err != nil {
			logger.Warn("ledger", "创建快照失败（可能已存在）", "err", err)
		} else {
			logger.Info("ledger", "今日快照已创建", "date", today)
		}
		// 启动时执行一次校验
		doVerify()

		// 每 24 小时检查一次（每分钟检查是否到 00:00）
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				// 每天 00:01 执行（给 1 分钟缓冲，避免刚好 00:00:00 没执行到）
				if now.Hour() == 0 && now.Minute() == 1 {
					logger.Info("ledger", "每日账本任务开始")
					// 每日重置账户日已用额度（DB + 内存池同步清零）
					if err := accountDB.ResetDailyUsed(ctx); err != nil {
						logger.Error("main", "重置账户日额度(DB)失败", "err", err)
					} else {
						accountPool.ResetDailyUsed()
						logger.Info("main", "账户日额度已重置")
					}
					// 创建前一日快照
					yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
					if err := lg.CreateSnapshot(ctx, yesterday); err != nil {
						logger.Warn("ledger", "创建快照失败", "date", yesterday, "err", err)
					} else {
						logger.Info("ledger", "快照已创建", "date", yesterday)
					}
					// 全链校验
					doVerify()
					// 日终对账报告（RESILIENCE P2-1）：昨日全量核对
					if rep, err := reconciler.BuildDailyReport(ctx, st, yesterday); err != nil {
						logger.Error("reconciler", "日终对账报告生成失败", "err", err)
					} else {
						rep.Log()
					}
					// 睡 59 秒，避免一天内重复触发
					time.Sleep(59 * time.Second)
				}
			}
		}
	}()

	// 7. 启动 Cloudflare Tunnel（后台，获取公网回调地址）
	// 隧道域名每次启动随机分配，通过回调变量动态更新，不硬编码
	// 隧道重启换域名时，OnURLChange 自动更新支付宝通道的回调地址

	go func() {
		defer bgRecover("cloudflare_tunnel")
		logger.Info("main", "正在启动 Cloudflare Tunnel...")

		// 从 .env（CF_TUNNEL_CREDENTIALS + CF_TUNNEL_HOSTNAME）生成 ~/.cloudflared/ 配置。
		// 换环境只需复制 .env 中对应行，无需手动创建凭据文件。
		if err := tunnel.EnsureCredentials(); err != nil {
			logger.Warn("main", "自动生成隧道配置失败（不影响已有文件）", "err", err)
		}

		// updateAlipayNotifyURL 更新支付宝通道的回调地址
		updateAlipayNotifyURL := func(tunURL string) {
			if tunURL == "" {
				return
			}
			notifyURL := tunURL + "/alipay/notify"
			cfg.AlipayNotifyURL = notifyURL
			// 通过类型断言更新支付宝通道的回调地址
			if ch, ok := channel.Get("merchant.alipay"); ok {
				if alipayCh, ok := ch.(*alipaych.Channel); ok {
					alipayCh.SetNotifyURL(notifyURL)
				}
			}
			// 更新微信通道回调地址
			wxNotifyURL := tunURL + "/wechat/notify"
			cfg.WechatNotifyURL = wxNotifyURL
			if ch, ok := channel.Get("merchant.wechat"); ok {
				if wc, ok := ch.(*wechatch.Channel); ok {
					wc.SetNotifyURL(wxNotifyURL)
				}
			}
			// 更新个人免签通道公网地址（套码二维码用）
			if personalAlipay != nil {
				personalAlipay.SetBaseURL(tunURL)
			}
			if personalWechat != nil {
				personalWechat.SetBaseURL(tunURL)
			}
			cfg.PersonalBaseURL = tunURL
			// 对接信息同源更新：env 里只写了 localhost/内网（历史默认）时由固定隧道域名接管，
			// 显式配置了公网域名的场景不覆盖（尊重运维意图）
			if cfg.EpayPublicBase == "" || notify.IsPrivateURL(cfg.EpayPublicBase) {
				cfg.EpayPublicBase = tunURL
			}
			// 保存到文件，方便查看当前回调地址
			_ = os.WriteFile("current_callback.txt", []byte("alipay: "+notifyURL+"\nwechat: "+wxNotifyURL+"\ntunnel: "+tunURL+"\n"), 0644)
			logger.Info("main", "回调地址已更新", "alipay", notifyURL, "wechat", wxNotifyURL, "tunnel", tunURL)
		}

		// 启动隧道（阻塞直到就绪或超时）
		// OnURLChange 回调：隧道就绪 + 重启换域名时自动触发
		tun, err := tunnel.New(ctx, &tunnel.Config{
			LocalPort:    8080,
			CallbackPath: "/alipay/notify",
			StartTimeout: 30 * time.Second,
			Connections:  4,
			OnURLChange:  updateAlipayNotifyURL,
			Hostname:     cfg.TunnelHostname,
		})
		if err != nil {
			logger.Warn("main", "隧道启动失败（不影响本地服务，但支付宝回调不可用）", "err", err)
			return
		}
		setTun(tun)

		logger.Info("main", "隧道就绪 ✅", "url", tun.URL(), "callback", cfg.AlipayNotifyURL)

		// 启动健康检查 + 自动重启（隧道挂了会自动换域名重连）
		tun.HealthLoop(ctx, 30*time.Second)
	}()

	// 8. HTTP 服务
	r := gin.Default()
	r.Use(traceMiddleware())

	// 公网支付路由组：按 IP 限流（防刷 / 防 DoS），不影响签名回调路由
	publicLimiter := ratelimit.IPRateLimit(cfg.PublicRateMax, time.Duration(cfg.PublicRateWindowSec)*time.Second)
	public := r.Group("", publicLimiter)

	// 易支付协议接口（New API 调用，保持兼容）
	public.GET("/submit.php", app.handleEpaySubmit)
	public.POST("/submit.php", app.handleEpaySubmit)
	public.GET("/api.php", app.handleEpayQuery)

	// 二维码刷新（生成新订单 + 新二维码）
	public.GET("/qr/refresh", app.handleQrRefresh)

	// 订单过期页（支付页倒计时归零后跳转；return_url 指向商家页用于重新下单）
	public.GET("/qr/expired", app.handleQrExpired)

	// 订单状态探测（过期页 JS 轮询：上游同号重提复活后自动跳回支付页，不死在过期页）
	public.GET("/qr/status", app.handleQrStatus)

	// 支付成功落地页（付款人支付后展示成功状态；仅 TRADE_SUCCESS 渲染）
	public.GET("/qr/success", app.handleQrSuccess)

	// 切换支付方式（订单智能迁移到目标通道可用账户，支持过期订单复活）
	public.GET("/qr/switch", app.handleQrSwitch)

	// 浏览器指纹绑定（支付页 JS 采集 canvas/时区/屏幕后上报，增强 buyer_key 抗换 IP 刷单）
	public.GET("/personal/bind-fp", app.handleBindFingerprint)

	// 支付宝异步回调
	r.POST("/alipay/notify", app.handleAlipayNotify)

	// 微信异步回调
	r.POST("/wechat/notify", app.handleWechatNotify)

	// 个人免签通道：直发码图片（真实收款码二维码；token 校验，图片不进支付页 HTML）
	public.GET("/qr/real", app.handleQrReal)

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Format(time.RFC3339)})
	})

	// 管理后台（独立端口 8081，Vue3 前端 + JSON API）
	if cfg.AdminAddr != "" {
		adminSrv := admin.New(st, ledgerDB, "/alipay/notify",
			func() string {
				if t := getTun(); t != nil {
					return t.URL()
				}
				return ""
			},
			func() bool {
				if t := getTun(); t != nil {
					return t.Healthy()
				}
				return false
			},
			func() []admin.ChannelInfo {
				return buildChannelInfos(accountPool)
			},
			func() *admin.EpayInfo { return buildEpayInfo(cfg) },
			func(key, value string) {
				// 设置变更回调：更新通道运行时配置
				// 支付模式切换
				if strings.HasPrefix(key, "alipay_mode_") {
					mode := strings.TrimPrefix(key, "alipay_mode_")
					if ch, ok := channel.Lookup("merchant.alipay"); ok {
						type modeSetter interface {
							SetModeEnabled(m string, enabled bool)
						}
						if ms, ok := ch.(modeSetter); ok {
							ms.SetModeEnabled(mode, value == "true")
						}
					}
				}
			},
			doVerify,
			func() error {
				ctx := context.Background()
				today := time.Now().Format("2006-01-02")
				return lg.CreateSnapshot(ctx, today)
			},
			func() string { return lg.PublicKeyPEM() },
			accountHandler,
			upstreamHandler,
			admin.NewReconcilerHandler(st, orderSvc, confirmSched),
			admin.NewPersonalHandler(personalDB, accountPool, accountDB, adbMgr()),
			cfg.JWTSecret,
		)
		// 实时设备流服务（云手机式操作）：注入 StreamServer，管理后台提供 WebSocket 入口
		streamSrv := scrcpy.NewStreamServer()
		adminSrv.SetStreamServer(streamSrv)
		adminSrv.SetLogStore(logDB)
		// 账本页监听状态（只读，docs/PLAN-ADB-LEDGER-IMPL.md §7）
		adminSrv.SetLedgerStatus(func() any {
			if app.ledgermon == nil {
				return []any{}
			}
			return app.ledgermon.Status()
		})
		// 设备手动预热（DeviceActor 模型：手动预热是队列中的一个命令，天然互斥）
		adminSrv.SetLedgerWarmup(func(ctx context.Context, deviceID string, providers map[string]bool) error {
			if app.ledgermon == nil {
				return fmt.Errorf("账本监听未启用")
			}
			return app.ledgermon.WarmupDevice(ctx, deviceID, providers)
		})
		go adminSrv.Run(cfg.AdminAddr)
	}

	logger.Info("main", "服务启动完成 ✅")
	if err := r.Run(cfg.ListenAddr); err != nil {
		logger.Error("main", "服务退出", "err", err)
	}
}

// adbMgr 创建设备操作管理器（web 可视化操作 USB 真机 / adb over WiFi 设备）
// adb 二进制从系统 PATH 获取（宿主机需安装 adb）；构建产物与运行环境需一致。
