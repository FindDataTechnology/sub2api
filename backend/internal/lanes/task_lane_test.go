//go:build unit

package lanes

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 任务车道的口径测试（add-ciyuan-task-lane）。
//
// 这里刻意只测**纯函数与判据**：与 DB/引擎交互的部分要靠真机验证（S5.3），
// 把「我自己的表达式」包一层 mock 只会得到同义反复的绿灯。

func TestClassifyAttempt(t *testing.T) {
	cases := []struct {
		status int
		want   string
		why    string
	}{
		{200, AttemptCompleted, "2xx 成功"},
		{201, AttemptCompleted, "2xx 成功"},
		{429, AttemptRetryQueueFull, "429 是容量信号，必须可重排"},
		{500, AttemptRetryTransient, "5xx 是上游抖动，退避重试"},
		{503, AttemptRetryTransient, "503 同上"},
		{400, AttemptFailedClient, "参数错重试无意义"},
		{401, AttemptFailedClient, "鉴权错重试无意义"},
		{404, AttemptFailedClient, "路径/模型不存在重试无意义"},
		{0, AttemptFailedClient, "没拿到状态码按不可重试处理（避免无限重试）"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, classifyAttempt(tc.status), "%s (status=%d)", tc.why, tc.status)
	}
}

func TestOutcomeRetryable(t *testing.T) {
	require.True(t, outcomeRetryable(AttemptRetryQueueFull))
	require.True(t, outcomeRetryable(AttemptRetryTransient))
	require.False(t, outcomeRetryable(AttemptCompleted))
	require.False(t, outcomeRetryable(AttemptFailedClient))
	require.False(t, outcomeRetryable(AttemptFailedPermanent))
}

// 每次尝试的 request_id 必须不同：usage_logs 上有 (request_id, api_key_id) 唯一索引，
// 复用会让第二次的用量行写不进去（任务成功、账单缺失）。这条测试是那个陷阱的钉子。
func TestTaskRequestIDIsUniquePerAttempt(t *testing.T) {
	a := taskRequestID(42, 1)
	b := taskRequestID(42, 2)
	c := taskRequestID(43, 1)
	require.NotEqual(t, a, b, "同一任务的不同尝试必须不同 id，否则撞 usage_logs 唯一索引")
	require.NotEqual(t, a, c, "不同任务必须不同 id")
	require.Equal(t, "task-42-1", a)
	require.Equal(t, "task-42-2", b)
	require.LessOrEqual(t, len(a), 64, "usage_logs.request_id 是 VARCHAR(64)")
}

// mode=now 的 request_id 会进 middleware.normalizeCorrelationID，
// 含非法字符会被改写成 uuid 从而断掉血缘 —— 字符集必须是 [a-z0-9-]。
func TestTaskNowRequestIDCharset(t *testing.T) {
	id := taskNowRequestID()
	require.Regexp(t, `^[a-z0-9-]+$`, id)
	require.NotEqual(t, id, taskNowRequestID(), "两次生成必须不同")
	require.LessOrEqual(t, len("client:"+id), 64)
}

func TestBackoffDelay(t *testing.T) {
	require.Equal(t, 5*time.Second, backoffDelay(1))
	require.Equal(t, 10*time.Second, backoffDelay(2))
	require.Equal(t, 20*time.Second, backoffDelay(3))
	require.Equal(t, 40*time.Second, backoffDelay(4))
	require.Equal(t, taskBackoffMax, backoffDelay(12), "必须封顶，不能无限增长")
	require.Equal(t, 5*time.Second, backoffDelay(0), "非法尝试号按第 1 次处理")
}

