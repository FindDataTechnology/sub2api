// Package lanes 承载词元（ciyuan）自有三车道：rerank / websearch / audio。
//
// 谱系纪律（ADR-0024，台账 docs/sub2api-fork-patches §二）：车道代码全部新增
// 在本包，只增不改；上游唯一插入点 = routes/gateway.go apiKeyAuth 链上的一行
// 挂载（登记缝点「路由注册」）。车道不绑 chat 分组语义，故挂在组门
// （groupModelAllowlist / requireGroup*）之前，与 /v1/sub2api/billing 同位。
// 认证复用既有 apiKeyAuth 中间件：外部调用者拿词元 key 即用三车道。
package lanes

import (
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 车道错误面统一构造函数见 errors.go（tasks 3.4）：notImplemented /
// laneUpstreamUnavailable / laneBadRequest / laneUpstreamError /
// laneUpstreamHTTPError / laneUpstreamMisconfigured 三车道共用同一形状与判据。

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
	// 能力快照（ciyuan-capability-endpoint）：只读、不计费。命名空间化以避开上游
	// 未来新增的 /v1/* 端点，与 /v1/sub2api/billing 同构。
	group.GET("/ciyuan/capabilities", func(c *gin.Context) { ServeCapabilities(c, app) })
}

func serveLane(c *gin.Context, lane string, app *Lanes) {
	if app == nil {
		notImplemented(c, lane)
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		// 正常路径由 apiKeyAuth 中间件先拦（401）；此分支仅在中间件未装配
		// （如测试裸挂载）时出现，保持同一错误形状。
		laneUnauthorized(c, lane)
		return
	}
	if app.Meter != nil && app.Meter.billing != nil {
		if err := app.Meter.CheckEligibility(c, apiKey); err != nil {
			laneBillingError(c, lane, err)
			return
		}
	}
	if app.Pool != nil {
		// 跨协议车道（audio）按平台集合归并取候选；其余车道等价于单平台。
		platforms := LanePlatforms(lane)
		var accounts []*service.Account
		var err error
		if len(platforms) > 1 {
			accounts, err = app.Pool.CandidatesForPlatforms(c.Request.Context(), platforms)
		} else {
			accounts, err = app.Pool.Candidates(c.Request.Context(), LanePlatform(lane))
		}
		if err != nil || len(accounts) == 0 {
			laneUpstreamUnavailable(c, lane)
			return
		}
		if adapter := app.Adapter(lane); adapter != nil {
			adapter.Serve(c, apiKey, accounts, app.Pool, app.Meter)
			return
		}
	}
	notImplemented(c, lane)
}
