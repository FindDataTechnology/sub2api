package handler

import (
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// ListMyMirrors 列出**当前登录用户自己**的套餐/外部结算镜像行。
// 用它把「套餐消费」放进调用者自己的面板（add-serving-plans D6 的可见面）。
//
// GET /api/v1/usage/mirrors
//
// 与 admin 侧 GET /api/v1/admin/usage/mirrors 只差一件事，但那是安全边界：
// user_id **只**取登录主体，query 里的 user_id 一律不读、也不校验
// —— 去校验它反而会让人以为它有用。admin 侧按 user_id 查是运营视角；这里是本人视角，
// 同 /api/v1/usage 其它读面（parseUserUsageFilters 也是从 subject 取 UserID）。
func (h *UsageHandler) ListMyMirrors(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
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

	mirrors, err := h.usageService.ListMirrors(c.Request.Context(), subject.UserID, limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": mirrors, "total": len(mirrors)})
}
