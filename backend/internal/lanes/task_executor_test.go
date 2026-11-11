//go:build unit

package lanes

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 重放机制的真实验证（add-ciyuan-task-lane 的核心设计决策）。
//
// 这条测试必须**真的起一个 gin 引擎并真的 ServeHTTP**：
// 如果只断言「我构造的 request 长什么样」，那测的是测试自己，不是机制。
// 这里断言的是机制实际把什么交给了下游 —— 认证头、请求体、血缘 id、客户端 IP。

func newReplayTestEngine(t *testing.T) (*gin.Engine, *capturedRequest) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cap := &capturedRequest{}
	r := gin.New()
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		cap.Hits++
		cap.Authorization = c.GetHeader("Authorization")
		cap.UserAgent = c.GetHeader("User-Agent")
		cap.ForwardedFor = c.GetHeader("X-Forwarded-For")
		cap.ClientRequestID, _ = c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		var body map[string]any
		_ = json.NewDecoder(c.Request.Body).Decode(&body)
		cap.Body = body
		c.JSON(http.StatusOK, gin.H{"id": "chatcmpl-test", "usage": gin.H{"total_tokens": 7}})
	})
	return r, cap
}

type capturedRequest struct {
	Hits            int
	Authorization   string
	UserAgent       string
	ForwardedFor    string
	ClientRequestID string
	Body            map[string]any
}

func newTestExecutor(engine *gin.Engine, key string) *taskExecutor {
	return &taskExecutor{
		engine: engine,
		resolveKey: func(context.Context, int64) (string, error) {
			return key, nil
		},
	}
}

func TestExecutorReplaysIntoEngine(t *testing.T) {
	engine, cap := newReplayTestEngine(t)
	exec := newTestExecutor(engine, "sk-test-abc")

	body, _, err := replayBody([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true,"mode":"defer","external_id":"x"}`))
	require.NoError(t, err)

	job := &service.LanesTaskJob{
		ID: 42, APIKeyID: 7, Model: "deepseek-v4.1-flash",
		RequestBody: body, ClientIP: "203.0.113.9",
	}
	out := exec.run(context.Background(), job, 3)

	require.Equal(t, 1, cap.Hits, "必须真的打到引擎里的处理函数")
	require.Equal(t, http.StatusOK, out.HTTPStatus)
	require.Equal(t, AttemptCompleted, out.Outcome)
	require.Contains(t, string(out.Body), "chatcmpl-test", "响应体要能被调用方读回")

	require.Equal(t, "Bearer sk-test-abc", cap.Authorization, "重放必须带凭据")
	require.Equal(t, "task-42-3", cap.ClientRequestID,
		"血缘 id 必须是 task-<job>-<attempt>，否则 usage_logs 追不回来")
	require.Equal(t, "203.0.113.9", cap.ForwardedFor, "客户端 IP 必须跟随，否则带白名单的 key 会被自己拒掉")
	require.Equal(t, "ciyuan-task-lane/1", cap.UserAgent, "上游要能一眼看出这是重放")

	// 控制字段不得漏到上游。
	require.NotContains(t, cap.Body, "mode")
	require.NotContains(t, cap.Body, "external_id")
	require.Equal(t, "deepseek-v4.1-flash", cap.Body["model"])

	// 时间戳必须真的被填上：命名返回值 + defer 才写得到。
	// 零值 EndedAt 会让退避基准变成 0001-01-01（重试立即重打）且 p95/完成速率全失真。
	require.False(t, out.StartedAt.IsZero(), "StartedAt 不得为零值")
	require.False(t, out.EndedAt.IsZero(), "EndedAt 不得为零值（defer 必须写在返回值上）")
	require.False(t, out.EndedAt.Before(out.StartedAt), "EndedAt 不得早于 StartedAt")
}

func TestExecutorClassifiesUpstreamFailures(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusTooManyRequests, `{"error":{"message":"Upstream account queue full, please retry later"}}`, AttemptRetryQueueFull},
		{http.StatusServiceUnavailable, `{"error":{"message":"upstream down"}}`, AttemptRetryTransient},
		{http.StatusBadRequest, `{"error":{"message":"invalid model"}}`, AttemptFailedClient},
		{http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, AttemptFailedClient},
	}
	for _, tc := range cases {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		status, body := tc.status, tc.body
		r.POST("/v1/chat/completions", func(c *gin.Context) {
			c.Data(status, "application/json", []byte(body))
		})
		exec := newTestExecutor(r, "k")
		out := exec.run(context.Background(), &service.LanesTaskJob{
			ID: 1, RequestBody: []byte(`{"model":"m","messages":[]}`),
		}, 1)
		require.Equal(t, tc.want, out.Outcome, "status=%d", tc.status)
		if tc.want != AttemptCompleted {
			require.NotEmpty(t, out.ErrMsg, "失败必须带可读理由，否则 failed 状态无从解释")
		}
	}
}

func TestExecutorFailsLoudlyWhenKeyUnresolvable(t *testing.T) {
	engine, cap := newReplayTestEngine(t)
	exec := newTestExecutor(engine, "")

	out := exec.run(context.Background(), &service.LanesTaskJob{
		ID: 1, APIKeyID: 9, RequestBody: []byte(`{"model":"m","messages":[]}`),
	}, 1)

	require.Equal(t, AttemptFailedPermanent, out.Outcome)
	require.Contains(t, out.ErrMsg, ReasonAPIKeyUnavailable)
	require.Zero(t, cap.Hits, "取不到凭据就不该发出去")
}

func TestExecutorFailsLoudlyWhenEngineMissing(t *testing.T) {
	exec := &taskExecutor{resolveKey: func(context.Context, int64) (string, error) { return "k", nil }}
	out := exec.run(context.Background(), &service.LanesTaskJob{ID: 1, RequestBody: []byte(`{}`)}, 1)
	require.Equal(t, AttemptFailedPermanent, out.Outcome)
	require.Contains(t, out.ErrMsg, "engine")
}

func TestExecutorRespectsContextCancellation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	reached := false
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		reached = true
		c.JSON(http.StatusOK, gin.H{})
	})
	exec := newTestExecutor(r, "k")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := exec.run(ctx, &service.LanesTaskJob{ID: 1, RequestBody: []byte(`{"model":"m","messages":[]}`)}, 1)

	// gin 的 ServeHTTP 不看 ctx 取消，所以必须由执行器自己在发之前拦下来 ——
	// 否则「客户端已断开」的 mode=now 调用照样烧上游、照样计费。
	require.False(t, reached, "ctx 已取消时不得发出请求")
	require.Equal(t, AttemptFailedPermanent, out.Outcome)
	require.Contains(t, out.ErrMsg, "cancelled")
}

func TestBackoffStaysWithinDeadlineGate(t *testing.T) {
	// 调度器的截止硬约束用的是 backoffDelay + taskEstDuration；
	// 这里确认最坏退避不会让「还来得及」的任务被误判超期。
	now := time.Now()
	deadline := now.Add(30 * time.Minute)
	next := now.Add(backoffDelay(3)) // 20s
	require.False(t, next.Add(taskEstDuration).After(deadline),
		"20s 退避 + 120s 估计仍应落在 30min 截止内")
}
