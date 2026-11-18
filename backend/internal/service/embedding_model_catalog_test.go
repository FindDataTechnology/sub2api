package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

// fork 补丁（ciyuan-v0.2.16-16）：聚合渠道的 /v1/models 不列 embedding 模型，
// 上游默认集也没有——管理台的「账号可用模型」与「分组白名单候选」两处因此
// 永远看不到 embedding，运维无法在后台配置它。本组测试钉住三条不变量：
//   1. 既有条目一个不少、顺序不变（只增不改）
//   2. embedding 目录确实补进来了
//   3. 重复调用不会翻倍

func TestEmbeddingCatalogAppendsWithoutTouchingExisting(t *testing.T) {
	base := openai.DefaultModelIDs()
	before := make([]string, len(base))
	copy(before, base)

	out := withEmbeddingModels(base)

	require.Equal(t, before, base, "input slice must not be mutated")
	require.GreaterOrEqual(t, len(out), len(before))
	require.Equal(t, before, out[:len(before)], "existing entries keep their order and values")
	for _, id := range embeddingModelIDs {
		require.Contains(t, out, id, "embedding model %s must appear", id)
	}
}

func TestEmbeddingCatalogIsIdempotent(t *testing.T) {
	once := withEmbeddingModels(openai.DefaultModelIDs())
	twice := withEmbeddingModels(once)
	require.Equal(t, once, twice, "re-applying must not duplicate entries")
}

func TestEmbeddingCatalogDoesNotDuplicatePresetModel(t *testing.T) {
	// 若某天默认集里也出现了 embedding 模型，不得出现两条。
	in := []string{"gpt-5.6", embeddingModelIDs[0]}
	out := withEmbeddingModels(in)
	count := 0
	for _, id := range out {
		if id == embeddingModelIDs[0] {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestWithEmbeddingModelsForPickerCarriesPickerFields(t *testing.T) {
	out := WithEmbeddingModelsForPicker(openai.DefaultModels)
	require.GreaterOrEqual(t, len(out), len(openai.DefaultModels))
	require.Equal(t, openai.DefaultModels, out[:len(openai.DefaultModels)], "preset rows untouched")

	var found *openai.Model
	for i := range out {
		if out[i].ID == embeddingModelIDs[0] {
			found = &out[i]
			break
		}
	}
	require.NotNil(t, found, "embedding row must be appended")
	require.Equal(t, "model", found.Object)
	require.Equal(t, "model", found.Type)
	require.NotEmpty(t, found.DisplayName, "picker needs a display name")
}

func TestIsEmbeddingModelID(t *testing.T) {
	require.True(t, IsEmbeddingModelID("Qwen/Qwen3-Embedding-8B"))
	require.True(t, IsEmbeddingModelID("  qwen/qwen3-embedding-8b  "), "trim + case-insensitive")
	require.False(t, IsEmbeddingModelID("gpt-5.6"))
	require.False(t, IsEmbeddingModelID(""))
}

func TestEmbeddingModelIDsReturnsCopy(t *testing.T) {
	a := EmbeddingModelIDs()
	a[0] = "mutated"
	require.NotEqual(t, "mutated", EmbeddingModelIDs()[0], "callers must not be able to mutate the catalog")
}

// 生产事故回归（2026-10-29，账号 47 nebius-embedding-bulk）：
// 聚合渠道 /v1/models 一条 embedding 都没有 + 账号 mapping 只声明了
// Qwen3-Embedding-8B → projectAccountModelsBody 把 mapping 目标当「上游不存在」
// 丢弃 → GetAvailableModels 拿到**成功但空**的列表（err==nil），后台既看不见
// embedding 模型，也就无从配置细分渠道钉扎。
func TestFetchOpenAIAccountModelsRestoresMappedEmbeddingOnEmptyUpstream(t *testing.T) {
	gateway := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		// 聚合渠道典型响应：421 条 chat 模型，零个 embedding。
		return ordinaryModelsUpstreamResponse(`{"object":"list","data":[
			{"id":"gpt-5.6","owned_by":"openrouter"},
			{"id":"claude-4.5-sonnet","owned_by":"openrouter"}
		]}`), nil
	}})
	svc := &AccountTestService{openaiGatewayService: gateway}
	account := newCodexModelsAPIKeyTestAccount("https://openrouter.ai/api/v1")
	account.Credentials["model_mapping"] = map[string]any{
		"Qwen/Qwen3-Embedding-8B": "Qwen/Qwen3-Embedding-8B",
	}

	models, err := svc.FetchOpenAIAccountModels(context.Background(), account)
	require.NoError(t, err)

	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	require.Contains(t, ids, "Qwen/Qwen3-Embedding-8B",
		"mapped embedding must survive projection when the upstream catalog omits it")
	require.Len(t, models, 1, "only the declared embedding row is restored — chat models stay upstream-authoritative")
	require.Equal(t, "model", models[0].Type)
	require.NotEmpty(t, models[0].DisplayName)
	// 回归：上游有的 chat 模型该照旧出现，不能被 embedding 补齐逻辑影响。
	chatOnly, err := svc.FetchOpenAIAccountModels(context.Background(), newCodexModelsAPIKeyTestAccount("https://openrouter.ai/api/v1"))
	require.NoError(t, err)
	require.NotContains(t, collectModelIDs(chatOnly), "Qwen/Qwen3-Embedding-8B",
		"accounts without an embedding mapping must not gain embedding rows from the static catalog here")
}

// chat 模型不得因为这条补丁被凭空造出来：mapping 里没有 embedding、上游也没列，
// 结果就该是原样的（可能为空）列表。
func TestWithEmbeddingModelsFromMappingIgnoresNonEmbeddingTargets(t *testing.T) {
	account := &Account{
		ID:          9,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-2026"}},
	}
	out := WithEmbeddingModelsFromMapping(nil, account)
	require.Empty(t, out, "non-embedding mapping targets must not be synthesized")
}

func TestWithEmbeddingModelsFromMappingDedupesAndIgnoresWildcard(t *testing.T) {
	account := &Account{
		ID:       10,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"Qwen/Qwen3-Embedding-8B": "Qwen/Qwen3-Embedding-8B",
			"text-embedding-3-large":  "text-embedding-3-large",
			"*":                       "*",
		}},
	}
	existing := []openai.Model{{ID: "text-embedding-3-large", Object: "model", Type: "model", DisplayName: "OpenAI Embedding L"}}
	out := WithEmbeddingModelsFromMapping(existing, account)
	ids := collectModelIDs(out)
	require.Equal(t, []string{"text-embedding-3-large", "Qwen/Qwen3-Embedding-8B"}, ids,
		"existing entry keeps its position, wildcard is skipped, the rest is appended in stable order")
	require.Equal(t, "OpenAI Embedding L", out[0].DisplayName, "existing rows are not rewritten")
}

func collectModelIDs(models []openai.Model) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}
