package admin

import (
	"net/http"
	"strconv"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 套餐/外部结算的虚拟用量镜像端点（add-serving-plans D6）。
// 契约正本：fd-wanxing/docs/sub2api-usage-mirror.md（递交上游 Wei-Shaw/sub2api 的稿）。

// RecordUsageMirrorRequest 是镜像写入的入参。
// recorded_at 是 epoch 毫秒（与契约示例一致）；缺省用服务端收到时刻。
type RecordUsageMirrorRequest struct {
	ExternalID string `json:"external_id"`
	UserID     int64  `json:"user_id"`
	Minutes    int    `json:"minutes"`
	Note       string `json:"note"`
	RecordedAt int64  `json:"recorded_at"`
}

// usageMirrorResponse 刻意**不使用** {code,message,data} 信封。
// 本端点的契约就是 {ok, usage_id, duplicate} 三个顶层键（萬星 facade 与上游递交稿
// 都按这三个键读），改信封会把「契约」变成「实现细节」。仓内其余 admin 面仍走信封。
type usageMirrorResponse struct {
	OK        bool  `json:"ok"`
	UsageID   int64 `json:"usage_id"`
	Duplicate bool  `json:"duplicate"`
}

// RecordMirror 写入一条镜像行：只增展示、不动余额；external_id 重复时返回首次结果。
// POST /api/v1/admin/usage
func (h *UsageHandler) RecordMirror(c *gin.Context) {
	var req RecordUsageMirrorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// 请求体都解不开时，最有用的判词是「external_id 必填」——它也正是契约的第一条。
		response.ErrorFrom(c, service.ErrUsageMirrorExternalIDRequired)
		return
	}

	recordedAt := time.Time{}
	if req.RecordedAt > 0 {
		recordedAt = time.UnixMilli(req.RecordedAt)
	}

	mirror, duplicate, err := h.usageService.RecordMirror(c.Request.Context(), service.UsageMirrorCreate{
		ExternalID: req.ExternalID,
		UserID:     req.UserID,
		Minutes:    req.Minutes,
		Note:       req.Note,
		RecordedAt: recordedAt,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	c.JSON(http.StatusOK, usageMirrorResponse{OK: true, UsageID: mirror.ID, Duplicate: duplicate})
}

// ListMirrors 列出某个用户最近的镜像行（运营/排障读面，走标准信封）。
// GET /api/v1/admin/usage/mirrors
func (h *UsageHandler) ListMirrors(c *gin.Context) {
	var userID int64
	if v := c.Query("user_id"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			response.ErrorFrom(c, infraerrors.BadRequest("USAGE_MIRROR_USER_ID_INVALID", "user_id must be an integer"))
			return
		}
		userID = parsed
	}
	if userID <= 0 {
		response.ErrorFrom(c, service.ErrUsageMirrorUserRequired)
		return
	}

	limit := 0
	if v := c.Query("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			response.ErrorFrom(c, infraerrors.BadRequest("USAGE_MIRROR_LIMIT_INVALID", "limit must be an integer"))
			return
		}
		limit = parsed
	}

	mirrors, err := h.usageService.ListMirrors(c.Request.Context(), userID, limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": mirrors, "total": len(mirrors)})
}
