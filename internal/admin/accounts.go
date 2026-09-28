// Package admin 管理后台：账户池管理 API
//
// 安全设计：
//   - 本文件是唯一访问 accounts.db 的组件
//   - 敏感字段（private_key / public_key / api_key）AES-256-GCM 加密后落库
//   - 解密后的账户数据推送到 AccountPool（内存），供其他模块使用
//   - API 响应不返回明文密钥（仅 "****" + 后4位）
//   - 唯一例外：GET /accounts/:id/secret（需登录，供管理后台「查看/复制完整密钥」），返回解密后的明文
package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"paygateway/internal/account"
	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/store"
)

// AccountHandler 账户管理处理器
type AccountHandler struct {
	accountDB *store.AccountDB // 唯一访问 accounts.db 的组件
	pool      *account.AccountPool
	crypto    *accountCrypto // 加密/解密工具
}

// accountCrypto 封装 AES 加解密（避免直接暴露 account 包的全局函数）
type accountCrypto struct {
	init   bool
	encFn  func([]byte) (string, error)
	decFn  func(string) ([]byte, error)
	encJFn func(any) (string, error)
	decJFn func(string, any) error
}

func newAccountCrypto(masterKey string) (*accountCrypto, error) {
	if err := account.InitCrypto(masterKey); err != nil {
		return nil, fmt.Errorf("账户加密初始化失败: %w", err)
	}
	return &accountCrypto{
		init:   true,
		encFn:  account.Encrypt,
		decFn:  account.Decrypt,
		encJFn: account.EncryptJSON,
		decJFn: account.DecryptJSON,
	}, nil
}

func (c *accountCrypto) encrypt(plaintext []byte) (string, error) { return c.encFn(plaintext) }
func (c *accountCrypto) decrypt(hex string) ([]byte, error)       { return c.decFn(hex) }

// NewAccountHandler 创建账户管理处理器
func NewAccountHandler(db *store.AccountDB, pool *account.AccountPool, masterKey string) (*AccountHandler, error) {
	crypto, err := newAccountCrypto(masterKey)
	if err != nil {
		return nil, err
	}
	return &AccountHandler{
		accountDB: db,
		pool:      pool,
		crypto:    crypto,
	}, nil
}

// InitFromDB 启动时从 accounts.db 加载所有账户，解密后初始化 AccountPool
func (h *AccountHandler) InitFromDB(ctx context.Context) error {
	encrypted, err := h.accountDB.ListAccounts(ctx, "")
	if err != nil {
		return fmt.Errorf("加载账户列表失败: %w", err)
	}
	var decryptErrs []string
	for _, e := range encrypted {
		acct, err := h.decryptAccount(e)
		if err != nil {
			logger.Error("account", "解密账户失败（主密钥不一致或数据损坏）",
				"id", e.ID, "name", e.Name, "err", err)
			decryptErrs = append(decryptErrs, fmt.Sprintf("id=%d name=%s: %v", e.ID, e.Name, err))
			continue
		}
		h.pool.Add(acct)
	}
	// P1 守卫：DB 非空但所有/部分记录解密失败 = 主密钥变更或数据损坏，禁止静默启动（否则网关收款链路全断）
	if len(decryptErrs) > 0 {
		return fmt.Errorf("accounts.db 解密失败 %d/%d 条（疑似 ACCOUNT_MASTER_KEY 变更），已拒绝启动；请恢复 .env 中的 ACCOUNT_MASTER_KEY 或清空 accounts.db 后重新建号。失败明细:\n%s",
			len(decryptErrs), len(encrypted), strings.Join(decryptErrs, "\n"))
	}
	logger.Info("account", "账户池初始化完成", "count", h.pool.Count())
	return nil
}

