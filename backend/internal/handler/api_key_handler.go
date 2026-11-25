// Package handler provides HTTP request handlers for the application.
package handler

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// APIKeyHandler handles API key-related requests
type APIKeyHandler struct {
	apiKeyService *service.APIKeyService
}

// NewAPIKeyHandler creates a new APIKeyHandler
func NewAPIKeyHandler(apiKeyService *service.APIKeyService) *APIKeyHandler {
	return &APIKeyHandler{
		apiKeyService: apiKeyService,
	}
}

// CreateAPIKeyRequest represents the create API key request payload
type CreateAPIKeyRequest struct {
	Name          string   `json:"name" binding:"required"`
	GroupID       *int64   `json:"group_id"`        // nullable
	CustomKey     *string  `json:"custom_key"`      // 可选的自定义key
	IPWhitelist   []string `json:"ip_whitelist"`    // IP 白名单
	IPBlacklist   []string `json:"ip_blacklist"`    // IP 黑名单
	Quota         *float64 `json:"quota"`           // 配额限制 (USD)
	ExpiresInDays *int     `json:"expires_in_days"` // 过期天数

	// Rate limit fields (0 = unlimited)
	RateLimit5h *float64 `json:"rate_limit_5h"`
	RateLimit1d *float64 `json:"rate_limit_1d"`
	RateLimit7d *float64 `json:"rate_limit_7d"`

	// 容量契约（add-key-capacity-contract）：0 = 不限/不占席位
	ConcurrencyRequest *int   `json:"concurrency_request"`
	ConcurrencyLimit   *int   `json:"concurrency_limit"`
	RPMLimit           *int   `json:"rpm_limit"`
	EffectiveAt        string `json:"effective_at"` // RFC3339；空 = 立即生效
}

// UpdateAPIKeyRequest represents the update API key request payload
type UpdateAPIKeyRequest struct {
	Name        string    `json:"name"`
	GroupID     *int64    `json:"group_id"`
	Status      string    `json:"status" binding:"omitempty,oneof=active inactive"`
	IPWhitelist *[]string `json:"ip_whitelist"` // IP 白名单（nil 不修改，空数组清空）
	IPBlacklist *[]string `json:"ip_blacklist"` // IP 黑名单（nil 不修改，空数组清空）
	Quota       *float64  `json:"quota"`        // 配额限制 (USD), 0=无限制
	ExpiresAt   *string   `json:"expires_at"`   // 过期时间 (ISO 8601)
	ResetQuota  *bool     `json:"reset_quota"`  // 重置已用配额

	// Rate limit fields (nil = no change, 0 = unlimited)
	RateLimit5h         *float64 `json:"rate_limit_5h"`
	RateLimit1d         *float64 `json:"rate_limit_1d"`
	RateLimit7d         *float64 `json:"rate_limit_7d"`
	ResetRateLimitUsage *bool    `json:"reset_rate_limit_usage"` // 重置限速用量

	// 容量契约（nil = 不修改；0 = 不限/不占席位）。effective_at 传空串表示清空。
	ConcurrencyRequest *int    `json:"concurrency_request"`
	ConcurrencyLimit   *int    `json:"concurrency_limit"`
	RPMLimit           *int    `json:"rpm_limit"`
	EffectiveAt        *string `json:"effective_at"`
}

