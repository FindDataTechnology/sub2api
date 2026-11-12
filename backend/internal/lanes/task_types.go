package lanes

import (
	"strings"
	"time"
)

// 任务车道（add-ciyuan-task-lane）：类型、状态机与失败分流。
//
// 定位：`POST /v1/ciyuan/tasks` + `GET /v1/ciyuan/tasks/{id}`，与仓内
// batch_image 的 submit/poll 同形。**不改 /v1/chat/completions 语义**：
// 不声明 mode=defer 的调用方行为与今天逐字一致（D1/D2）。
//
// 执行方式：车道自己**不转发、不计费**，而是把请求体原样重放进同一个 gin 引擎的
// /v1/chat/completions（见 task_executor.go）。这样鉴权、分组门、账号调度、
// per-account 队列、失败转移、RecordUsage 全部走既有的那一套——
// 「第二套账」在物理上不存在（D5 / Risks 第一条）。

// 任务状态。终态只有 completed / failed：不引入「取消」这条第四态（Q-c）。
const (
	TaskStatusPending   = "pending"
	TaskStatusRunning   = "running"
	TaskStatusCompleted = "completed"
	TaskStatusFailed    = "failed"
)

// 任务模式。
const (
	TaskModeNow   = "now"   // 立刻执行：等价于一次同步调用；容量不足即 429，不建行
	TaskModeDefer = "defer" // 入队等待：容量不足保持 pending，直到成功或到 deadline
)

// 尝试结论（写入 lanes_task_attempts.outcome，也是失败分流的唯一判据）。
const (
	AttemptCompleted       = "completed"
	AttemptRetryQueueFull  = "retry_queue_full"
	AttemptRetryTransient  = "retry_transient"
	AttemptFailedClient    = "failed_client"
	AttemptFailedDeadline  = "failed_deadline"
	AttemptFailedPermanent = "failed_permanent"
)

// 失败原因码（回给调用方，机器可读）。
const (
	ReasonDeadlineExceeded   = "deadline_exceeded"
	ReasonInfeasibleDeadline = "TASK_INFEASIBLE_BY_DEADLINE"
	ReasonQueueLimit         = "TASK_QUEUE_LIMIT_REACHED"
	ReasonAPIKeyUnavailable  = "api_key_unavailable"
	ReasonLaneUnavailable    = "task_lane_unavailable"
	ReasonClientError        = "client_error"
	ReasonUpstreamError      = "upstream_error"
)

// 车道调参：全部是「机制」参数，**不含任何账号维度**（不读账号数、不读 Concurrency）。
// 账号拓扑变化（增删/改并发/冷却）不需要改这里任何一项——I4。
const (
	// taskWorkerConcurrency 同时在跑的尝试数。刻意保守：网关自己还有 per-account
	// 有界队列，车道再做一次慢速并发，两层的乘积才是真正的上游压力。
	taskWorkerConcurrency = 2
	// taskBackoffBase/Max 退避区间。429/5xx 走指数退避，避免把压力还给上游。
	taskBackoffBase = 5 * time.Second
	taskBackoffMax  = 5 * time.Minute
	// taskMaxAttempts 单任务尝试上限（超过即 failed，不再无限重试）。
	taskMaxAttempts = 6
	// taskEstDuration 单次执行的保守耗时估计（用于 deadline 可行性判定）。
	// 取观测到的 p95（约 86s）向上取整，宁可判不可行也不要收下再失败。
	taskEstDuration = 120 * time.Second
	// taskAttemptTimeout 单次尝试的硬上限。必须是 taskEstDuration 的倍数：
	// 拿估计值本身当超时会误杀「正常但偏慢」的那批，所以给到 3 倍。
	//
	// 为什么必须有：并发只有 taskWorkerConcurrency=2，一个挂住的上游就把半个
	// 车道占死；而且取消后往往是空响应，classifyAttempt(0) 走 default 落成
	// client error（不可重试）——「上游抖动」会被写成「永久失败」。
	taskAttemptTimeout = 3 * taskEstDuration
	// taskUnknownRPS 没有任何观测时的保守容量假设（S0 实测整类 p50 = 0.117 rps，
	// 这里取更低值，宁可把等待时间估长导致当场拒，也不要收下注定超期的任务）。
	taskUnknownRPS = 0.05
	// taskQueueLimitPerKey 单 key 同时未完成的 defer 任务上限（防无界排队）。
	taskQueueLimitPerKey = 32
	// taskResultMaxBytes 结果体保留上限；超出截断并置 result_truncated。
	taskResultMaxBytes = 256 << 10
	// taskThroughputWindow 观测完成速率的时间窗。
	taskThroughputWindow = 10 * time.Minute
)

// taskBodyMeta 是从客户端请求体里抽出的调度所需字段。
type taskBodyMeta struct {
	Model        string
	Stream       bool
	MaxTokens    int
	PromptTokens int // 粗估：字节数/4（只用于「值不值得等」的决策，不进账单）
}

// classifyAttempt 把一次重放的 HTTP 结果分流成尝试结论。
//
// 口径（task 3.3）：
//
//	2xx                       → completed
//	429                       → retry_queue_full（容量信号，退避后重排）
//	5xx                       → retry_transient（上游抖动，退避后重试）
//	其余 4xx / 其余           → failed_client（参数/权限问题，重试无意义）
//
// 不按错误文案分类：文案会随上游改版漂移（本仓吃过「失败文案被改写导致分类器不认」
// 的亏），状态码是稳定契约。
func classifyAttempt(status int) string {
	switch {
	case status >= 200 && status < 300:
		return AttemptCompleted
	case status == 429:
		return AttemptRetryQueueFull
	case status >= 500:
		return AttemptRetryTransient
	default:
		return AttemptFailedClient
	}
}

func outcomeRetryable(outcome string) bool {
	return outcome == AttemptRetryQueueFull || outcome == AttemptRetryTransient
}

// backoffDelay 计算第 attempt 次失败后的退避。
//
// 指数退避 + 上限；**不加随机抖动**（单副本、无 herd 问题，加了反而让
// 测试与排障不可复现）。扩副本时应加抖动，届时改动点在函数内、调用方不变。
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := taskBackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= taskBackoffMax {
			return taskBackoffMax
		}
	}
	return d
}

// taskRequestID 生成一次尝试的 request_id。
//
// **必须含尝试号**：usage_logs 上有 (request_id, api_key_id) 唯一索引（迁移 027），
// 重试复用同一个 id 会让第二次的用量行写不进去 —— 任务显示成功、账单缺失。
// 该值经 ctxkey.ClientRequestID 注入重放请求，最终落成 usage_logs.request_id
// 的 "client:<id>"，即任务行与账单行之间的唯一血缘。
func taskRequestID(jobID int64, attemptNo int) string {
	return "task-" + itoa(jobID) + "-" + itoa(int64(attemptNo))
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// truncateResult 截断过大的结果体，并如实标注（不静默丢）。
func truncateResult(body []byte) ([]byte, bool) {
	if len(body) <= taskResultMaxBytes {
		return body, false
	}
	return body[:taskResultMaxBytes], true
}

func trimOrEmpty(s string) string { return strings.TrimSpace(s) }