// RegisterRoutes 注册账户管理路由（需在 authMiddleware 组内）
func (h *AccountHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/accounts", h.handleListAccounts)
	rg.GET("/accounts/:id", h.handleGetAccount)
	rg.POST("/accounts", h.handleCreateAccount)
	rg.PUT("/accounts/:id", h.handleUpdateAccount)
	rg.DELETE("/accounts/:id", h.handleDeleteAccount)
	rg.POST("/accounts/:id/freeze", h.handleFreezeAccount)
	rg.POST("/accounts/:id/unfreeze", h.handleUnfreezeAccount)
	rg.POST("/accounts/:id/enable", h.handleEnableAccount)
	rg.POST("/accounts/:id/disable", h.handleDisableAccount)
	rg.GET("/accounts/:id/logs", h.handleAccountLogs)
	rg.GET("/accounts/:id/secret", h.handleAccountSecret)
	// 收款码图片解析（docs/MIANQIAN.md §7.6 阶段一：上传收款码 → 服务端解析 → 绑定账户）
	rg.POST("/accounts/decode-qrcode", h.handleDecodeQRCode)
}

// ========== API Handlers ==========

// AccountResponse 前端展示的账户信息（密钥脱敏）
type AccountResponse struct {
	ID           int64  `json:"id"`
	Channel      string `json:"channel"`
	Name         string `json:"name"`
	AppID        string `json:"app_id"`
	MchID        string `json:"mch_id,omitempty"`
	PrivateKey   string `json:"private_key"`          // 脱敏显示
	PublicKey    string `json:"public_key,omitempty"` // 脱敏显示
	ApiKey       string `json:"api_key,omitempty"`    // 脱敏显示
	QRContent    string `json:"qr_content,omitempty"` // 收款码内容（脱敏，个人通道）
	Weight       int    `json:"weight"`
	Priority     int    `json:"priority"`
	Status       string `json:"status"`
	FrozenReason string `json:"frozen_reason,omitempty"` // 冻结原因（offline/risk/manual/retire；前端展示用）
	AutoBan      bool   `json:"auto_ban"`
	DailyLimit   int64  `json:"daily_limit"`
	DailyUsed    int64  `json:"daily_used"` // 今日已用（0 必须返回，omitempty 会导致前端 NaN）
	FailCount    int    `json:"fail_count"` // 连续失败次数（同上，0 必须返回）
	Group        string `json:"group"`
	IsProd       bool   `json:"is_prod"`
	Remark       string `json:"remark,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// maskKey 脱敏：只显示后 4 位；空值返回空串（前端据此显示「未配置」）
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

// maskQR 收款码内容脱敏：只显示后 8 位（含 https:// 前缀时截断显示）；空值返回空串
func maskQR(qr string) string {
	if qr == "" {
		return ""
	}
	if len(qr) <= 8 {
		return "****"
	}
	return "****" + qr[len(qr)-8:]
}

// acctToResponse 将 Account 转为前端响应（脱敏密钥）
func acctToResponse(acct *account.Account) *AccountResponse {
	return &AccountResponse{
		ID:           acct.ID,
		Channel:      acct.Channel,
		Name:         acct.Name,
		AppID:        acct.AppID,
		MchID:        acct.MchID,
		PrivateKey:   maskKey(acct.PrivateKey),
		PublicKey:    maskKey(acct.PublicKey),
		ApiKey:       maskKey(acct.ApiKey),
		QRContent:    maskQR(acct.QRContent),
		Weight:       acct.Weight,
		Priority:     acct.Priority,
		Status:       string(acct.Status),
		FrozenReason: acct.FrozenReason,
		AutoBan:      acct.AutoBan,
		DailyLimit:   acct.DailyLimit,
		DailyUsed:    acct.DailyUsed,
		FailCount:    acct.FailCount,
		Group:        acct.Group,
		IsProd:       acct.IsProd,
		Remark:       acct.Remark,
		CreatedAt:    acct.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    acct.UpdatedAt.Format(time.RFC3339),
	}
}

// encryptAccount 加密账户的敏感字段，返回 AccountEncrypted（用于落库）
func (h *AccountHandler) EncryptAccount(acct *account.Account) (*account.AccountEncrypted, error) {
	e := &account.AccountEncrypted{
		Channel:      acct.Channel,
		Name:         acct.Name,
		AppID:        acct.AppID,
		MchID:        acct.MchID,
		Weight:       acct.Weight,
		Priority:     acct.Priority,
		Status:       string(acct.Status),
		FrozenReason: acct.FrozenReason,
		AutoBan:      acct.AutoBan,
		DailyLimit:   acct.DailyLimit,
		DailyUsed:    acct.DailyUsed,
		Group:        acct.Group,
		IsProd:       acct.IsProd,
		Remark:       acct.Remark,
		CreatedAt:    acct.CreatedAt,
		UpdatedAt:    acct.UpdatedAt,
	}
	var err error
	if acct.PrivateKey != "" {
		e.PrivateKeyEnc, err = h.crypto.encrypt([]byte(acct.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("加密应用私钥失败: %w", err)
		}
	}
	if acct.PublicKey != "" {
		e.PublicKeyEnc, err = h.crypto.encrypt([]byte(acct.PublicKey))
		if err != nil {
			return nil, fmt.Errorf("加密公钥失败: %w", err)
		}
	}
	if acct.ApiKey != "" {
		e.ApiKeyEnc, err = h.crypto.encrypt([]byte(acct.ApiKey))
		if err != nil {
			return nil, fmt.Errorf("加密 API 密钥失败: %w", err)
		}
	}
	if acct.QRContent != "" {
		e.QRContentEnc, err = h.crypto.encrypt([]byte(acct.QRContent))
		if err != nil {
			return nil, fmt.Errorf("加密收款码内容失败: %w", err)
		}
	}
	return e, nil
}

// decryptAccount 解密账户的敏感字段，返回 Account（用于内存）
func (h *AccountHandler) decryptAccount(e *account.AccountEncrypted) (*account.Account, error) {
	acct := &account.Account{
		ID:           e.ID,
		Channel:      e.Channel,
		Name:         e.Name,
		AppID:        e.AppID,
		MchID:        e.MchID,
		Weight:       e.Weight,
		Priority:     e.Priority,
		Status:       account.AccountStatus(e.Status),
		FrozenReason: e.FrozenReason,
		AutoBan:      e.AutoBan,
		DailyLimit:   e.DailyLimit,
		DailyUsed:    e.DailyUsed,
		Group:        e.Group,
		IsProd:       e.IsProd,
		Remark:       e.Remark,
		CreatedAt:    e.CreatedAt,
		UpdatedAt:    e.UpdatedAt,
	}
	if e.PrivateKeyEnc != "" {
		data, err := h.crypto.decrypt(e.PrivateKeyEnc)
		if err != nil {
			return nil, fmt.Errorf("解密应用私钥失败: %w", err)
		}
		acct.PrivateKey = string(data)
	}
	if e.PublicKeyEnc != "" {
		data, err := h.crypto.decrypt(e.PublicKeyEnc)
		if err != nil {
			return nil, fmt.Errorf("解密公钥失败: %w", err)
		}
		acct.PublicKey = string(data)
	}
	if e.ApiKeyEnc != "" {
		data, err := h.crypto.decrypt(e.ApiKeyEnc)
		if err != nil {
			return nil, fmt.Errorf("解密 API 密钥失败: %w", err)
		}
		acct.ApiKey = string(data)
	}
	if e.QRContentEnc != "" {
		data, err := h.crypto.decrypt(e.QRContentEnc)
		if err != nil {
			return nil, fmt.Errorf("解密收款码内容失败: %w", err)
		}
		acct.QRContent = string(data)
	}
	return acct, nil
}

// ========== List ==========

func (h *AccountHandler) handleListAccounts(c *gin.Context) {
	channel := c.Query("channel")

	// 从内存池取（已包含运行状态：daily_used, fail_count, frozen_until）
	poolAccts := h.pool.ListAll()
	if channel != "" {
		poolAccts = h.pool.ListByChannel(channel)
	}

	resp := make([]*AccountResponse, 0, len(poolAccts))
	for _, acct := range poolAccts {
		resp = append(resp, acctToResponse(acct))
	}

	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// ========== Get ==========

func (h *AccountHandler) handleGetAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}

	// 先从内存池取
	acct := h.pool.GetByID(id)
	if acct == nil {
		// 再查数据库（可能被删了但池没更新）
		enc, err := h.accountDB.GetAccount(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if enc == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账户不存在"})
			return
		}
		acct, err = h.decryptAccount(enc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解密账户失败"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": acctToResponse(acct)})
}

// handleAccountSecret 返回账户完整敏感字段（明文，需登录）。
// 用途：管理后台「查看/复制完整密钥」，用于确认对接配置是否准确、复制真实可用的密钥给外部系统。
// 安全提示：仅登录用户可调用；前端展示时应二次确认并提示勿泄露。
func (h *AccountHandler) handleAccountSecret(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}

	// 先从内存池取（明文），未命中再回退 DB 解密
	acct := h.pool.GetByID(id)
	if acct == nil {
		enc, err := h.accountDB.GetAccount(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if enc == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账户不存在"})
			return
		}
		acct, err = h.decryptAccount(enc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解密账户失败"})
			return
		}
	}

	logger.Info("account", "账户密钥被查看（明文）", "id", id, "name", acct.Name, "channel", acct.Channel, "operator", c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":          acct.ID,
		"name":        acct.Name,
		"channel":     acct.Channel,
		"app_id":      acct.AppID,
		"mch_id":      acct.MchID,
		"private_key": acct.PrivateKey,
		"public_key":  acct.PublicKey,
		"api_key":     acct.ApiKey,
		"qr_content":  acct.QRContent,
	}})
}

// ========== Create ==========

// 账户通道白名单（前后端差异化：商家=密钥，个人=收款码）
var validAccountChannels = map[string]bool{
	"alipay":          true, // 商家·支付宝（gopay 官方 API）
	"wechat":          true, // 商家·微信（gopay 官方 API）
	"qq":              true, // 商家·QQ（预留）
	"paypal":          true, // 商家·PayPal（预留）
	"personal.alipay": true, // 个人·支付宝（收款码监听）
	"personal.wechat": true, // 个人·微信（收款码监听）
	"personal.qq":     true, // 个人·QQ（预留）
}

// isPersonalChannel 是否个人免签通道（收款码监听类）
func isPersonalChannel(ch string) bool {
	return strings.HasPrefix(ch, "personal.")
}

// validateAccountReq 按通道类型校验账户字段（docs/MIANQIAN.md 通道大类差异化）
//   - 个人通道（personal.*）：必须配置收款码内容 qr_content（无官方 API 密钥）
//   - 商家通道（merchant.*）：必须配置 app_id（官方 API 必需），密钥二选一
func validateAccountReq(req createAccountReq) error {
	if !validAccountChannels[req.Channel] {
		return fmt.Errorf("不支持的通道类型: %s（可选：alipay/wechat/qq/paypal/personal.alipay/personal.wechat/personal.qq）", req.Channel)
	}
	if isPersonalChannel(req.Channel) {
		// 个人免签：收款码是核心凭据，必须配置
		if strings.TrimSpace(req.QRContent) == "" {
			return fmt.Errorf("个人免签通道必须配置收款码内容（qr_content），可从收款码图片上传自动解析")
		}
		// 个人通道不应携带官方 API 密钥（避免混淆）
		if req.PrivateKey != "" || req.ApiKey != "" || req.AppID != "" {
			return fmt.Errorf("个人免签通道无需配置官方密钥/AppID，请仅填写收款码内容")
		}
	} else {
		// 商家通道：官方 API 必须配置 AppID
		if strings.TrimSpace(req.AppID) == "" {
			return fmt.Errorf("商家通道必须配置 AppID（官方 API 凭据）")
		}
		if strings.TrimSpace(req.PrivateKey) == "" && strings.TrimSpace(req.ApiKey) == "" {
			return fmt.Errorf("商家通道必须配置应用私钥或 API 密钥（至少一项）")
		}
	}
	return nil
}

// validateAccountReqUpdate 账户编辑校验：与 validateAccountReq 一致，但个人通道允许
// qr_content 为空——空表示「沿用已有收款码」（编辑场景，资金安全由调用方 handleUpdateAccount
// 的复用逻辑保证）。若编辑时填写了新的收款码，则必须通过 InvalidQRContent 校验（脏数据防线）。
func validateAccountReqUpdate(req createAccountReq) error {
	if !validAccountChannels[req.Channel] {
		return fmt.Errorf("不支持的通道类型: %s（可选：alipay/wechat/qq/paypal/personal.alipay/personal.wechat/personal.qq）", req.Channel)
	}
	if isPersonalChannel(req.Channel) {
		// 编辑场景：收款码可为空（沿用已有）。
		if strings.TrimSpace(req.QRContent) != "" {
			if reason := account.InvalidQRContent(req.Channel, strings.TrimSpace(req.QRContent)); reason != "" {
				return fmt.Errorf("收款码内容非法（%s），请重新上传解析", reason)
			}
		}
		// 个人通道不应携带官方 API 密钥（避免混淆；更新时也保持该约束）
		if req.PrivateKey != "" || req.ApiKey != "" || req.AppID != "" {
			return fmt.Errorf("个人免签通道无需配置官方密钥/AppID，请仅填写收款码内容")
		}
	} else {
		// 商家通道：官方 API 必须配置 AppID
		if strings.TrimSpace(req.AppID) == "" {
			return fmt.Errorf("商家通道必须配置 AppID（官方 API 凭据）")
		}
		if strings.TrimSpace(req.PrivateKey) == "" && strings.TrimSpace(req.ApiKey) == "" {
			return fmt.Errorf("商家通道必须配置应用私钥或 API 密钥（至少一项）")
		}
	}
	return nil
}

type createAccountReq struct {
	Channel    string `json:"channel" binding:"required"`
	Name       string `json:"name" binding:"required"`
	AppID      string `json:"app_id"`
	MchID      string `json:"mch_id"`
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	ApiKey     string `json:"api_key"`
	QRContent  string `json:"qr_content"`
	Weight     int    `json:"weight"`
	Priority   int    `json:"priority"`
	AutoBan    bool   `json:"auto_ban"`
	DailyLimit int64  `json:"daily_limit"`
	Group      string `json:"group"`
	IsProd     bool   `json:"is_prod"`
	Remark     string `json:"remark"`
}

func (h *AccountHandler) handleCreateAccount(c *gin.Context) {
	var req createAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	// 按通道类型校验（商家=官方密钥 / 个人=收款码）
	if err := validateAccountReq(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Weight <= 0 {
		req.Weight = 50
	}
	if req.Group == "" {
		req.Group = "default"
	}

	now := time.Now()
	acct := &account.Account{
		Channel:    req.Channel,
		Name:       req.Name,
		AppID:      req.AppID,
		MchID:      req.MchID,
		PrivateKey: req.PrivateKey,
		PublicKey:  req.PublicKey,
		ApiKey:     req.ApiKey,
		QRContent:  req.QRContent,
		Weight:     req.Weight,
		Priority:   req.Priority,
		Status:     account.StatusNormal,
		AutoBan:    req.AutoBan,
		DailyLimit: req.DailyLimit,
		Group:      req.Group,
		IsProd:     req.IsProd,
		Remark:     req.Remark,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// 加密敏感字段
	enc, err := h.EncryptAccount(acct)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 写入 accounts.db（仅 Admin API 可访问）
	id, err := h.accountDB.CreateAccount(c.Request.Context(), enc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建账户失败: " + err.Error()})
		return
	}
	acct.ID = id

	// 推送到内存池（其他模块立即可用）
	h.pool.Add(acct)

	// 审计日志
	_ = h.accountDB.InsertAccountLog(c.Request.Context(), &account.AccountLog{
		AccountID: id,
		Action:    "create",
		Detail:    fmt.Sprintf("创建%s账户: %s", req.Channel, req.Name),
		Operator:  c.GetString("username"),
	})

	logger.Info("account", "账户创建成功", "id", id, "name", req.Name, "channel", req.Channel)
	c.JSON(http.StatusOK, gin.H{"data": acctToResponse(acct)})
}

// ========== Update ==========

type updateAccountReq struct {
	Channel    string `json:"channel" binding:"required"`
	Name       string `json:"name" binding:"required"`
	AppID      string `json:"app_id"`
	MchID      string `json:"mch_id"`
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
	ApiKey     string `json:"api_key"`
	QRContent  string `json:"qr_content"`
	Weight     int    `json:"weight"`
	Priority   int    `json:"priority"`
	AutoBan    bool   `json:"auto_ban"`
	DailyLimit int64  `json:"daily_limit"`
	Group      string `json:"group"`
	IsProd     bool   `json:"is_prod"`
	Remark     string `json:"remark"`
}

func (h *AccountHandler) handleUpdateAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}

	var req updateAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	// 按通道类型校验（编辑场景：个人通道允许 qr_content 为空 → 沿用已有；新填须合法）
	if err := validateAccountReqUpdate(createAccountReq(req)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Group == "" {
		req.Group = "default"
	}

	now := time.Now()
	acct := &account.Account{
		ID:         id,
		Channel:    req.Channel,
		Name:       req.Name,
		AppID:      req.AppID,
		MchID:      req.MchID,
		PrivateKey: req.PrivateKey,
		PublicKey:  req.PublicKey,
		ApiKey:     req.ApiKey,
		QRContent:  req.QRContent,
		Weight:     req.Weight,
		Priority:   req.Priority,
		AutoBan:    req.AutoBan,
		DailyLimit: req.DailyLimit,
		Group:      req.Group,
		IsProd:     req.IsProd,
		Remark:     req.Remark,
		UpdatedAt:  now,
	}

	// 保留原有状态（先从内存池读，池未命中再回退 DB；二者均无则账户不存在）
	existing := h.pool.GetByID(id)
	if existing == nil {
		// 池未命中：从 DB 拉取并解密，避免静默用零值覆盖 CreatedAt/状态/密钥
		enc, err := h.accountDB.GetAccount(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询账户失败: " + err.Error()})
			return
		}
		if enc == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "账户不存在"})
			return
		}
		existing, err = h.decryptAccount(enc)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "解密账户失败"})
			return
		}
	}
	acct.Status = existing.Status
	acct.DailyUsed = existing.DailyUsed
	acct.FailCount = existing.FailCount
	acct.FrozenUntil = existing.FrozenUntil
	acct.CreatedAt = existing.CreatedAt
	// 密钥字段前端不传（空）时保留原值；qr_content 额外跳过脱敏占位（"****" 前缀 = 回显值，非真实内容）
	if req.PrivateKey == "" {
		acct.PrivateKey = existing.PrivateKey
	}
	if req.PublicKey == "" {
		acct.PublicKey = existing.PublicKey
	}
	if req.ApiKey == "" {
		acct.ApiKey = existing.ApiKey
	}
	if req.QRContent == "" || strings.HasPrefix(req.QRContent, "****") {
		acct.QRContent = existing.QRContent
	}

	// 加密敏感字段
	enc, err := h.EncryptAccount(acct)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	enc.ID = id

	// 更新 accounts.db
	if err := h.accountDB.UpdateAccount(c.Request.Context(), enc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新账户失败: " + err.Error()})
		return
	}

	// 更新内存池
	h.pool.Update(acct)

	// 清除对应商家通道的 gopay client 缓存（app_id/密钥变更后旧缓存会用旧参数下单）
	invalidateChannelClient(acct.Channel, id)

	// 审计日志
	_ = h.accountDB.InsertAccountLog(c.Request.Context(), &account.AccountLog{
		AccountID: id,
		Action:    "update",
		Detail:    fmt.Sprintf("更新%s账户: %s", req.Channel, req.Name),
		Operator:  c.GetString("username"),
	})

	logger.Info("account", "账户更新成功", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": acctToResponse(acct)})
}

// invalidateChannelClient 清除账户对应商家通道的 gopay client 缓存。
// 账户 app_id / 密钥变更后，缓存的旧 client 仍用旧参数下单（旧 app_id 会导致支付宝
// 返回 invalid-app-id），必须强制重建。个人免签通道无 gopay client，无需处理。
func invalidateChannelClient(accountChannel string, accountID int64) {
	name := "merchant." + strings.TrimPrefix(accountChannel, "merchant.")
	if strings.HasPrefix(accountChannel, "personal.") {
		return
	}
	ch, ok := channel.Lookup(name)
	if !ok {
		return
	}
	if inv, ok := ch.(channel.ClientCacheInvalidator); ok {
		inv.RemoveClient(accountID)
	}
}

// ========== Delete ==========

func (h *AccountHandler) handleDeleteAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}

	ctx := c.Request.Context()

	// 删除前先取账户（用于删除后清除通道缓存）
	var delChannel string
	if enc, err := h.accountDB.GetAccount(ctx, id); err == nil && enc != nil {
		delChannel = enc.Channel
	}

	// 删除数据库
	if err := h.accountDB.DeleteAccount(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除账户失败: " + err.Error()})
		return
	}

	// 移除内存池
	h.pool.Remove(id)

	// 清除商家通道的 client 缓存，避免残留
	if delChannel != "" {
		invalidateChannelClient(delChannel, id)
	}

	// 审计日志
	_ = h.accountDB.InsertAccountLog(ctx, &account.AccountLog{
		AccountID: id,
		Action:    "delete",
		Detail:    fmt.Sprintf("删除账户 ID: %d", id),
		Operator:  c.GetString("username"),
	})

	logger.Info("account", "账户已删除", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// ========== Freeze / Unfreeze / Enable / Disable ==========

func (h *AccountHandler) handleFreezeAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}
	ctx := c.Request.Context()

	if err := h.accountDB.SetAccountFrozen(ctx, id, account.FrozenReasonManual); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.FreezeManual(id)

	_ = h.accountDB.InsertAccountLog(ctx, &account.AccountLog{
		AccountID: id, Action: "freeze", Detail: "手动冻结", Operator: c.GetString("username"),
	})
	logger.Info("account", "账户已冻结", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

func (h *AccountHandler) handleUnfreezeAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}
	ctx := c.Request.Context()

	// 人工解冻：任意冻结原因都可解冻（SetAccountStatus 同时清空 frozen_reason）
	if err := h.accountDB.SetAccountStatus(ctx, id, string(account.StatusNormal)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.SetStatus(id, account.StatusNormal)

	_ = h.accountDB.InsertAccountLog(ctx, &account.AccountLog{
		AccountID: id, Action: "unfreeze", Detail: "手动解冻", Operator: c.GetString("username"),
	})
	logger.Info("account", "账户已解冻", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

func (h *AccountHandler) handleEnableAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}
	ctx := c.Request.Context()

	if err := h.accountDB.SetAccountStatus(ctx, id, string(account.StatusNormal)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.SetStatus(id, account.StatusNormal)

	_ = h.accountDB.InsertAccountLog(ctx, &account.AccountLog{
		AccountID: id, Action: "enable", Detail: "手动启用", Operator: c.GetString("username"),
	})
	logger.Info("account", "账户已启用", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

func (h *AccountHandler) handleDisableAccount(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}
	ctx := c.Request.Context()

	if err := h.accountDB.SetAccountStatus(ctx, id, string(account.StatusDisabled)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.SetStatus(id, account.StatusDisabled)

	_ = h.accountDB.InsertAccountLog(ctx, &account.AccountLog{
		AccountID: id, Action: "disable", Detail: "手动禁用", Operator: c.GetString("username"),
	})
	logger.Info("account", "账户已禁用", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// ========== Account Logs ==========

// AccountName 返回账户池中指定账户的名称（订单列表展示用；查不到返回空）
func (h *AccountHandler) AccountName(id int64) string {
	if h == nil || id <= 0 {
		return ""
	}
	acct := h.pool.GetByID(id)
	if acct == nil {
		return ""
	}
	return acct.Name
}

func (h *AccountHandler) handleAccountLogs(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效账户ID"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	logs, err := h.accountDB.ListAccountLogs(c.Request.Context(), id, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": logs})
}
