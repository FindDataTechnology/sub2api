package service

import (
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
