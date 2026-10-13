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

// notImplemented 统一 501 占位：无适配器时车道只到达路由+鉴权+计费准入层，
// 业务实现由 3.x 适配器接管（本占位保留为无适配器形状）。错误体带 lane 标识，
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

// RegisterGatewayLanes 把三车道路由挂进已带 apiKeyAuth 的 /v1 组。
// group = routes/gateway.go 的 gateway 组（apiKeyAuth 已挂、组门之前）。
// meter 可为 nil（未装配计费服务时退化为纯 501 骨架；生产装配后先过计费
// 准入——窗口耗尽/余额不足在此拒绝，不触上游（tasks 2.3））。
func RegisterGatewayLanes(group *gin.RouterGroup, meter *Meter) {
	serve := func(lane string) gin.HandlerFunc {
		return func(c *gin.Context) { serveLane(c, lane, meter) }
	}
	group.POST("/rerank", serve("rerank"))
	group.POST("/search", serve("websearch"))
	group.POST("/audio/transcriptions", serve("audio"))
	group.POST("/audio/speech", serve("audio"))
}

func serveLane(c *gin.Context, lane string, meter *Meter) {
	if meter != nil && meter.billing != nil {
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
		if err := meter.CheckEligibility(c, apiKey); err != nil {
			laneBillingError(c, lane, err)
			return
		}
	}
	notImplemented(c, lane)
}
