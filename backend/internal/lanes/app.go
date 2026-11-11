package lanes

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// Lanes 车道应用装配（Meter 计量 + Pool 上游池 + 车道适配器）：由路由注册层
// 从 OpenAI 网关 handler 只读访问器装配（缝点台账§二「handler 服务访问器」
// 「service 账号仓储访问器」）。任一服务缺失（测试骨架）时对应能力退化；网关
// 服务整体缺失时返回 nil——RegisterGatewayLanes 对 nil 走纯 501 骨架。
type Lanes struct {
	Meter *Meter
	Pool  *Pool
	// Tasks 是任务车道（add-ciyuan-task-lane）。engine 在路由装配时补齐
	// （SetEngine），未补齐时任务端点返回 503 而不是假装执行。
	Tasks    *TaskLane
	adapters map[string]LaneAdapter
	// models = 能力快照的模型推导入口（handler 只读 getter LanesCapabilityModels）：
	// 返回 (可调度账号可用模型, 与 /v1/models 同源的可见模型源)。nil 时能力端点
	// 返回 503，不影响三车道。
	models func(ctx context.Context, apiKey *service.APIKey) (available, source []string)
}

// NewFromOpenAIHandler 从 OpenAI 网关 handler 装配车道应用。
// 适配器注册表=车道 v1 落地进度：3.1 rerank、3.2 audio、3.3 websearch 全接。
// capabilityModels = 能力快照的模型源入口（handler 侧只读 getter，nil 时能力端点
// 退化为 503，三车道不受影响）。
func NewFromOpenAIHandler(openAI *handler.OpenAIGatewayHandler, capabilityModels func(ctx context.Context, apiKey *service.APIKey) (available, source []string), apiKeyService *service.APIKeyService, cfg *config.Config) *Lanes {
	if openAI == nil {
		return nil
	}
	gatewayService := openAI.LanesGatewayService()
	if gatewayService == nil {
		return nil
	}
	// 池=同一 accountRepo 双面（取数+冷却写，ciyuan-lane-pool-failover D1：
	// 冷却复用 SetTempUnschedulable，零新增缝点）。
	repo := gatewayService.LanesAccountRepo()
	return &Lanes{
		Meter: NewMeter(openAI.LanesBillingCacheService(), apiKeyService, gatewayService.RecordUsage, cfg),
		Pool:  NewPool(repo, repo),
		Tasks: newTaskLane(&taskExecutor{apiKeys: apiKeyService}, apiKeyService),
		adapters: map[string]LaneAdapter{
			LaneRerank:    NewRerankAdapter(),
			LaneWebsearch: NewWebsearchAdapter(),
			LaneAudio:     NewAudioAdapter(),
		},
		models: capabilityModels,
	}
}

// SetEngine 注入 gin 引擎：任务车道靠「把请求重放回同一个引擎」来复用
// 鉴权/分组门/账号调度/计费，不在车道里复制任何一条准入规则。
// 由 routes/gateway.go 在装配时调用一次（缝点：路由注册）。
func (l *Lanes) SetEngine(r *gin.Engine) {
	if l == nil || l.Tasks == nil {
		return
	}
	l.Tasks.exec.engine = r
	// 调度器在装配完成时就启动，而不是等第一个 defer 提交才启动：
	// 否则**进程重启前入队的任务永远不会被领取**（库里躺着 pending，没有任何人在看）。
	// startOnce 保证只启动一次。
	l.Tasks.startScheduler()
}

// Adapter 返回车道适配器；未落地车道返回 nil（serveLane 保持 501 占位）。
func (l *Lanes) Adapter(lane string) LaneAdapter {
	if l == nil {
		return nil
	}
	return l.adapters[lane]
}
