package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/sjson"
)

// EmbeddingProviderOnlyExtraKey 是账号级嵌入上游 provider 钉扎配置位
// （accounts.extra）：值形如 "Nebius" / "Nebius,DeepInfra"（逗号分隔名单）。
//
// 为什么必须有它：聚合渠道（如 OpenRouter）对同一模型 slug 常挂多个 provider，
// 各家量化/实现不同 → 同一段文本多次调用返回不同向量（实测同文本连调 10 次，
// 不钉扎 min_cos=0.996511；钉 Nebius min_cos=0.999925，钉 DeepInfra 仅
// 0.997614 不过门）。后果比"质量稍差"严重得多：库内向量与查询向量可能来自
// 不同空间，检索静默漂移且无人能发现。
//
// 语义：词元在 embeddings 转发时按本配置注入/覆盖请求体的 provider 选择器
// （{"only":[...]}），调用方自带的同名字段被覆盖而非透传——否则调用方既能
// 覆盖钉扎，而部分消费方（原本直连单一上游）根本不带该字段。
const EmbeddingProviderOnlyExtraKey = "embedding_provider_only"

// ValidateEmbeddingProviderOnlyExtra 校验账号级嵌入 provider 钉扎值：
// 必须是字符串或 nil；空串/纯空白视为"未配置"（由 normalize 删除该键）。
func ValidateEmbeddingProviderOnlyExtra(platform string, extra map[string]any) error {
	if platform != PlatformOpenAI || extra == nil {
		return nil
	}
	raw, exists := extra[EmbeddingProviderOnlyExtraKey]
	if !exists || raw == nil {
		return nil
	}
	if _, ok := raw.(string); !ok {
		return infraerrors.BadRequest(
			"EMBEDDING_PROVIDER_ONLY_INVALID",
			"embedding_provider_only must be a string (comma-separated provider names) or null",
		)
	}
	return nil
}

// normalizeEmbeddingProviderOnlyExtra 归一化该配置位：空串/纯空白一律删键，
// 使"未配置"与"显式置空"在存储层同义（读侧只看存在性）。
func normalizeEmbeddingProviderOnlyExtra(platform string, extra map[string]any) (map[string]any, error) {
	if platform != PlatformOpenAI {
		return extra, nil
	}
	if err := ValidateEmbeddingProviderOnlyExtra(platform, extra); err != nil {
		return nil, err
	}
	if extra == nil {
		return nil, nil
	}
	normalized := shallowCopyMap(extra)
	if raw, ok := normalized[EmbeddingProviderOnlyExtraKey]; ok {
		if s, isStr := raw.(string); !isStr || strings.TrimSpace(s) == "" {
			delete(normalized, EmbeddingProviderOnlyExtraKey)
		}
	}
	return normalized, nil
}

// normalizeEmbeddingProviderOnlyUpdateExtra 处理账号更新：仅在调用方显式提交了
// extra 时参与（与其它 normalize*UpdateExtra 同口径），未提交则保持原值不动。
func normalizeEmbeddingProviderOnlyUpdateExtra(
	account *Account,
	input *UpdateAccountInput,
	normalized map[string]any,
) (map[string]any, error) {
	if account == nil || input == nil {
		return normalized, nil
	}
	if _, ok := input.Extra[EmbeddingProviderOnlyExtraKey]; !ok {
		return normalized, nil
	}
	raw := input.Extra[EmbeddingProviderOnlyExtraKey]
	if raw == nil {
		// 显式置 null = 移除钉扎（与"未配置"同义）。
		delete(normalized, EmbeddingProviderOnlyExtraKey)
		return normalized, nil
	}
	if _, ok := raw.(string); !ok {
		return nil, infraerrors.BadRequest(
			"EMBEDDING_PROVIDER_ONLY_INVALID",
			"embedding_provider_only must be a string (comma-separated provider names) or null",
		)
	}
	normalized[EmbeddingProviderOnlyExtraKey] = raw
	return normalizeEmbeddingProviderOnlyExtra(account.Platform, normalized)
}

// embeddingProviderOnlyList 解析账号级 provider 钉扎为上游选择器名单。
// 返回 nil 表示未配置（不下发该字段，保持与直连单一上游的既有行为一致）。
func embeddingProviderOnlyList(account *Account) []string {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[EmbeddingProviderOnlyExtraKey]
	if !ok || raw == nil {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil
	}
	names := make([]string, 0, 2)
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// applyEmbeddingProviderPin 按账号级钉扎注入/覆盖 embeddings 请求体的 provider
// 选择器。未配置时原样返回（不改写 body，零行为变化）。
//
// 只增不改：仅作用于 embeddings 转发路径，其它端点的请求体不受影响。
func applyEmbeddingProviderPin(body []byte, account *Account) []byte {
	names := embeddingProviderOnlyList(account)
	if len(names) == 0 || len(body) == 0 {
		return body
	}
	updated, err := sjson.SetBytes(body, "provider", map[string]any{"only": names})
	if err != nil {
		// 注入失败不阻断请求：按未钉扎放行并如实记录，由调用方/运维发现后修配置。
		// 上游仍会返回向量，只是失去向量一致性保证——这优于让嵌入能力整体不可用。
		return body
	}
	return updated
}
