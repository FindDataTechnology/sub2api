//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// 词元 rerank 车道计费（fork 缝点，tasks 3.1）：镜像 alpha_search 计费测试形状。

func TestCalculateRerankCostNilAndOverride(t *testing.T) {
	t.Parallel()
	s := &BillingService{}

	// nil=未配置：按 0 计费（区别于 web search 的默认 0.01；开放前须配置）。
	cost := s.CalculateRerankCost(1, nil, 1.0)
	require.Zero(t, cost.TotalCost)
	require.Zero(t, cost.ActualCost)
	require.Equal(t, string(BillingModePerRequest), cost.BillingMode)

	// 分组配置价 × 倍率
	cost = s.CalculateRerankCost(1, float64Ptr(0.002), 2.5)
	require.InDelta(t, 0.002, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.005, cost.ActualCost, 1e-12)
	require.Equal(t, string(BillingModePerRequest), cost.BillingMode)

	// 0 = 显式免费
	cost = s.CalculateRerankCost(2, float64Ptr(0), 3.0)
	require.Zero(t, cost.TotalCost)
	require.Zero(t, cost.ActualCost)

	// 负数倍率按 0 处理
	cost = s.CalculateRerankCost(1, float64Ptr(0.002), -1)
	require.InDelta(t, 0.002, cost.TotalCost, 1e-12)
	require.Zero(t, cost.ActualCost)

	// 次数 <= 0 不产生费用
	cost = s.CalculateRerankCost(0, float64Ptr(0.002), 1.0)
	require.Zero(t, cost.TotalCost)
	require.Empty(t, cost.BillingMode)
}

func TestCalculateOpenAIRecordUsageCostRerankPerRequest(t *testing.T) {
	t.Parallel()
	svc := &OpenAIGatewayService{billingService: &BillingService{}}
	groupID := int64(11)

	// 未配置价：0 计费（不报错、不落 pricing 告警路径）；按次倍率=第 4 倍率参数 2.0。
	apiKey := &APIKey{ID: 1, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI}}
	result := &OpenAIForwardResult{Model: "BAAI/bge-reranker-v2-m3", UpstreamModel: "BAAI/bge-reranker-v2-m3", RerankRequests: 1}
	cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), result, apiKey, []string{"BAAI/bge-reranker-v2-m3"}, 3.0, 1.0, 1.0, 2.0, UsageTokens{}, "", boolPtr(false), false, time.Time{})
	require.NoError(t, err)
	require.Equal(t, string(BillingModePerRequest), cost.BillingMode)
	require.Zero(t, cost.TotalCost)
	require.Zero(t, cost.ActualCost)

	// 分组配置价 0.002 × 基础倍率 1.5
	apiKey.Group.RerankPricePerRequest = float64Ptr(0.002)
	cost, err = svc.calculateOpenAIRecordUsageCost(context.Background(), result, apiKey, []string{"BAAI/bge-reranker-v2-m3"}, 1.0, 1.0, 1.0, 1.5, UsageTokens{}, "", boolPtr(false), false, time.Time{})
	require.NoError(t, err)
	require.InDelta(t, 0.002, cost.TotalCost, 1e-12)
	require.InDelta(t, 0.003, cost.ActualCost, 1e-12)

	// RerankRequests = 0 时不得走按请求分支（回落 token 路径：无定价数据报错）。
	result.RerankRequests = 0
	_, err = svc.calculateOpenAIRecordUsageCost(context.Background(), result, apiKey, []string{"BAAI/bge-reranker-v2-m3"}, 1.0, 1.0, 1.0, 1.0, UsageTokens{InputTokens: 10}, "", boolPtr(false), false, time.Time{})
	require.Error(t, err)
}

func TestAPIKeyService_SnapshotRoundTrip_PreservesRerankPricePerRequest(t *testing.T) {
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})
	groupID := int64(9)
	apiKey := &APIKey{
		ID:      1,
		UserID:  2,
		GroupID: &groupID,
		Key:     "k-rerank",
		Status:  StatusActive,
		User:    &User{ID: 2, Status: StatusActive, Role: RoleUser},
		Group: &Group{
			ID:                    groupID,
			Name:                  "openai",
			Platform:              PlatformOpenAI,
			Status:                StatusActive,
			SubscriptionType:      SubscriptionTypeStandard,
			RateMultiplier:        1,
			RerankPricePerRequest: float64Ptr(0.002),
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	roundTrip := svc.snapshotToAPIKey(apiKey.Key, snapshot)

	require.NotNil(t, roundTrip)
	require.NotNil(t, roundTrip.Group)
	require.NotNil(t, roundTrip.Group.RerankPricePerRequest)
	require.InDelta(t, 0.002, *roundTrip.Group.RerankPricePerRequest, 1e-12)
}