func validAPIKeyLimit(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func validateAPIKeyCreateRequest(req CreateAPIKeyRequest) error {
	if req.Quota != nil && !validAPIKeyLimit(*req.Quota) {
		return errors.New("invalid quota")
	}
	if req.RateLimit5h != nil && !validAPIKeyLimit(*req.RateLimit5h) {
		return errors.New("invalid rate_limit_5h")
	}
	if req.RateLimit1d != nil && !validAPIKeyLimit(*req.RateLimit1d) {
		return errors.New("invalid rate_limit_1d")
	}
	if req.RateLimit7d != nil && !validAPIKeyLimit(*req.RateLimit7d) {
		return errors.New("invalid rate_limit_7d")
	}
	if req.ExpiresInDays != nil && *req.ExpiresInDays <= 0 {
		return errors.New("invalid expires_in_days")
	}
	if err := validateCapacityRequest(req.ConcurrencyRequest, req.ConcurrencyLimit, req.RPMLimit); err != nil {
		return err
	}
	return nil
}

// validateCapacityRequest 只查形状（非负）。聚合不变量（Σrequest ≤ 池）在 service 层，
// 那里才有用户池容量。
func validateCapacityRequest(request, limit, rpm *int) error {
	for _, f := range []struct {
		name string
		v    *int
	}{{"concurrency_request", request}, {"concurrency_limit", limit}, {"rpm_limit", rpm}} {
		if f.v != nil && *f.v < 0 {
			return errors.New("invalid " + f.name)
		}
	}
	return nil
}

// parseCapacityEffectiveAt 解析 effective_at：空串 = 明确清空（clear=true）。
func parseCapacityEffectiveAt(s string) (t *time.Time, clear bool, err error) {
	if s == "" {
		return nil, true, nil
	}
	parsed, perr := time.Parse(time.RFC3339, s)
	if perr != nil {
		return nil, false, perr
	}
	return &parsed, false, nil
}

func validateAPIKeyUpdateRequest(req UpdateAPIKeyRequest) error {
	if req.Quota != nil && !validAPIKeyLimit(*req.Quota) {
		return errors.New("invalid quota")
	}
	if req.RateLimit5h != nil && !validAPIKeyLimit(*req.RateLimit5h) {
		return errors.New("invalid rate_limit_5h")
	}
	if req.RateLimit1d != nil && !validAPIKeyLimit(*req.RateLimit1d) {
		return errors.New("invalid rate_limit_1d")
	}
	if req.RateLimit7d != nil && !validAPIKeyLimit(*req.RateLimit7d) {
		return errors.New("invalid rate_limit_7d")
	}
	return validateCapacityRequest(req.ConcurrencyRequest, req.ConcurrencyLimit, req.RPMLimit)
}

// List handles listing user's API keys with pagination
// GET /api/v1/api-keys
func (h *APIKeyHandler) List(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	page, pageSize := response.ParsePagination(c)
	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    c.DefaultQuery("sort_by", "created_at"),
		SortOrder: c.DefaultQuery("sort_order", "desc"),
	}

	// Parse filter parameters
	var filters service.APIKeyListFilters
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		if len(search) > 100 {
			search = search[:100]
		}
		filters.Search = search
	}
	filters.Status = c.Query("status")
	if groupIDStr := c.Query("group_id"); groupIDStr != "" {
		gid, err := strconv.ParseInt(groupIDStr, 10, 64)
		if err == nil {
			filters.GroupID = &gid
		}
	}

	keys, result, err := h.apiKeyService.List(c.Request.Context(), subject.UserID, params, filters)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.APIKey, 0, len(keys))
	for i := range keys {
		out = append(out, *dto.APIKeyFromService(&keys[i]).Localized(c.GetHeader("Accept-Language")))
	}
	response.Paginated(c, out, result.Total, page, pageSize)
}

// GetByID handles getting a single API key
// GET /api/v1/api-keys/:id
func (h *APIKeyHandler) GetByID(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	keyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid key ID")
		return
	}

	key, err := h.apiKeyService.GetByID(c.Request.Context(), keyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 验证所有权
	if key.UserID != subject.UserID {
		response.NotFound(c, "API key not found")
		return
	}

	response.Success(c, dto.APIKeyFromService(key).Localized(c.GetHeader("Accept-Language")))
}

// State 回报该 key 的容量状态：库里的配置、鉴权链实际看到的值、运行时在飞数、
// 以及"配置是否已生效"。
//
// 存在理由：认证快照有 L1(15s)/L2(300s) 缓存，改完 concurrency 后运维只能等 15 秒猜。
// 这个端点把"猜"变成"看"。
// GET /api/v1/keys/:id/state
func (h *APIKeyHandler) State(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	keyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid key ID")
		return
	}

	state, err := h.apiKeyService.GetKeyCapacityState(c.Request.Context(), keyID, middleware2.CapacityGate())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 越权返回 404（不泄露存在性），与 GetByID 一致
	if state == nil {
		response.NotFound(c, "API key not found")
		return
	}
	owner, gerr := h.apiKeyService.GetByID(c.Request.Context(), keyID)
	if gerr != nil || owner == nil || owner.UserID != subject.UserID {
		response.NotFound(c, "API key not found")
		return
	}

	response.Success(c, state)
}

// Stability 回报该 key 的**基线**（新 key 从同类群体继承，带置信度）与**稳定性**
// （成功率/p95/限流占比/活跃时段）。
//
// 存在理由：用户问「平台侧怎么快速统计出某个新 key 的基础情况」和「key 的稳定性」。
// 口径纪律：成功率分母含失败（否则恒 100%）；无数据给 null 不给 0/1。
// GET /api/v1/keys/:id/stability?window=1h|24h|7d
func (h *APIKeyHandler) Stability(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	keyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid key ID")
		return
	}

	window, lookback, okWindow := service.ParseKeyUsageWindow(c.Query("window"))
	if !okWindow {
		response.BadRequest(c, "Invalid window, use 1h|24h|7d")
		return
	}

	key, err := h.apiKeyService.GetByID(c.Request.Context(), keyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 越权返回 404（不泄露存在性），与 GetByID / State 一致
	if key == nil || key.UserID != subject.UserID {
		response.NotFound(c, "API key not found")
		return
	}

	view, err := h.apiKeyService.GetKeyStability(c.Request.Context(), key, window, lookback)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}

