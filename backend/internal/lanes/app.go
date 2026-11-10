package lanes

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Lanes 车道应用装配（Meter 计量 + Pool 上游池 + 车道适配器）：由路由注册层
// 从 OpenAI 网关 handler 只读访问器装配（缝点台账§二「handler 服务访问器」
// 「service 账号仓储访问器」）。任一服务缺失（测试骨架）时对应能力退化；网关
// 服务整体缺失时返回 nil——RegisterGatewayLanes 对 nil 走纯 501 骨架。
type Lanes struct {
	Meter    *Meter
	Pool     *Pool
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
		adapters: map[string]LaneAdapter{
			LaneRerank:    NewRerankAdapter(),
			LaneWebsearch: NewWebsearchAdapter(),
			LaneAudio:     NewAudioAdapter(),
		},
		models: capabilityModels,
	}
}

// Adapter 返回车道适配器；未落地车道返回 nil（serveLane 保持 501 占位）。
func (l *Lanes) Adapter(lane string) LaneAdapter {
	if l == nil {
		return nil
	}
	return l.adapters[lane]
}
