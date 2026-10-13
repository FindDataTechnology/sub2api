// Package lanes 承载词元（ciyuan）自有三车道：rerank / websearch / audio。
//
// 谱系纪律（ADR-0024，台账 docs/sub2api-fork-patches §二）：车道代码全部新增
// 在本包，只增不改；上游唯一插入点 = routes/gateway.go apiKeyAuth 链上的一行
// 挂载（登记缝点「路由注册」）。车道不绑 chat 分组语义，故挂在组门
// （groupModelAllowlist / requireGroup*）之前，与 /v1/sub2api/billing 同位。
// 认证复用既有 apiKeyAuth 中间件：外部调用者拿词元 key 即用三车道。
package lanes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// notImplemented 统一 501 占位：过准入、有上游账号后停在无适配器位（3.x 适配器
// 接管此位：携带 account 调上游 + Meter.WriteUsage 计量）。错误体带 lane 标识，
// 与 3.4 的车道统一错误体同构。
func notImplemented(c *gin.Context, lane string) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": gin.H{
			"type":    "lane_not_implemented",
			"message": lane + " lane is not implemented yet (ciyuan lanes skeleton)",
			"lane":    lane,
		},
	})
}

// laneUpstreamUnavailable 池空（tasks 2.4）：车道暂无可用上游，503 与 501 占位
// 可区分——上游未配置/全部停用是一回事，适配器缺失是另一回事。
func laneUpstreamUnavailable(c *gin.Context, lane string) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"type":    "lane_upstream_unavailable",
			"message": lane + " lane has no available upstream account (configure one in admin accounts)",
			"lane":    lane,
		},
	})
}

// RegisterGatewayLanes 把三车道路由挂进已带 apiKeyAuth 的 /v1 组。
// group = routes/gateway.go 的 gateway 组（apiKeyAuth 已挂、组门之前）。
// app 可为 nil（未装配网关服务时退化为纯 501 骨架）；装配后服务链=
// 鉴权（组中间件）→ 计费准入 → 上游池取号 → 适配器位（3.x 接管）。
func RegisterGatewayLanes(group *gin.RouterGroup, app *Lanes) {
	serve := func(lane string) gin.HandlerFunc {
		return func(c *gin.Context) { serveLane(c, lane, app) }
	}
	group.POST("/rerank", serve(LaneRerank))
	group.POST("/search", serve(LaneWebsearch))
	group.POST("/audio/transcriptions", serve(LaneAudio))
	group.POST("/audio/speech", serve(LaneAudio))
}

func serveLane(c *gin.Context, lane string, app *Lanes) {
	if app == nil {
		notImplemented(c, lane)
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{
				"type":    "unauthorized",
				"message": "missing api key",
				"lane":    lane,
			},
		})
		return
	}
	if app.Meter != nil && app.Meter.billing != nil {
		if err := app.Meter.CheckEligibility(c, apiKey); err != nil {
			laneBillingError(c, lane, err)
			return
		}
	}
	if app.Pool != nil {
		account, err := app.Pool.Pick(c.Request.Context(), LanePlatform(lane))
		if err != nil {
			laneUpstreamUnavailable(c, lane)
			return
		}
		_ = account // 3.x 适配器在此接管：携带 account 调上游 + WriteUsage 计量
	}
	notImplemented(c, lane)
}
