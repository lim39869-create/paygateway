// Package admin 管理后台：上游对接方管理 API
//
// 安全设计：
//   - 本文件是唯一访问 upstreams.db 的组件
//   - 敏感字段（易支付密钥 key）AES-256-GCM 加密后落库
//   - 解密后的上游数据推送到 UpstreamPool（内存），供 epay 验签/回调签名使用
//   - 常规 API 响应不返回明文密钥（仅 "****" + 后4位）
//   - 唯一例外：GET /upstreams/:id/secret（需登录，供管理后台「查看/复制完整密钥」），返回解密后的明文 key
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
	"paygateway/internal/logger"
	"paygateway/internal/store"
	"paygateway/internal/upstream"
)

// UpstreamHandler 上游管理处理器
type UpstreamHandler struct {
	upstreamDB *store.UpstreamDB // 唯一访问 upstreams.db 的组件
	pool       *upstream.UpstreamPool
	initCrypto func(string) error // 初始化加密模块（与账户共用主密钥）
	encFn      func([]byte) (string, error)
	decFn      func(string) ([]byte, error)
}

// NewUpstreamHandler 创建上游管理处理器
func NewUpstreamHandler(db *store.UpstreamDB, pool *upstream.UpstreamPool, masterKey string) (*UpstreamHandler, error) {
	// 复用 account 包的 AES-256-GCM 加密能力（主密钥与账户池一致）
	if err := account.InitCrypto(masterKey); err != nil {
		return nil, fmt.Errorf("上游加密初始化失败: %w", err)
	}
	return &UpstreamHandler{
		upstreamDB: db,
		pool:       pool,
		initCrypto: account.InitCrypto,
		encFn:      account.Encrypt,
		decFn:      account.Decrypt,
	}, nil
}

// InitFromDB 启动时从 upstreams.db 加载所有上游，解密后初始化 UpstreamPool
func (h *UpstreamHandler) InitFromDB(ctx context.Context) error {
	encrypted, err := h.upstreamDB.ListUpstreams(ctx)
	if err != nil {
		return fmt.Errorf("加载上游列表失败: %w", err)
	}
	var decryptErrs []string
	for _, e := range encrypted {
		up, err := h.decryptUpstream(e)
		if err != nil {
			logger.Error("upstream", "解密上游失败（主密钥不一致或数据损坏）",
				"id", e.ID, "name", e.Name, "err", err)
			decryptErrs = append(decryptErrs, fmt.Sprintf("id=%d name=%s: %v", e.ID, e.Name, err))
			continue
		}
		h.pool.Add(up)
	}
	// P1 守卫：DB 非空但所有/部分记录解密失败 = 主密钥变更或数据损坏，禁止静默启动（否则网关下单链路全断）
	if len(decryptErrs) > 0 {
		return fmt.Errorf("upstreams.db 解密失败 %d/%d 条（疑似 ACCOUNT_MASTER_KEY 变更），已拒绝启动；请恢复 .env 中的 ACCOUNT_MASTER_KEY 或清空 upstreams.db 后重新对接。失败明细:\n%s",
			len(decryptErrs), len(encrypted), strings.Join(decryptErrs, "\n"))
	}
	logger.Info("upstream", "上游池初始化完成", "count", h.pool.Count())
	return nil
}

// RegisterRoutes 注册上游管理路由（需在 authMiddleware 组内）
func (h *UpstreamHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/upstreams", h.handleListUpstreams)
	rg.GET("/upstreams/:id", h.handleGetUpstream)
	rg.POST("/upstreams", h.handleCreateUpstream)
	rg.PUT("/upstreams/:id", h.handleUpdateUpstream)
	rg.DELETE("/upstreams/:id", h.handleDeleteUpstream)
	rg.POST("/upstreams/:id/enable", h.handleEnableUpstream)
	rg.POST("/upstreams/:id/disable", h.handleDisableUpstream)
	rg.GET("/upstreams/:id/logs", h.handleUpstreamLogs)
	rg.GET("/upstreams/:id/secret", h.handleUpstreamSecret)
}

// ========== API Handlers ==========

