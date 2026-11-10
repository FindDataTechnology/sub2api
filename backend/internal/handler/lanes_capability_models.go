package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// LanesCapabilityModels 是词元能力快照（/v1/ciyuan/capabilities）的只读模型源入口。
//
// 返回两个集合：
//   - available：分组内**可调度账号**的模型映射并集（composite 分组走 composite 口径）
//   - source：与 /v1/models 列表接口同源的可见模型集合（未应用分组白名单）
//
// 两者都复用既有推导函数（GetAvailableModels / compositeAvailableModels /
// modelListingSource / defaultModelIDsForPlatform），因此能力快照与列表接口不会出现
// 「同一把 key 两个口径」的分歧。
//
// 设计归属：internal/lanes（ADR-0024 只增不改）；本文件为新增文件，不改任何上游文件。
// 只读：不写状态、不计费、不触上游、不探测。
func (h *GatewayHandler) LanesCapabilityModels(ctx context.Context, apiKey *service.APIKey) (available, source []string) {
	if h == nil || h.gatewayService == nil {
		return nil, nil
	}
	var groupID *int64
	var platform string
	if apiKey != nil && apiKey.Group != nil {
		groupID = &apiKey.Group.ID
		platform = apiKey.Group.Platform
	}
	if platform == service.PlatformComposite {
		available = h.compositeAvailableModels(ctx, groupID, "", true)
	} else {
		available = h.gatewayService.GetAvailableModels(ctx, groupID, platform)
	}
	return available, modelListingSource(platform, available, defaultModelIDsForPlatform(platform))
}
