package lanes

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Lanes 车道应用装配（Meter 计量 + Pool 上游池）：由路由注册层从 OpenAI 网关
// handler 只读访问器装配（缝点台账§二「handler 服务访问器」「service 账号仓储
// 访问器」）。任一服务缺失（测试骨架）时对应能力退化；网关服务整体缺失时
// 返回 nil——RegisterGatewayLanes 对 nil 走纯 501 骨架。
type Lanes struct {
	Meter *Meter
	Pool  *Pool
}

// NewFromOpenAIHandler 从 OpenAI 网关 handler 装配车道应用。
func NewFromOpenAIHandler(openAI *handler.OpenAIGatewayHandler, apiKeyService *service.APIKeyService, cfg *config.Config) *Lanes {
	if openAI == nil {
		return nil
	}
	gatewayService := openAI.LanesGatewayService()
	if gatewayService == nil {
		return nil
	}
	return &Lanes{
		Meter: NewMeter(openAI.LanesBillingCacheService(), apiKeyService, gatewayService.RecordUsage, cfg),
		Pool:  NewPool(gatewayService.LanesAccountRepo()),
	}
}
