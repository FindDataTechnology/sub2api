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

	"github.com/gin-gonic/gin"
)

// notImplemented 统一 501 占位：骨架期（tasks 2.2）只定路由形状与鉴权链，
// 业务实现按切片填入（2.3 计量通道 / 3.x 三车道 v1）。错误体带 lane 标识，
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
func RegisterGatewayLanes(group *gin.RouterGroup) {
	group.POST("/rerank", func(c *gin.Context) { notImplemented(c, "rerank") })
	group.POST("/search", func(c *gin.Context) { notImplemented(c, "websearch") })
	group.POST("/audio/transcriptions", func(c *gin.Context) { notImplemented(c, "audio") })
	group.POST("/audio/speech", func(c *gin.Context) { notImplemented(c, "audio") })
}