func TestReplayBodyStripsControlFields(t *testing.T) {
	raw := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"mode":"defer","external_id":"abc","deadline_seconds":3600,"min_saving_ratio":0.3}`)
	out, meta, err := replayBody(raw)
	require.NoError(t, err)
	require.Equal(t, "deepseek-v4.1-flash", meta.Model)
	require.True(t, meta.Stream, "要如实回报客户端原本是否请求了流式")

	body := string(out)
	require.NotContains(t, body, `"mode"`, "任务车道控制字段不得污染上游请求")
	require.NotContains(t, body, `"external_id"`)
	require.NotContains(t, body, `"deadline_seconds"`)
	require.NotContains(t, body, `"min_saving_ratio"`)
	require.Contains(t, body, `"stream":false`, "异步任务没有流式消费者，必须强制非流式")
	require.Contains(t, body, `"content":"hi"`, "业务字段必须原样保留")
}

func TestReplayBodyRequiresModel(t *testing.T) {
	_, _, err := replayBody([]byte(`{"messages":[]}`))
	require.Error(t, err)
}

// 固定时刻：2026-11-11 是周三；02:00 UTC 落在 DeepSeek 高峰段 [01:00,04:00)。
func peakTime() time.Time    { return time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC) }
func offpeakTime() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }

func TestPriceSnapshotAt(t *testing.T) {
	peak := priceSnapshotAt(nil, peakTime())
	require.Equal(t, 2.0, peak.PeakMultiplier)
	require.Equal(t, time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC), peak.NextOffpeakStart,
		"高峰段 [01,04)，下一个低谷必须是 04:00 UTC")

	off := priceSnapshotAt(nil, offpeakTime())
	require.Equal(t, 1.0, off.PeakMultiplier)
	require.True(t, off.NextOffpeakStart.IsZero(), "已在低谷时不应报下一个低谷")
	require.True(t, off.PeakEndsAt.IsZero())
}

func TestDecideDefer(t *testing.T) {
	now := peakTime()
	snap := priceSnapshotAt(nil, now)
	deadline := now.Add(24 * time.Hour)

	d := decideDefer(now, &deadline, snap, 0.2)
	require.True(t, d.Defer)
	require.NotNil(t, d.DeferredUntil)
	require.Equal(t, time.Date(2026, 11, 11, 4, 0, 0, 0, time.UTC), *d.DeferredUntil)
	require.InDelta(t, 0.5, d.SavingRatio, 1e-9, "2× 峰谷 ⇒ 省 50%")
	require.Contains(t, d.Reason, "deferred_to_offpeak", "推迟必须留下可读理由")

	t.Run("低谷不推迟", func(t *testing.T) {
		off := priceSnapshotAt(nil, offpeakTime())
		d := decideDefer(offpeakTime(), &deadline, off, 0.2)
		require.False(t, d.Defer)
		require.Equal(t, "already_offpeak", d.Reason)
	})

	t.Run("省得不够就不等", func(t *testing.T) {
		// 因子 2 ⇒ 省 50%；门槛抬到 60% 就必须否决，且要说明原因
		d := decideDefer(now, &deadline, snap, 0.6)
		require.False(t, d.Defer)
		require.Contains(t, d.Reason, "saving_too_small")
	})

	t.Run("截止太紧就不等", func(t *testing.T) {
		tight := now.Add(30 * time.Second) // 连 est_duration 都不够
		d := decideDefer(now, &tight, snap, 0.2)
		require.False(t, d.Defer)
		require.Contains(t, d.Reason, "deadline_too_tight", "否决原因必须可观测，不静默")
	})

	t.Run("无截止时间可推迟", func(t *testing.T) {
		d := decideDefer(now, nil, snap, 0.2)
		require.True(t, d.Defer)
	})
}

func TestBuildCostEstimate(t *testing.T) {
	snap := priceSnapshotAt(nil, peakTime())

	none := buildCostEstimate(nil, snap)
	require.Nil(t, none.AtPeakUSD, "无历史样本时必须给 null，不能编造数字")
	require.Nil(t, none.AtOffpeakUSD)
	require.Equal(t, "no_historical_samples", none.Source)

	base := 0.01
	est := buildCostEstimate(&base, snap)
	require.NotNil(t, est.AtPeakUSD)
	require.NotNil(t, est.AtOffpeakUSD)
	require.InDelta(t, 0.02, *est.AtPeakUSD, 1e-9, "高峰 = 基数 × 2")
	require.InDelta(t, 0.01, *est.AtOffpeakUSD, 1e-9, "低谷 = 基数 × 1")
}

func TestFeasibleByDeadline(t *testing.T) {
	now := peakTime()

	ok, _, _ := feasibleByDeadline(now, nil, 100, 0.1)
	require.True(t, ok, "没有截止时间的任务永远可行")

	now := peakTime()
	deadline := now.Add(2 * time.Hour) // 7200s

	feasible, wait, _ := feasibleByDeadline(now, &deadline, 60, 0.1) // 600s + 120s
	require.True(t, feasible)
	require.InDelta(t, 600, wait.Seconds(), 1)

	feasible, _, _ = feasibleByDeadline(now, &deadline, 1000, 0.1) // 10000s + 120s > 7200s
	require.False(t, feasible, "排不完就必须当场拒，不能收下再报 deadline_exceeded")

	// 无观测时用保守兜底速率（taskUnknownRPS=0.05），仍要能得出结论而非除零。
	feasible, wait, _ = feasibleByDeadline(now, &deadline, 10, 0)
	require.True(t, feasible)
	require.InDelta(t, 200, wait.Seconds(), 1)
}

func TestTruncateResult(t *testing.T) {
	small := []byte(`{"ok":true}`)
	out, truncated := truncateResult(small)
	require.False(t, truncated)
	require.Equal(t, small, out)

	big := make([]byte, taskResultMaxBytes+10)
	_, truncated = truncateResult(big)
	require.True(t, truncated, "超大结果必须如实标注截断，不静默丢")
}

func TestTruncateJSONWrapsNonJSON(t *testing.T) {
	out, _ := truncateJSON([]byte("upstream returned plain text"))
	require.JSONEq(t, `{"raw":"upstream returned plain text"}`, string(out),
		"非 JSON 响应要包一层，保证 jsonb 列可写")
}

func TestParseDeadline(t *testing.T) {
	now := peakTime()
	got, err := parseDeadline([]byte(`{"deadline":"2026-11-12T02:00:00Z"}`), now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC), *got)

	got, err = parseDeadline([]byte(`{"deadline_seconds":600}`), now)
	require.NoError(t, err)
	require.Equal(t, now.Add(600*time.Second), *got)

	_, err = parseDeadline([]byte(`{"deadline":"not-a-time"}`), now)
	require.ErrorIs(t, err, errDeadlineFormat)

	_, err = parseDeadline([]byte(`{"deadline":"2020-01-01T00:00:00Z"}`), now)
	require.ErrorIs(t, err, errDeadlinePast)

	got, err = parseDeadline([]byte(`{}`), now)
	require.NoError(t, err)
	require.Nil(t, got)
}

// I4 的静态保证：任务车道的调度参数里不得出现任何账号维度标识。
// 这条测试不证明运行时行为，但能挡住「有人把 account.Concurrency 加进来」这类回归。
func TestNoAccountDerivedConstants(t *testing.T) {
	// 结构性断言：如果将来有人把账号数量/并发写进调度参数，必须同时改这里。
	require.Equal(t, 2, taskWorkerConcurrency)
	require.Equal(t, 0.05, taskUnknownRPS)
	require.Equal(t, 120*time.Second, taskEstDuration)

	// 容量的唯一来源是实测：无观测时用兜底常量，绝不从账号推。
	require.Greater(t, taskUnknownRPS, 0.0, "兜底速率必须为正，否则队列等待估算除零")
}

var _ = service.LanesTaskJob{}
