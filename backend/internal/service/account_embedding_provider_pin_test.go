package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newEmbeddingsPinTestContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func newEmbeddingsPinTestService(upstream *httpUpstreamRecorder) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
}

// gjsonValues 把 gjson 数组结果摊成字符串切片，便于直接断言（避免
// []gjson.Result 与 []any 的类型噪音）。
func gjsonValues(body []byte, path string) []string {
	raw := gjson.GetBytes(body, path).Array()
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.String())
	}
	return out
}

func embeddingsPinUpstream() *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"pin-rid"},
		},
		Body: io.NopCloser(strings.NewReader(`{
			"object":"list",
			"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
			"model":"qwen/qwen3-embedding-8b",
			"usage":{"prompt_tokens":7,"total_tokens":7}
		}`)),
	}}
}

func embeddingsPinAccount(extra map[string]any) *Account {
	return &Account{
		ID:       77,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-pin",
			"base_url": "https://openrouter.ai/api/v1",
		},
		Extra: extra,
	}
}

// 配了账号级钉扎 → 上游收到 {"provider":{"only":["Nebius"]}}
func TestForwardEmbeddings_InjectsAccountProviderPin(t *testing.T) {
	body := []byte(`{"model":"qwen/qwen3-embedding-8b","input":"hi","dimensions":1024}`)
	c, _ := newEmbeddingsPinTestContext(t, body)
	upstream := embeddingsPinUpstream()
	svc := newEmbeddingsPinTestService(upstream)

	_, err := svc.ForwardEmbeddings(context.Background(), c,
		embeddingsPinAccount(map[string]any{EmbeddingProviderOnlyExtraKey: "Nebius"}), body, "")

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, c.Writer.Status())
	require.Equal(t, []string{"Nebius"}, gjsonValues(upstream.lastBody, "provider.only"))
	// 既有字段不被破坏
	require.Equal(t, "qwen/qwen3-embedding-8b", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, int64(1024), gjson.GetBytes(upstream.lastBody, "dimensions").Int())
}

// 调用方自带 provider → 被账号级钉扎覆盖，不透传调用方所填值
func TestForwardEmbeddings_ClientProviderIsOverriddenByAccountPin(t *testing.T) {
	body := []byte(`{"model":"qwen/qwen3-embedding-8b","input":"hi","provider":{"only":["DeepInfra"]}}`)
	c, _ := newEmbeddingsPinTestContext(t, body)
	upstream := embeddingsPinUpstream()
	svc := newEmbeddingsPinTestService(upstream)

	_, err := svc.ForwardEmbeddings(context.Background(), c,
		embeddingsPinAccount(map[string]any{EmbeddingProviderOnlyExtraKey: "Nebius,DeepInfra"}), body, "")

	require.NoError(t, err)
	// 逗号分隔名单解析为多元素，且 DeepInfra 只出现在账号名单里（调用方原值被覆盖）
	require.Equal(t, []string{"Nebius", "DeepInfra"}, gjsonValues(upstream.lastBody, "provider.only"))
}

// 未配置钉扎 → body 不含 provider 字段（零行为变化，保持与直连单一上游一致）
func TestForwardEmbeddings_NoPinLeavesBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"qwen/qwen3-embedding-8b","input":"hi"}`)
	c, _ := newEmbeddingsPinTestContext(t, body)
	upstream := embeddingsPinUpstream()
	svc := newEmbeddingsPinTestService(upstream)

	for _, extra := range []map[string]any{
		nil,
		{},
		{EmbeddingProviderOnlyExtraKey: ""},
		{EmbeddingProviderOnlyExtraKey: "  "},
		{EmbeddingProviderOnlyExtraKey: nil},
	} {
		_, err := svc.ForwardEmbeddings(context.Background(), c,
			embeddingsPinAccount(extra), body, "")
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(upstream.lastBody, "provider").Exists(),
			"未配置/置空时不得下发 provider 字段: extra=%v", extra)
	}
}

// 配置位校验：非字符串值在 create 路径被拒；空串/纯空白归一化为删键
func TestEmbeddingProviderOnlyExtraValidationAndNormalize(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateEmbeddingProviderOnlyExtra(PlatformOpenAI,
		map[string]any{EmbeddingProviderOnlyExtraKey: "Nebius"}))
	require.NoError(t, ValidateEmbeddingProviderOnlyExtra(PlatformOpenAI,
		map[string]any{EmbeddingProviderOnlyExtraKey: nil}))

	err := ValidateEmbeddingProviderOnlyExtra(PlatformOpenAI,
		map[string]any{EmbeddingProviderOnlyExtraKey: 42})
	require.Error(t, err)
	require.Contains(t, err.Error(), "embedding_provider_only")

	// 非 openai 平台不介入
	require.NoError(t, ValidateEmbeddingProviderOnlyExtra(PlatformAnthropic,
		map[string]any{EmbeddingProviderOnlyExtraKey: 42}))

	normalized, err := normalizeEmbeddingProviderOnlyExtra(PlatformOpenAI,
		map[string]any{EmbeddingProviderOnlyExtraKey: "  ", "other": "keep"})
	require.NoError(t, err)
	require.NotContains(t, normalized, EmbeddingProviderOnlyExtraKey)
	require.Equal(t, "keep", normalized["other"])

	normalized, err = normalizeEmbeddingProviderOnlyExtra(PlatformOpenAI,
		map[string]any{EmbeddingProviderOnlyExtraKey: " Nebius , DeepInfra "})
	require.NoError(t, err)
	require.Equal(t, " Nebius , DeepInfra ", normalized[EmbeddingProviderOnlyExtraKey])
}

func (r *httpUpstreamRecorder) RecordAIMDResult(accountID int64, statusCode int) {}