// Create handles creating a new API key
// POST /api/v1/api-keys
func (h *APIKeyHandler) Create(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	var req CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := validateAPIKeyCreateRequest(req); err != nil {
		response.BadRequest(c, "Invalid request: numeric limits must be finite and non-negative, and expires_in_days must be greater than zero")
		return
	}

	svcReq := service.CreateAPIKeyRequest{
		Name:          req.Name,
		GroupID:       req.GroupID,
		CustomKey:     req.CustomKey,
		IPWhitelist:   req.IPWhitelist,
		IPBlacklist:   req.IPBlacklist,
		ExpiresInDays: req.ExpiresInDays,
	}
	if req.ConcurrencyRequest != nil {
		svcReq.ConcurrencyRequest = *req.ConcurrencyRequest
	}
	if req.ConcurrencyLimit != nil {
		svcReq.ConcurrencyLimit = *req.ConcurrencyLimit
	}
	if req.RPMLimit != nil {
		svcReq.RPMLimit = *req.RPMLimit
	}
	if req.EffectiveAt != "" {
		t, _, err := parseCapacityEffectiveAt(req.EffectiveAt)
		if err != nil {
			response.BadRequest(c, "Invalid effective_at format: "+err.Error())
			return
		}
		svcReq.EffectiveAt = t
	}
	if req.Quota != nil {
		svcReq.Quota = *req.Quota
	}
	if req.RateLimit5h != nil {
		svcReq.RateLimit5h = *req.RateLimit5h
	}
	if req.RateLimit1d != nil {
		svcReq.RateLimit1d = *req.RateLimit1d
	}
	if req.RateLimit7d != nil {
		svcReq.RateLimit7d = *req.RateLimit7d
	}

	executeUserIdempotentJSON(c, "user.api_keys.create", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		key, err := h.apiKeyService.Create(ctx, subject.UserID, svcReq)
		if err != nil {
			return nil, err
		}
		return dto.APIKeyFromService(key).Localized(c.GetHeader("Accept-Language")), nil
	})
}

// Update handles updating an API key
// PUT /api/v1/api-keys/:id
func (h *APIKeyHandler) Update(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	keyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid key ID")
		return
	}

	var req UpdateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := validateAPIKeyUpdateRequest(req); err != nil {
		response.BadRequest(c, "Invalid request: numeric limits must be finite and non-negative")
		return
	}

	svcReq := service.UpdateAPIKeyRequest{
		IPWhitelist:         req.IPWhitelist,
		IPBlacklist:         req.IPBlacklist,
		Quota:               req.Quota,
		ResetQuota:          req.ResetQuota,
		RateLimit5h:         req.RateLimit5h,
		RateLimit1d:         req.RateLimit1d,
		RateLimit7d:         req.RateLimit7d,
		ResetRateLimitUsage: req.ResetRateLimitUsage,
		ConcurrencyRequest:  req.ConcurrencyRequest,
		ConcurrencyLimit:    req.ConcurrencyLimit,
		RPMLimit:            req.RPMLimit,
	}
	if req.Name != "" {
		svcReq.Name = &req.Name
	}
	svcReq.GroupID = req.GroupID
	if req.Status != "" {
		svcReq.Status = &req.Status
	}
	// Parse expires_at if provided
	if req.ExpiresAt != nil {
		if *req.ExpiresAt == "" {
			// Empty string means clear expiration
			svcReq.ExpiresAt = nil
			svcReq.ClearExpiration = true
		} else {
			t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
			if err != nil {
				response.BadRequest(c, "Invalid expires_at format: "+err.Error())
				return
			}
			svcReq.ExpiresAt = &t
		}
	}
	if req.EffectiveAt != nil {
		t, clear, err := parseCapacityEffectiveAt(*req.EffectiveAt)
		if err != nil {
			response.BadRequest(c, "Invalid effective_at format: "+err.Error())
			return
		}
		svcReq.EffectiveAt = t
		svcReq.ClearEffectiveAt = clear
	}

	key, err := h.apiKeyService.Update(c.Request.Context(), keyID, subject.UserID, svcReq)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.APIKeyFromService(key).Localized(c.GetHeader("Accept-Language")))
}

// Delete handles deleting an API key
// DELETE /api/v1/api-keys/:id
func (h *APIKeyHandler) Delete(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	keyID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid key ID")
		return
	}

	err = h.apiKeyService.Delete(c.Request.Context(), keyID, subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, gin.H{"message": "API key deleted successfully"})
}

// GetAvailableGroups 获取用户可以绑定的分组列表
// GET /api/v1/groups/available
func (h *APIKeyHandler) GetAvailableGroups(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	groups, err := h.apiKeyService.GetAvailableGroups(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.Group, 0, len(groups))
	for i := range groups {
		out = append(out, *dto.GroupFromService(&groups[i]).Localized(c.GetHeader("Accept-Language")))
	}
	response.Success(c, out)
}

// GetUserGroupRates 获取当前用户的专属分组倍率配置
// GET /api/v1/groups/rates
func (h *APIKeyHandler) GetUserGroupRates(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	rates, err := h.apiKeyService.GetUserGroupRates(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, rates)
}