// UpstreamResponse 前端展示的上游信息（密钥脱敏）
type UpstreamResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Pid           string `json:"pid"`
	Key           string `json:"key"` // 脱敏显示
	BaseURL       string `json:"base_url"`
	NotifyDomains string `json:"notify_domains"`
	Status        string `json:"status"`
	Remark        string `json:"remark,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// upstreamToResponse 将 Upstream 转为前端响应（脱敏密钥）
func upstreamToResponse(up *upstream.Upstream) *UpstreamResponse {
	return &UpstreamResponse{
		ID:            up.ID,
		Name:          up.Name,
		Pid:           up.Pid,
		Key:           maskKey(up.Key),
		BaseURL:       up.BaseURL,
		NotifyDomains: up.NotifyDomains,
		Status:        string(up.Status),
		Remark:        up.Remark,
		CreatedAt:     up.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     up.UpdatedAt.Format(time.RFC3339),
	}
}

// encryptUpstream 加密上游的敏感字段，返回 UpstreamEncrypted（用于落库）
func (h *UpstreamHandler) encryptUpstream(up *upstream.Upstream) (*upstream.UpstreamEncrypted, error) {
	e := &upstream.UpstreamEncrypted{
		Name:          up.Name,
		Pid:           up.Pid,
		BaseURL:       up.BaseURL,
		NotifyDomains: up.NotifyDomains,
		Status:        string(up.Status),
		Remark:        up.Remark,
		CreatedAt:     up.CreatedAt,
		UpdatedAt:     up.UpdatedAt,
	}
	var err error
	if up.Key != "" {
		e.KeyEnc, err = h.encFn([]byte(up.Key))
		if err != nil {
			return nil, fmt.Errorf("加密易支付密钥失败: %w", err)
		}
	}
	return e, nil
}

// decryptUpstream 解密上游的敏感字段，返回 Upstream（用于内存）
func (h *UpstreamHandler) decryptUpstream(e *upstream.UpstreamEncrypted) (*upstream.Upstream, error) {
	up := &upstream.Upstream{
		ID:            e.ID,
		Name:          e.Name,
		Pid:           e.Pid,
		BaseURL:       e.BaseURL,
		NotifyDomains: e.NotifyDomains,
		Status:        upstream.UpstreamStatus(e.Status),
		Remark:        e.Remark,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
	if e.KeyEnc != "" {
		data, err := h.decFn(e.KeyEnc)
		if err != nil {
			return nil, fmt.Errorf("解密易支付密钥失败: %w", err)
		}
		up.Key = string(data)
	}
	return up, nil
}

// ========== List ==========

func (h *UpstreamHandler) handleListUpstreams(c *gin.Context) {
	ups := h.pool.All()
	resp := make([]*UpstreamResponse, 0, len(ups))
	for _, up := range ups {
		resp = append(resp, upstreamToResponse(up))
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

// ========== Get ==========

func (h *UpstreamHandler) handleGetUpstream(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}
	up := h.pool.ByID(id)
	if up == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "上游不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": upstreamToResponse(up)})
}

// handleUpstreamSecret 返回上游完整密钥（明文，需登录）。
// 用途：管理后台「查看/复制完整密钥」与「对接指引」复制真实可用的密钥。
// 安全提示：仅登录用户可调用；前端展示时应二次确认并提示勿泄露。
func (h *UpstreamHandler) handleUpstreamSecret(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}
	up := h.pool.ByID(id)
	if up == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "上游不存在"})
		return
	}
	logger.Info("upstream", "上游密钥被查看（明文）", "id", id, "name", up.Name, "operator", c.GetString("username"))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":   up.ID,
		"name": up.Name,
		"pid":  up.Pid,
		"key":  up.Key,
	}})
}

// ========== Create ==========

type createUpstreamReq struct {
	Name          string `json:"name" binding:"required"`
	Pid           string `json:"pid" binding:"required"`
	Key           string `json:"key"`
	BaseURL       string `json:"base_url"`
	NotifyDomains string `json:"notify_domains"`
	Remark        string `json:"remark"`
}

func (h *UpstreamHandler) handleCreateUpstream(c *gin.Context) {
	var req createUpstreamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	if req.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "易支付密钥不能为空"})
		return
	}

	now := time.Now()
	up := &upstream.Upstream{
		Name:          req.Name,
		Pid:           req.Pid,
		Key:           req.Key,
		BaseURL:       req.BaseURL,
		NotifyDomains: req.NotifyDomains,
		Status:        upstream.StatusEnabled,
		Remark:        req.Remark,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	// 加密敏感字段
	enc, err := h.encryptUpstream(up)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 写入 upstreams.db（仅 Admin API 可访问）
	id, err := h.upstreamDB.CreateUpstream(c.Request.Context(), enc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建上游失败（pid 可能已存在）: " + err.Error()})
		return
	}
	up.ID = id

	// 推送到内存池（其他模块立即可用）
	h.pool.Add(up)

	// 审计日志
	_ = h.upstreamDB.InsertUpstreamLog(c.Request.Context(), &upstream.UpstreamLog{
		UpstreamID: id,
		Action:     "create",
		Detail:     fmt.Sprintf("创建上游: %s (pid=%s)", req.Name, req.Pid),
		Operator:   c.GetString("username"),
	})

	logger.Info("upstream", "上游创建成功", "id", id, "name", req.Name, "pid", req.Pid)
	c.JSON(http.StatusOK, gin.H{"data": upstreamToResponse(up)})
}

// ========== Update ==========

type updateUpstreamReq struct {
	Name          string `json:"name" binding:"required"`
	Pid           string `json:"pid" binding:"required"`
	Key           string `json:"key"`
	BaseURL       string `json:"base_url"`
	NotifyDomains string `json:"notify_domains"`
	Remark        string `json:"remark"`
}

func (h *UpstreamHandler) handleUpdateUpstream(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}

	var req updateUpstreamReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}

	// 保留原有状态与密钥（密钥字段前端不传（空）时保留原值，避免误清空）
	existing := h.pool.ByID(id)
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "上游不存在"})
		return
	}

	up := &upstream.Upstream{
		ID:            id,
		Name:          req.Name,
		Pid:           req.Pid,
		Key:           existing.Key,
		BaseURL:       req.BaseURL,
		NotifyDomains: req.NotifyDomains,
		Status:        existing.Status,
		Remark:        req.Remark,
		CreatedAt:     existing.CreatedAt,
		UpdatedAt:     time.Now(),
	}
	if req.Key != "" {
		up.Key = req.Key
	}

	// 加密敏感字段
	enc, err := h.encryptUpstream(up)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	enc.ID = id

	// 更新 upstreams.db
	if err := h.upstreamDB.UpdateUpstream(c.Request.Context(), enc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新上游失败（pid 可能已存在）: " + err.Error()})
		return
	}

	// 更新内存池
	h.pool.Update(up)

	// 审计日志
	_ = h.upstreamDB.InsertUpstreamLog(c.Request.Context(), &upstream.UpstreamLog{
		UpstreamID: id,
		Action:     "update",
		Detail:     fmt.Sprintf("更新上游: %s (pid=%s)", req.Name, req.Pid),
		Operator:   c.GetString("username"),
	})

	logger.Info("upstream", "上游更新成功", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": upstreamToResponse(up)})
}

// ========== Delete ==========

func (h *UpstreamHandler) handleDeleteUpstream(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}

	ctx := c.Request.Context()

	// 删除数据库
	if err := h.upstreamDB.DeleteUpstream(ctx, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除上游失败: " + err.Error()})
		return
	}

	// 移除内存池
	h.pool.Remove(id)

	// 审计日志
	_ = h.upstreamDB.InsertUpstreamLog(ctx, &upstream.UpstreamLog{
		UpstreamID: id,
		Action:     "delete",
		Detail:     fmt.Sprintf("删除上游 ID: %d", id),
		Operator:   c.GetString("username"),
	})

	logger.Info("upstream", "上游已删除", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// ========== Enable / Disable ==========

func (h *UpstreamHandler) handleEnableUpstream(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}
	ctx := c.Request.Context()

	if err := h.upstreamDB.SetUpstreamStatus(ctx, id, string(upstream.StatusEnabled)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.SetStatus(id, upstream.StatusEnabled)

	_ = h.upstreamDB.InsertUpstreamLog(ctx, &upstream.UpstreamLog{
		UpstreamID: id, Action: "enable", Detail: "手动启用", Operator: c.GetString("username"),
	})
	logger.Info("upstream", "上游已启用", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

func (h *UpstreamHandler) handleDisableUpstream(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}
	ctx := c.Request.Context()

	if err := h.upstreamDB.SetUpstreamStatus(ctx, id, string(upstream.StatusDisabled)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.pool.SetStatus(id, upstream.StatusDisabled)

	_ = h.upstreamDB.InsertUpstreamLog(ctx, &upstream.UpstreamLog{
		UpstreamID: id, Action: "disable", Detail: "手动禁用", Operator: c.GetString("username"),
	})
	logger.Info("upstream", "上游已禁用", "id", id)
	c.JSON(http.StatusOK, gin.H{"data": "ok"})
}

// ========== Upstream Logs ==========

func (h *UpstreamHandler) handleUpstreamLogs(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效上游ID"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "30"))
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	logs, err := h.upstreamDB.ListUpstreamLogs(c.Request.Context(), id, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": logs})
}
