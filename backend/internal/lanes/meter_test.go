package lanes

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Meter 单元测（tasks 2.3）：WriteUsage 的入参形状=AlphaSearch 参考实现同构
// （原生账本写入本体由上游自身测试覆盖）；pool=nil 走同步兜底。
func TestMeterWriteUsageBuildsNativeRecordInput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type captured struct {
		ctxErrAtRecord error
		input          *service.OpenAIRecordUsageInput
	}
	got := make(chan captured, 1)
	record := func(ctx context.Context, input *service.OpenAIRecordUsageInput) error {
		got <- captured{ctxErrAtRecord: ctx.Err(), input: input}
		return nil
	}

	apiKey := &service.APIKey{
		ID:     100,
		UserID: 7,
		Key:    "lanes-meter-test-key",
		User:   &service.User{ID: 7, Balance: 10},
		Group:  &service.Group{ID: 42, Platform: service.PlatformOpenAI},
	}
	account := &service.Account{ID: 3000, Platform: "serpapi"}
	meter := &Meter{recordUsage: record} // pool=nil → 同步兜底路径

	w := gin.New()
	w.POST("/v1/search", func(c *gin.Context) {
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             "websearch",
			Model:            "websearch:serpapi",
			UpstreamModel:    "serpapi:google",
			RequestID:        "req-lane-1",
			Account:          account,
			WebSearchCalls:   1,
			UpstreamEndpoint: "/serp_api/search",
			Duration:         150 * time.Millisecond,
		})
		c.Status(200)
	})
	req := httptest.NewRequest("POST", "/v1/search", nil)
	req.Header.Set("User-Agent", "lanes-meter-test/1.0")
	req.Header.Set("X-Session-Id", "sess-lane-1")
	w.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case c := <-got:
		input := c.input
		require.Equal(t, "req-lane-1", input.Result.RequestID)
		require.Equal(t, "websearch:serpapi", input.Result.Model)
		require.Equal(t, "serpapi:google", input.Result.UpstreamModel)
		require.Equal(t, 1, input.Result.WebSearchCalls)
		require.Nil(t, input.Result.AudioUsage)
		require.Equal(t, int64(100), input.APIKey.ID)
		require.Equal(t, int64(7), input.User.ID)
		require.Equal(t, int64(3000), input.Account.ID)
		require.Equal(t, "/v1/search", input.InboundEndpoint)
		require.Equal(t, "/serp_api/search", input.UpstreamEndpoint)
		require.Equal(t, "lanes-meter-test/1.0", input.UserAgent)
		require.Equal(t, "sess-lane-1", input.SessionID)
		require.Equal(t, service.QuotaPlatform(context.WithoutCancel(req.Context()), apiKey), input.QuotaPlatform)
		// 请求取消不传导进计量任务（mandatory 语义：扣费不因响应提前返回而丢）。
		// 计量任务执行瞬间不受请求取消影响（mandatory 语义：扣费不因响应提前返回
		// 而丢）。任务返回后 defer cancel() 会正常取消 ctx，故在 record 内部快照断言。
		require.NoError(t, c.ctxErrAtRecord)
	case <-time.After(2 * time.Second):
		t.Fatal("record usage was not invoked")
	}
}

// 池化路径：任务经 UsageRecordWorkerPool 异步执行，入参同构。
func TestMeterWriteUsageViaPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	got := make(chan *service.OpenAIRecordUsageInput, 1)
	record := func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
		got <- input
		return nil
	}
	meter := &Meter{
		recordUsage: record,
		pool: service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
			WorkerCount: 1,
			QueueSize:   4,
		}),
	}
	t.Cleanup(meter.Stop)

	apiKey := &service.APIKey{ID: 101, User: &service.User{ID: 8}, Group: &service.Group{ID: 42}}
	w := gin.New()
	w.POST("/v1/audio/speech", func(c *gin.Context) {
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:      "audio",
			Model:     "audio:tts",
			RequestID: "req-lane-2",
			Account:   &service.Account{ID: 3001},
			Audio:     &service.AudioUsage{Mode: "tts", DurationOrUnits: 120},
			Duration:  time.Second,
		})
		c.Status(200)
	})
	w.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/audio/speech", nil))

	select {
	case input := <-got:
		require.Equal(t, "req-lane-2", input.Result.RequestID)
		require.NotNil(t, input.Result.AudioUsage)
		require.Equal(t, "tts", input.Result.AudioUsage.Mode)
		require.Equal(t, 120.0, input.Result.AudioUsage.DurationOrUnits)
		require.Zero(t, input.Result.WebSearchCalls)
		require.Equal(t, "/v1/audio/speech", input.InboundEndpoint)
	case <-time.After(2 * time.Second):
		t.Fatal("pooled record usage was not invoked")
	}
}

func TestMeterNilGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var meter *Meter
	require.NotPanics(t, func() { meter.Stop() })

	m := &Meter{} // 无 recordUsage：静默不记
	w := gin.New()
	w.POST("/v1/rerank", func(c *gin.Context) {
		m.WriteUsage(c, &service.APIKey{ID: 1, User: &service.User{ID: 1}}, UsageInput{Lane: "rerank"})
		c.Status(200)
	})
	require.NotPanics(t, func() {
		w.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/rerank", nil))
	})
}
