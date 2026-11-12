package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// LanesCapabilityModels 的白盒测试：验证能力快照的模型源与 /v1/models 列表接口同源，
// 且 key 未绑分组时退化为全量可调度账号口径（消费侧接入前的关键分支）。

// ListSchedulable 补齐嵌入的 AccountRepository（nil 接口会 panic）：
// 未绑分组的 key 走这条路，按 byGroup[0] 取全量口径。
func (s *gatewayModelsAccountRepoStub) ListSchedulable(ctx context.Context) ([]service.Account, error) {
	return s.ListSchedulableByGroupID(ctx, 0)
}

func TestLanesCapabilityModels_UsesGroupAccountMappings(t *testing.T) {
	const groupID int64 = 8801
	repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		groupID: {
			{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"deepseek-v4.1-flash": "upstream-ds"},
			}},
			{ID: 2, Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"glm-5.3-flash": "upstream-glm"},
			}},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)
	apiKey := &service.APIKey{Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI}}

	available, source := h.LanesCapabilityModels(context.Background(), apiKey)

	require.ElementsMatch(t, []string{"deepseek-v4.1-flash", "glm-5.3-flash"}, available,
		"serving 应为可调度账号映射并集")
	require.ElementsMatch(t, available, source,
		"无白名单时可见源 == 可服务集合（两谓词不分裂）")
}

func TestLanesCapabilityModels_UnboundKeyFallsBackToAllSchedulable(t *testing.T) {
	// key 未绑分组：groupID=nil → 走 ListSchedulable 全量口径。
	repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		0: {
			{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{
				"model_mapping": map[string]any{"global-model": "upstream"},
			}},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)

	available, _ := h.LanesCapabilityModels(context.Background(), &service.APIKey{})

	require.Equal(t, []string{"global-model"}, available,
		"未绑分组的 key 应看到全量可调度账号口径，而不是空集")
}

func TestLanesCapabilityModels_NilServiceDegrades(t *testing.T) {
	h := &GatewayHandler{}
	available, source := h.LanesCapabilityModels(context.Background(), &service.APIKey{})
	require.Nil(t, available)
	require.Nil(t, source)
}
