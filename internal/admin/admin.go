// Package admin 管理后台：静态页面 + JSON API
//
// 前端为 Vue3 + Element Plus 构建产物，embed 进二进制（internal/admin/static）。
// 部署时只需一个 gateway 二进制，无需单独部署前端。
package admin

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/scrcpy"
	"paygateway/internal/store"
)

// mimeTypeByPath 根据文件扩展名返回 Content-Type
func mimeTypeByPath(p string) string {
	switch ext := filepath.Ext(p); ext {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".woff", ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".ico":
		return "image/x-icon"
	default:
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
		return "application/octet-stream"
	}
}

//go:embed all:static
var staticFS embed.FS

// TunnelStatus 隧道状态提供者（由 main 注入，避免循环依赖）
type TunnelStatus struct {
	URL       string `json:"url"`      // 隧道地址（空 = 未启动）
	Callback  string `json:"callback"` // 完整回调地址
	Healthy   bool   `json:"healthy"`  // 健康状态
	Running   bool   `json:"running"`  // 是否已启动
	Error     string `json:"error,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// EpayInfo 易支付对接信息（其他项目配置 paygateway 时使用）
type EpayInfo struct {
	BaseURL   string `json:"base_url"`   // 服务基础地址
	SubmitURL string `json:"submit_url"` // 下单接口（New API 的"易支付网关地址"填这个）
	QueryURL  string `json:"query_url"`  // 订单查询接口
	Pid       string `json:"pid"`        // 商户ID
	Key       string `json:"key"`        // 商户密钥
	SignType  string `json:"sign_type"`  // 签名方式
}

// ChannelInfo 支付通道信息（由 main 组装，admin 不依赖具体通道包）
type ChannelInfo struct {
	Name         string          `json:"name"`
	Enabled      bool            `json:"enabled"`
	Kind         string          `json:"kind"`                    // callback / listener
	Modes        []string        `json:"modes"`                   // 支持模式
	ModesEnabled map[string]bool `json:"modes_enabled,omitempty"` // 各模式启停状态
	AppID        string          `json:"app_id,omitempty"`
	MchID        string          `json:"mch_id,omitempty"`
	IsProd       bool            `json:"is_prod"`
	NotifyURL    string          `json:"notify_url,omitempty"`
	// 账户配置状态（引导用）：是否已配置账户 + 账户数量
	Configured   bool `json:"configured"`
	AccountCount int  `json:"account_count"`
	// 统计（由 admin 填充）
	TodayOrders int64 `json:"today_orders"`
	TodayAmount int64 `json:"today_amount"`
	TotalOrders int64 `json:"total_orders"`
	TotalAmount int64 `json:"total_amount"`
}

// LogStore 独立日志库接口（查询管理界面日志用，与订单主库分离）
type LogStore interface {
	QueryLogs(ctx context.Context, level, scope string, limit, offset int) ([]*store.LogRecord, error)
	CountLogs(ctx context.Context, level, scope string) (int64, error)
}

// Server 管理后台服务
type Server struct {
	st                store.Store
	logStore          LogStore                   // 独立日志数据库（长期保存，查询日志用）
	ledgerStore       store.LedgerStore          // 独立账本数据库
	callbackPath      string                     // /alipay/notify
	tunnelURL         func() string              // 获取隧道 URL
	tunnelHealth      func() bool                // 隧道健康
	tunnelErr         func() string              // 隧道错误信息（可选）
	channels          func() []ChannelInfo       // 通道信息提供者
	epayInfo          func() *EpayInfo           // 易支付对接信息提供者
	onSettingsChange  func(key, value string)    // 设置变更回调（通知 main 更新通道等）
	onLedgerVerify    func() *VerifyLedgerResult // 账本校验回调
	onLedgerSnapshot  func() error               // 创建快照回调
	onLedgerPublicKey func() string              // 账本公钥回调

	// 账户池管理（唯一访问 accounts.db 的组件）
	accountHandler *AccountHandler
	// 上游管理（唯一访问 upstreams.db 的组件）
	upstreamHandler   *UpstreamHandler
	reconcilerHandler *ReconcilerHandler
	// 个人免签通道管理（设备/推送流水/无订单收款）
	personalHandler *PersonalHandler
	// 实时设备流服务（云手机式操作；nil = 未启用）
	streamSrv *scrcpy.StreamServer
	// 账本页服务端监听状态提供者（main.go 注入；nil = 未启用）。返回 JSON 序列化数据。
	ledgerStatus func() any
	// ledgerWarmup 设备手动预热函数（main.go 注入；nil = 未启用）。DeviceActor 模型中
	// 手动预热是队列中的一个命令，与自动预热/轮询天然互斥，无需窗口独占。
	ledgerWarmup func(ctx context.Context, deviceID string, providers map[string]bool) error

	// 鉴权：凭据来源 DB(settings)；无凭据时 adminSetupNeeded=true 引导注册
	adminUser        string
	adminPass        []byte     // bcrypt 哈希，禁止明文比对
	adminSetupNeeded bool       // 首次启动，尚无管理员，需引导注册
	setupMu          sync.Mutex // 保护注册的并发竞态（防多个请求同时通过检查）
	jwtSecret        []byte

	startTime time.Time

	loginLimiter *loginLimiter
}

// VerifyLedgerResult 账本校验结果（admin 展示用）
type VerifyLedgerResult struct {
	Valid        bool     `json:"valid"`
	TotalEntries int64    `json:"total_entries"`
	Errors       []string `json:"errors,omitempty"`
	SnapshotOK   bool     `json:"snapshot_ok"`
	Duration     string   `json:"duration"`
	CheckedAt    string   `json:"checked_at"`
}

// tokenExpiry token 过期时间
const tokenExpiry = 24 * time.Hour

// New 创建管理后台服务。
// 管理员凭据完全由数据库驱动（settings 表 admin_username / admin_password_hash，bcrypt）。
// 无凭据时 adminSetupNeeded=true，前端引导首次注册；.env 不再承载管理员凭据。
func New(st store.Store, ledgerStore store.LedgerStore, callbackPath string, tunURL func() string, tunHealth func() bool, channels func() []ChannelInfo, epayInfo func() *EpayInfo, onSettingsChange func(key, value string), onLedgerVerify func() *VerifyLedgerResult, onLedgerSnapshot func() error, onLedgerPublicKey func() string, accountHandler *AccountHandler, upstreamHandler *UpstreamHandler, reconcilerHandler *ReconcilerHandler, personalHandler *PersonalHandler, jwtSecret string) *Server {
	secret := jwtSecret
	if secret == "" {
		// 安全：未配置 JWT_SECRET 时生成随机密钥（禁止可预测的日期密钥，防止伪造 token）
		// 注意：随机密钥仅内存持有，服务重启后已发 token 失效（重新登录即可）
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			// crypto/rand 失败几乎不可能；兜底用时间戳+pid 混合，仍不可预测
			buf = []byte(fmt.Sprintf("pgw-%d-%d", time.Now().UnixNano(), os.Getpid()))
		}
		secret = hex.EncodeToString(buf)
		logger.Warn("admin", "JWT_SECRET 未配置，已生成随机密钥（重启后需重新登录）")
	}
	s := &Server{
		st:                st,
		ledgerStore:       ledgerStore,
		callbackPath:      callbackPath,
		tunnelURL:         tunURL,
		tunnelHealth:      tunHealth,
		channels:          channels,
		epayInfo:          epayInfo,
		onSettingsChange:  onSettingsChange,
		onLedgerVerify:    onLedgerVerify,
		onLedgerSnapshot:  onLedgerSnapshot,
		onLedgerPublicKey: onLedgerPublicKey,
		accountHandler:    accountHandler,
		upstreamHandler:   upstreamHandler,
		reconcilerHandler: reconcilerHandler,
		personalHandler:   personalHandler,
		jwtSecret:         []byte(secret),
		startTime:         time.Now(),
		loginLimiter:      newLoginLimiter(),
	}
	s.loadAdminFromDB()
	return s
}

// SetLogStore 绑定独立日志数据库（New 之后调用；日志查询/统计均走该库）
func (s *Server) SetLogStore(ls LogStore) { s.logStore = ls }

// loadAdminFromDB 从 settings 表加载管理员凭据（admin_username / admin_password_hash，bcrypt）
func (s *Server) loadAdminFromDB() {
	ctx := context.Background()
	user, errU := s.st.GetSetting(ctx, "admin_username")
	pass, errP := s.st.GetSetting(ctx, "admin_password_hash")
	if errU == nil && errP == nil && user != "" && strings.HasPrefix(pass, "$2") {
		s.adminUser = user
		s.adminPass = []byte(pass)
		logger.Info("admin", "管理员凭据已从数据库加载", "username", user)
		return
	}
	// 无管理员凭据 → 首次启动，引导注册
	s.adminSetupNeeded = true
	logger.Info("admin", "首次启动：无管理员凭据，等待注册（POST /api/auth/register）")
}

// SetStreamServer 注入实时设备流服务（web 控制台云手机式操作）
// 不通过 New 注入以保持签名稳定；未设置时设备控制台回退到截图模式
func (s *Server) SetStreamServer(ss *scrcpy.StreamServer) {
	s.streamSrv = ss
}

// SetLedgerStatus 注入账本页监听状态提供者（docs/PLAN-ADB-LEDGER-IMPL.md §7 只读接口）
func (s *Server) SetLedgerStatus(fn func() any) { s.ledgerStatus = fn }

// SetLedgerWarmup 注入设备手动预热函数（DeviceActor 模型：手动预热是队列中的一个命令，天然互斥）。
// fn 接收 (ctx, deviceID, providers)，provider 指定通道（{"alipay":true,"wechat":true}，空=全部）。
func (s *Server) SetLedgerWarmup(fn func(ctx context.Context, deviceID string, providers map[string]bool) error) {
	s.ledgerWarmup = fn
}

// Run 启动管理后台（阻塞）
func (s *Server) Run(addr string) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// ---- 登录（无需鉴权）----
	r.POST("/api/auth/login", s.handleLogin)
	// ---- 首次启动注册（无需鉴权；已配置管理员后自动拒绝）----
	r.POST("/api/auth/register", s.handleRegister)
	r.GET("/api/auth/setup-status", s.handleSetupStatus)

	// ---- 健康检查（无需鉴权，供 LB/监控/heartbeat 用）----
	// 返回 200 = 服务进程活着 + 业务 DB 可读 + 账本表可读；隧道状态作为子项返回不影响 200。
	// 失败的子项以 503 + JSON 暴露给运维 / 监控告警。
	r.GET("/api/health", s.handleHealth)

	// ---- JSON API（需鉴权）----
	api := r.Group("/api", s.authMiddleware())
	{
		api.GET("/dashboard", s.handleDashboard)
		api.GET("/epay-info", s.handleEpayInfo)
		api.GET("/channels", s.handleChannels)
		api.GET("/orders", s.handleListOrders)
		api.GET("/orders/:id/events", s.handleOrderEvents)
		api.GET("/notify-tasks", s.handleNotifyTasks)
		api.GET("/notify-stats", s.handleNotifyStats)
		api.GET("/logs", s.handleLogs)
		api.GET("/ledger", s.handleLedger)
		api.POST("/ledger/verify", s.handleLedgerVerify)
		api.GET("/ledger/snapshots", s.handleLedgerSnapshots)
		api.POST("/ledger/snapshot", s.handleCreateSnapshot)
		api.GET("/ledger/public-key", s.handleLedgerPublicKey)
		api.GET("/ledger/export", s.handleLedgerExport)
		api.GET("/tunnel", s.handleTunnel)
		api.GET("/settings", s.handleSettings)
		api.POST("/settings", s.handleUpdateSettings)
		api.POST("/auth/reset", s.handleResetAdmin)
		// 安全密钥查看（确认后返回完整值，含账本私钥/QR token 等）
		api.GET("/settings/keys", s.handleSettingsKeys)
		api.POST("/settings/channel/:name/toggle", s.handleToggleChannel)
		api.POST("/settings/channel/:name/mode/:mode/toggle", s.handleToggleChannelMode)

		// 账户池管理路由（仅 Admin API 可访问 accounts.db）
		if s.accountHandler != nil {
			s.accountHandler.RegisterRoutes(api)
		}
		// 上游管理路由（仅 Admin API 可访问 upstreams.db）
		if s.upstreamHandler != nil {
			s.upstreamHandler.RegisterRoutes(api)
		}
		if s.reconcilerHandler != nil {
			s.reconcilerHandler.RegisterRoutes(api)
		}
		// 个人免签通道管理路由（设备/推送流水/无订单收款）
		if s.personalHandler != nil {
			if s.ledgerWarmup != nil {
				s.personalHandler.SetLedgerWarmup(s.ledgerWarmup)
			}
			s.personalHandler.RegisterRoutes(api)
		}
		// 账本页服务端监听状态（只读，docs/PLAN-ADB-LEDGER-IMPL.md §7）
		if s.ledgerStatus != nil {
			api.GET("/ledger-monitors", s.handleLedgerMonitors)
		}
	}
	// 实时设备流（云手机式操作）—— WebSocket 升级无法携带 Authorization header，
	// 必须注册在根路由（无 authMiddleware），token 走 query 参数在 handler 内校验
	if s.streamSrv != nil {
		r.GET("/api/personal/devices/stream", s.handleDeviceStream)
	}

	// ---- 静态页面（SPA，NoRoute 统一处理，避免与 /api 路由冲突）----
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		logger.Error("admin", "静态资源加载失败", "err", err)
		sub = nil
	}
	r.NoRoute(func(c *gin.Context) {
		if sub != nil && c.Request.Method == http.MethodGet {
			// 1) 先尝试从静态目录读真实文件（/assets/*.js 等）
			p := strings.TrimPrefix(c.Request.URL.Path, "/")
			if p == "" {
				p = "index.html"
			}
			if data, err := fs.ReadFile(sub, p); err == nil {
				// index.html 不缓存（保证 SPA 入口始终引用最新 bundle；JS/CSS 带 hash 名天然不冲突）
				if p == "index.html" {
					c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
				}
				c.Data(http.StatusOK, mimeTypeByPath(p), data)
				return
			}
			// 2) 读不到 → SPA 回退到 index.html（前端路由）
			if data, err := fs.ReadFile(sub, "index.html"); err == nil {
				c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
				c.Data(http.StatusOK, "text/html; charset=utf-8", data)
				return
			}
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})

	logger.Info("admin", "管理后台启动", "addr", addr)
	if err := r.Run(addr); err != nil {
		logger.Error("admin", "管理后台退出", "err", err)
	}
}

// ========== Dashboard ==========

func (s *Server) handleDashboard(c *gin.Context) {
	ctx := c.Request.Context()
	d := gin.H{}
	var err error

	if d["total_orders"], err = s.st.CountOrders(ctx, ""); err != nil {
		s.err(c, err)
		return
	}
	if d["success_orders"], err = s.st.CountOrders(ctx, model.StatusSuccess); err != nil {
		s.err(c, err)
		return
	}
	if d["wait_orders"], err = s.st.CountOrders(ctx, model.StatusWaitPay); err != nil {
		s.err(c, err)
		return
	}
	if d["closed_orders"], err = s.st.CountOrders(ctx, model.StatusClosed); err != nil {
		s.err(c, err)
		return
	}
	if d["total_amount_minor"], err = s.st.SumAmount(ctx, model.StatusSuccess); err != nil {
		s.err(c, err)
		return
	}

	// 通知任务统计
	if d["notify_pending"], err = s.st.CountNotifyTasks(ctx, "pending"); err != nil {
		s.err(c, err)
		return
	}
	if d["notify_success"], err = s.st.CountNotifyTasks(ctx, "success"); err != nil {
		s.err(c, err)
		return
	}
	if d["notify_dead"], err = s.st.CountNotifyTasks(ctx, "dead"); err != nil {
		s.err(c, err)
		return
	}

	// 日志统计（独立日志库；未配置时跳过）
	if s.logStore != nil {
		if d["log_errors"], err = s.logStore.CountLogs(ctx, "ERROR", ""); err != nil {
			s.err(c, err)
			return
		}
		if d["log_warns"], err = s.logStore.CountLogs(ctx, "WARN", ""); err != nil {
			s.err(c, err)
			return
		}
	} else {
		d["log_errors"] = int64(0)
		d["log_warns"] = int64(0)
	}

	// 账本（独立数据库）
	if d["ledger_count"], err = s.ledgerStore.CountLedger(ctx); err != nil {
		s.err(c, err)
		return
	}
	last, err := s.ledgerStore.GetLastLedgerEntry(ctx)
	if err != nil {
		s.err(c, err)
		return
	}
	if last != nil {
		d["ledger_last_hash"] = last.EntryHash
		d["ledger_last_time"] = last.CreatedAt.Format(time.RFC3339)
	}

	// 最近订单
	recent, err := s.st.ListOrders(ctx, "", "", 8, 0)
	if err != nil {
		s.err(c, err)
		return
	}
	d["recent_orders"] = recent

	// 订单/金额趋势：支持 start/end（日历范围）；无则用旧的 days 快捷参数
	start := c.Query("start")
	end := c.Query("end")
	if start == "" || end == "" {
		days, _ := strconv.Atoi(c.DefaultQuery("days", "14"))
		if days <= 0 {
			days = 14
		}
		end = time.Now().Format("2006-01-02")
		start = time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}
	trend, err := s.st.OrdersTrend(ctx, start, end)
	if err != nil {
		s.err(c, err)
		return
	}
	d["trend"] = trend

	// 隧道状态
	d["tunnel"] = s.tunnelStatus()

	c.JSON(http.StatusOK, gin.H{"data": d})
}

// ========== Epay Info ==========

func (s *Server) handleEpayInfo(c *gin.Context) {
	if s.epayInfo == nil {
		c.JSON(http.StatusOK, gin.H{"data": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": s.epayInfo()})
}

// ========== Channels ==========

func (s *Server) handleChannels(c *gin.Context) {
	ctx := c.Request.Context()
	var out []ChannelInfo

	if s.channels != nil {
		out = s.channels()
	} else {
		out = []ChannelInfo{}
	}

	// 填充统计：每个通道的今日/累计订单与金额
	today := time.Now().Format("2006-01-02")
	for i := range out {
		// 累计
		if n, err := s.st.CountOrdersByChannel(ctx, "", out[i].Name); err == nil {
			out[i].TotalOrders = n
		}
		if a, err := s.st.SumAmountByChannel(ctx, model.StatusSuccess, out[i].Name); err == nil {
			out[i].TotalAmount = a
		}
		// 今日（通过 trend 或直接查：简单方式查今日创建的全部订单）
		if n, err := s.st.CountOrdersByChannelToday(ctx, out[i].Name, today); err == nil {
			out[i].TodayOrders = n
		}
		if a, err := s.st.SumAmountByChannelToday(ctx, out[i].Name, today); err == nil {
			out[i].TodayAmount = a
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ========== Notify Stats ==========

func (s *Server) handleNotifyStats(c *gin.Context) {
	ctx := c.Request.Context()
	d := gin.H{}
	var err error
	if d["pending"], err = s.st.CountNotifyTasks(ctx, "pending"); err != nil {
		s.err(c, err)
		return
	}
	if d["success"], err = s.st.CountNotifyTasks(ctx, "success"); err != nil {
		s.err(c, err)
		return
	}
	if d["dead"], err = s.st.CountNotifyTasks(ctx, "dead"); err != nil {
		s.err(c, err)
		return
	}
	// 待重试（即将到期的 pending）
	if d["total"], err = s.st.CountNotifyTasks(ctx, ""); err != nil {
		s.err(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": d})
}

// ========== Orders ==========

func (s *Server) handleListOrders(c *gin.Context) {
	ctx := c.Request.Context()
	status := c.Query("status")
	channel := c.Query("channel")
	keyword := c.Query("keyword")
	from := c.Query("from")
	to := c.Query("to")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	total, err := s.st.CountOrdersFiltered(ctx, status, channel, keyword, from, to)
	if err != nil {
		s.err(c, err)
		return
	}
	list, err := s.st.ListOrdersFiltered(ctx, status, channel, keyword, from, to, limit, offset)
	if err != nil {
		s.err(c, err)
		return
	}
	// 附加账户名称（从账户池内存查询）
	if s.accountHandler != nil {
		for _, o := range list {
			if o.AccountID > 0 {
				o.AccountName = s.accountHandler.AccountName(o.AccountID)
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"total": total, "list": list}})
}

func (s *Server) handleOrderEvents(c *gin.Context) {
	ctx := c.Request.Context()
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		s.err(c, fmt.Errorf("无效订单ID"))
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	events, err := s.st.ListEvents(ctx, id, limit)
	if err != nil {
		s.err(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": events})
}

// ========== Notify Tasks ==========

func (s *Server) handleNotifyTasks(c *gin.Context) {
	ctx := c.Request.Context()
	status := c.Query("status")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := s.st.ListNotifyTasks(ctx, status, limit)
	if err != nil {
		s.err(c, err)
		return
	}
	// 关联订单号
	type taskWithOrder struct {
		*store.NotifyTask
		OutTradeNo  string `json:"out_trade_no"`
		Subject     string `json:"subject"`
		AmountMinor int64  `json:"amount_minor"`
	}
	var out []taskWithOrder
	for _, t := range list {
		item := taskWithOrder{NotifyTask: t}
		if o, err := s.st.GetOrderByID(ctx, t.OrderID); err == nil && o != nil {
			item.OutTradeNo = o.OutTradeNo
			item.Subject = o.Subject
			item.AmountMinor = o.AmountMinor
		}
		out = append(out, item)
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ========== Logs ==========

func (s *Server) handleLogs(c *gin.Context) {
	ctx := c.Request.Context()
	if s.logStore == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"total": 0, "list": []*store.LogRecord{}}})
		return
	}
	level := c.Query("level")
	scope := c.Query("scope")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	total, err := s.logStore.CountLogs(ctx, level, scope)
	if err != nil {
		s.err(c, err)
		return
	}
	list, err := s.logStore.QueryLogs(ctx, level, scope, limit, offset)
	if err != nil {
		s.err(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"total": total, "list": list}})
}

// ========== Ledger ==========

func (s *Server) handleLedger(c *gin.Context) {
	ctx := c.Request.Context()
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	list, err := s.ledgerStore.ListLedger(ctx, limit)
	if err != nil {
		s.err(c, err)
		return
	}
	count, err := s.ledgerStore.CountLedger(ctx)
	if err != nil {
		s.err(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"total": count, "list": list}})
}

// handleLedgerVerify 手动触发账本校验
func (s *Server) handleLedgerVerify(c *gin.Context) {
	if s.onLedgerVerify == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"valid": false, "error": "未配置账本校验回调"}})
		return
	}
	logger.Info("admin", "手动触发账本校验")
	res := s.onLedgerVerify()
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// handleLedgerSnapshots 快照列表
func (s *Server) handleLedgerSnapshots(c *gin.Context) {
	ctx := c.Request.Context()
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	list, err := s.ledgerStore.ListSnapshots(ctx, limit)
	if err != nil {
		s.err(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// handleCreateSnapshot 手动创建快照
func (s *Server) handleCreateSnapshot(c *gin.Context) {
	if s.onLedgerSnapshot == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未配置账本"})
		return
	}
	if err := s.onLedgerSnapshot(); err != nil {
		logger.Error("admin", "创建快照失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Info("admin", "手动创建快照成功")
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "快照创建成功"}})
}

// handleLedgerPublicKey 返回账本公钥（供外部验证签名）
func (s *Server) handleLedgerPublicKey(c *gin.Context) {
	if s.onLedgerPublicKey == nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"public_key": "未配置账本"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"public_key": s.onLedgerPublicKey()}})
}

// handleLedgerExport 导出账本为 CSV（审计报告）
func (s *Server) handleLedgerExport(c *gin.Context) {
	ctx := c.Request.Context()

	// 账本条目 CSV
	entries, err := s.ledgerStore.ListLedger(ctx, 100000)
	if err != nil {
		s.err(c, err)
		return
	}

	// 快照 CSV
	snapshots, err := s.ledgerStore.ListSnapshots(ctx, 1000)
	if err != nil {
		s.err(c, err)
		return
	}

	var b strings.Builder
	// 写入 UTF-8 BOM（Excel 中文兼容）
	b.WriteString("\xEF\xBB\xBF")
	b.WriteString("# PayGateway 审计账本导出\n")
	b.WriteString("# 生成时间: " + time.Now().Format("2006-01-02 15:04:05") + "\n")
	b.WriteString("# 账本条目数: " + strconv.Itoa(len(entries)) + ", 快照数: " + strconv.Itoa(len(snapshots)) + "\n\n")

	b.WriteString("== 账本条目 ==\n")
	b.WriteString("id,event_id,prev_hash,payload_hash,entry_hash,signature,created_at\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("%d,%d,%s,%s,%s,%s,%s\n",
			e.ID, e.EventID, e.PrevHash, e.PayloadHash, e.EntryHash, e.Signature,
			e.CreatedAt.Format(time.RFC3339)))
	}

	b.WriteString("\n== 每日快照 ==\n")
	b.WriteString("id,snapshot_date,last_hash,signature,created_at\n")
	for _, sn := range snapshots {
		b.WriteString(fmt.Sprintf("%d,%s,%s,%s,%s\n",
			sn.ID, sn.SnapshotDate, sn.LastHash, sn.Signature,
			sn.CreatedAt.Format(time.RFC3339)))
	}

	// 设置下载响应头
	filename := fmt.Sprintf("ledger_audit_%s.csv", time.Now().Format("20060102"))
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte(b.String()))
}

// ========== Tunnel ==========

func (s *Server) handleTunnel(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": s.tunnelStatus()})
}

// ========== 账本页服务端监听状态（只读） ==========

func (s *Server) handleLedgerMonitors(c *gin.Context) {
	if s.ledgerStatus == nil {
		c.JSON(http.StatusOK, gin.H{"data": []any{}, "enabled": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": s.ledgerStatus(), "enabled": true})
}

func (s *Server) tunnelStatus() TunnelStatus {
	ts := TunnelStatus{UpdatedAt: time.Now().Format(time.RFC3339)}
	if s.tunnelURL != nil {
		ts.URL = s.tunnelURL()
	}
	if s.tunnelHealth != nil {
		ts.Healthy = s.tunnelHealth()
	}
	if s.tunnelErr != nil {
		ts.Error = s.tunnelErr()
	}
	if ts.URL != "" {
		ts.Running = true
		ts.Callback = ts.URL + s.callbackPath
	}
	return ts
}

// handleHealth 深度健康检查（无鉴权，挂载在 r.GET("/api/health")，供 LB / 监控 / 告警调用）。
//
// HTTP 状态码契约（运维 / 监控据此配 alert）：
//   - 200 OK     : 服务进程活着 + 业务 DB 可读 + 账本表可读。
//   - 503 + JSON : 任一关键子项（db / ledger）失败；tunnel 失败不影响整体 200（隧道仅是支付回调通道，本地支付能力不受影响）。
func (s *Server) handleHealth(c *gin.Context) {
	ctx := c.Request.Context()

	resp := gin.H{"status": "ok", "checked_at": time.Now().Format(time.RFC3339)}
	if !s.startTime.IsZero() {
		resp["uptime_sec"] = int64(time.Since(s.startTime).Seconds())
	}

	criticalFail := false

	disk := gin.H{}
	if n, err := s.st.CountOrders(ctx, ""); err != nil {
		disk["ok"] = false
		disk["error"] = err.Error()
		criticalFail = true
	} else {
		disk["ok"] = true
		disk["orders"] = n
	}
	resp["db"] = disk

	led := gin.H{}
	if s.ledgerStore != nil {
		if n, err := s.ledgerStore.CountLedger(ctx); err != nil {
			led["ok"] = false
			led["error"] = err.Error()
			criticalFail = true
		} else {
			led["ok"] = true
			led["count"] = n
		}
	} else {
		led["ok"] = false
		led["error"] = "ledgerStore 未注入"
		criticalFail = true
	}
	resp["ledger"] = led

	tun := gin.H{}
	if s.tunnelHealth != nil {
		tun["healthy"] = s.tunnelHealth()
	}
	if s.tunnelURL != nil {
		tun["url"] = s.tunnelURL()
	}
	resp["tunnel"] = tun

	if criticalFail {
		resp["status"] = "error"
		c.JSON(http.StatusServiceUnavailable, resp)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// ========== 鉴权 ==========

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// authMiddleware 鉴权中间件，验证 JWT token
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
			return
		}
		// 去掉 "Bearer " 前缀
		if len(token) > 7 && token[:7] == "Bearer " {
			token = token[7:]
		}
		claims := &jwtClaims{}
		parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
			return s.jwtSecret, nil
		})
		if err != nil || !parsed.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token 无效或已过期"})
			return
		}
		c.Set("username", claims.Username)
		c.Next()
	}
}

type jwtClaims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

func (s *Server) handleLogin(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}

	// 失败退避限速：IP + username 双维度校验，已锁定直接 429
	if s.enforceLoginLimit(c, req.Username) {
		return
	}

	if s.adminSetupNeeded {
		// 首次启动尚未注册管理员，引导注册（与"登录未启用"区分，避免管理员困惑）
		c.JSON(http.StatusConflict, gin.H{"error": "管理员未注册，请先完成初始化"})
		return
	}
	if s.adminUser == "" || len(s.adminPass) == 0 || !strings.HasPrefix(string(s.adminPass), "$2") {
		// 后台登录被禁用（配置不全或非 bcrypt 哈希），统一返回 403 避免泄露配置状态
		c.JSON(http.StatusForbidden, gin.H{"error": "管理后台登录未启用"})
		return
	}
	if req.Username != s.adminUser {
		// 用户名错误也走 bcrypt 路径以保持固定时间，避免用户名枚举
		_ = bcrypt.CompareHashAndPassword(s.adminPass, []byte("00"+req.Password))
		s.recordLoginOutcome(c, req.Username, true)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}
	if err := bcrypt.CompareHashAndPassword(s.adminPass, []byte(req.Password)); err != nil {
		s.recordLoginOutcome(c, req.Username, true)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "用户名或密码错误"})
		return
	}

	s.recordLoginOutcome(c, req.Username, false)

	now := time.Now()
	claims := &jwtClaims{
		Username: req.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenExpiry)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "paygateway",
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
	if err != nil {
		logger.Error("admin", "token 生成失败", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成 token 失败"})
		return
	}
	logger.Info("admin", "登录成功", "username", req.Username)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"token":   token,
		"expires": now.Add(tokenExpiry).Unix(),
	}})
}

// ========== 首次启动注册 ==========

type registerReq struct {
	Username string `json:"username" binding:"required,min=3,max=32"`
	Password string `json:"password" binding:"required,min=8,max=72"`
}

// handleSetupStatus 查询是否需要首次注册（前端据此决定展示注册页 or 登录页）
func (s *Server) handleSetupStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"setup_needed": s.adminSetupNeeded}})
}

// handleRegister 首次启动注册管理员（bcrypt 哈希存入 settings 表）
// 安全：仅在尚无管理员凭据时可调用；注册成功后立即禁用
func (s *Server) handleRegister(c *gin.Context) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	// 双重校验：内存标志 + DB 实况（防内存竞态绕过导致重复注册覆盖凭据）
	if !s.adminSetupNeeded {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理员已配置，注册已关闭"})
		return
	}
	ctx := c.Request.Context()
	if user, err := s.st.GetSetting(ctx, "admin_username"); err == nil && user != "" {
		s.adminSetupNeeded = false
		c.JSON(http.StatusForbidden, gin.H{"error": "管理员已配置，注册已关闭"})
		return
	}
	var req registerReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误：用户名 3-32 位，密码至少 8 位"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "密码加密失败"})
		return
	}
	// 先写哈希再写用户名：任一失败都不留半配置（用户名无值 = 注册未完成）
	if err := s.st.SetSetting(ctx, "admin_password_hash", string(hash)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存管理员密码失败: " + err.Error()})
		return
	}
	if err := s.st.SetSetting(ctx, "admin_username", req.Username); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存管理员失败: " + err.Error()})
		return
	}
	// 立即生效：内存凭据切换为 DB 值，关闭注册入口
	s.adminUser = req.Username
	s.adminPass = hash
	s.adminSetupNeeded = false
	logger.Info("admin", "管理员注册成功", "username", req.Username)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ========== 重置管理员（设置页入口） ==========

type resetAdminReq struct {
	Password string `json:"password" binding:"required"`
}

// handleResetAdmin 重置管理员凭据：验证当前密码后清空 DB 与内存凭据，系统回到首次注册态。
// 安全：需登录 + 提交当前密码（防误操作/CSRF）；重置后所有已发 token 仍有效但登录校验立即切换到注册态，
// 前端收到 ok 后清除本地 token 并跳转注册页。重置动作记审计日志。
func (s *Server) handleResetAdmin(c *gin.Context) {
	if s.adminSetupNeeded {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前已是未注册状态，无需重置"})
		return
	}
	var req resetAdminReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	// 验证当前管理员密码（防未授权操作）
	if err := bcrypt.CompareHashAndPassword(s.adminPass, []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "当前密码错误"})
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	ctx := c.Request.Context()
	// 删除 DB 凭据
	_ = s.st.DeleteSetting(ctx, "admin_username")
	_ = s.st.DeleteSetting(ctx, "admin_password_hash")
	// 清空内存凭据，回到注册态
	s.adminUser = ""
	s.adminPass = nil
	s.adminSetupNeeded = true
	logger.Warn("admin", "管理员凭据已重置，系统回到首次注册态", "operator", c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// ========== Settings ==========

func (s *Server) handleSettings(c *gin.Context) {
	ctx := c.Request.Context()
	dbSettings, err := s.st.GetAllSettings(ctx)
	if err != nil {
		s.err(c, err)
		return
	}
	// 多商户模式：易支付商户凭据已迁移到「上游对接」页（upstreams.db），
	// 系统设置不再返回/管理 epay_pid/epay_key/epay_public_base_url
	delete(dbSettings, "epay_pid")
	delete(dbSettings, "epay_key")
	delete(dbSettings, "epay_public_base_url")
	// 管理员凭据哈希不得暴露给前端（仅 /api/auth/* 使用）
	delete(dbSettings, "admin_username")
	delete(dbSettings, "admin_password_hash")
	// 账本私钥 / QR token 等完整密钥不随常规设置下发，仅由 /api/settings/keys 确认后返回
	delete(dbSettings, "ledger_private_key")
	delete(dbSettings, "qr_token_secret")
	// 合并通道启用状态
	channels := channel.AllNames()
	channelEnabled := make(map[string]bool)
	for _, name := range channels {
		channelEnabled[name] = channel.IsEnabled(name)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"settings":        dbSettings,
		"channel_enabled": channelEnabled,
		"channels":        channels,
	}})
}

// handleSettingsKeys 返回安全密钥状态与完整值（需登录鉴权）。
// 仅返回有实际意义的密钥：账本私钥 / QR token 密钥；调用即记审计日志。
func (s *Server) handleSettingsKeys(c *gin.Context) {
	ctx := c.Request.Context()
	dbSettings, err := s.st.GetAllSettings(ctx)
	if err != nil {
		s.err(c, err)
		return
	}
	keys := gin.H{}
	if v, ok := dbSettings["ledger_private_key"]; ok && v != "" {
		keys["ledger_private_key"] = v
	}
	if v, ok := dbSettings["qr_token_secret"]; ok && v != "" {
		keys["qr_token_secret"] = v
	}
	logger.Info("admin", "安全密钥被查看（明文）", "operator", c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"data": keys})
}

type settingsUpdate struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *Server) handleUpdateSettings(c *gin.Context) {
	var req struct {
		Settings []settingsUpdate `json:"settings"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	ctx := c.Request.Context()
	for _, item := range req.Settings {
		// 多商户模式：易支付商户凭据一律禁止通过系统设置保存（只认「上游对接」页）
		switch item.Key {
		case "epay_pid", "epay_key", "epay_public_base_url":
			logger.Warn("admin", "拒绝保存已废弃的易支付系统设置", "key", item.Key)
			continue
		}
		if err := s.st.SetSetting(ctx, item.Key, item.Value); err != nil {
			s.err(c, err)
			return
		}
		// 通知主进程更新运行时配置（如通道环境切换）
		if s.onSettingsChange != nil {
			s.onSettingsChange(item.Key, item.Value)
		}
	}
	logger.Info("admin", "设置已更新", "count", len(req.Settings))
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

func (s *Server) handleToggleChannel(c *gin.Context) {
	name := c.Param("name")
	on := !channel.IsEnabled(name)
	channel.SetEnabled(name, on)
	// 持久化到 settings 表
	ctx := c.Request.Context()
	val := "false"
	if on {
		val = "true"
	}
	_ = s.st.SetSetting(ctx, "channel_enabled_"+name, val)
	logger.Info("admin", "通道启停", "channel", name, "enabled", on)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": name, "enabled": on}})
}

// handleToggleChannelMode 开启/关闭通道的某个支付模式
func (s *Server) handleToggleChannelMode(c *gin.Context) {
	name := c.Param("name")
	mode := c.Param("mode")
	ch, ok := channel.Lookup(name)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "通道不存在"})
		return
	}
	// 通过类型断言判断是否支持模式切换
	type modeToggler interface {
		IsModeEnabled(mode string) bool
		SetModeEnabled(mode string, enabled bool)
		EnabledModes() map[string]bool
	}
	toggler, ok := ch.(modeToggler)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该通道不支持模式切换"})
		return
	}
	on := !toggler.IsModeEnabled(mode)
	toggler.SetModeEnabled(mode, on)
	// 持久化到 settings 表
	ctx := c.Request.Context()
	val := "false"
	if on {
		val = "true"
	}
	_ = s.st.SetSetting(ctx, "alipay_mode_"+mode, val)
	logger.Info("admin", "通道支付模式启停", "channel", name, "mode", mode, "enabled", on)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"channel": name, "mode": mode, "enabled": on}})
}

// handleDeviceStream 实时设备流 WebSocket（云手机式操作）
// GET /api/personal/devices/stream?serial=127.0.0.1:5556&token=<jwt>
// token 走 query（浏览器 WebSocket 无法自定义 header）
func (s *Server) handleDeviceStream(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	claims := &jwtClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		return s.jwtSecret, nil
	})
	if err != nil || !parsed.Valid {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token 无效或已过期"})
		return
	}
	if s.streamSrv == nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "实时流未启用"})
		return
	}
	s.streamSrv.HandleStream(c.Writer, c.Request)
}

func (s *Server) err(c *gin.Context, err error) {
	logger.Error("admin", "API错误", "err", err, "path", c.Request.URL.Path)
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
