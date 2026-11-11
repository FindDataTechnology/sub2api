package lanes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 执行器（add-ciyuan-task-lane S2.2/S2.3）：把任务**重放进同一个 gin 引擎**的
// /v1/chat/completions，而不是自己写一条转发链。
//
// 这是本 change 最重要的一条实现决策，理由：
//
//  1. 鉴权/分组门/IP 白名单/RPM 全部照旧 —— 车道不复制任何一条准入规则。
//  2. 账号调度与 per-account 有界队列照旧 —— 上层不需要知道账号存在（I4）。
//  3. 计费与用量写入照旧（RecordUsage）—— 「第二套账」在物理上不存在。
//  4. 峰谷计费按既有 PricingAt 在请求发起时捕获 —— 车道不改计价时刻规则。
//
// 血缘：重放请求的 ctxkey.ClientRequestID 被设为 task-<job_id>-<attempt>，
// 于是 usage_logs.request_id 落成 "client:task-<job>-<n>"，
// 任务行 → 账单行可以一对一追。**每次尝试必须不同的 id**，否则撞
// usage_logs 的 (request_id, api_key_id) 唯一索引（迁移 027）。

// taskRunner 是调度器与端点依赖的最小执行面。
//
// 抽成接口的唯一目的是让「调度状态机」可以被真库 + 桩执行器驱动着测：
// 截止淘汰、退避重排、终态分流这些判据不该依赖真的 LLM。
type taskRunner interface {
	runWithRequestID(ctx context.Context, job *service.LanesTaskJob, requestID string) attemptOutcome
}

// taskExecutor 执行一次尝试。
type taskExecutor struct {
	engine *gin.Engine
	// resolveKey 把 api_key_id 还原成可用的明文凭据（重放要带 Authorization）。
	//
	// 抽成函数而不是直接依赖 APIKeyService：
	//   - 测试可注入，不必装配整套 service；
	//   - key 落库形态一旦改成「哈希 + 单独加密副本」
	//     （harden-sub2api-credential-storage 在途），只需换这里的实现，
	//     车道的重放逻辑一行不改。
	resolveKey func(ctx context.Context, apiKeyID int64) (string, error)
}

// attemptOutcome 是一次重放的结果。
type attemptOutcome struct {
	RequestID  string
	HTTPStatus int
	Header     http.Header
	Outcome    string // classifyAttempt 的结果
	Body       []byte
	ErrMsg     string
	StartedAt  time.Time
	EndedAt    time.Time
}

// bufferedResponse 是内置的 ResponseWriter：车道要读回响应体，
// 又不想把 net/http/httptest 拖进生产代码路径。
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header)}
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) Status() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

// replayBody 把客户端原始请求体改写成「适合异步执行」的形状。
//
// 只做三件事：强制 stream=false（异步任务没有流式消费者）、
// 剥掉任务车道自己的控制字段（不能污染上游请求）、保留其余一切。
func replayBody(raw []byte) ([]byte, taskBodyMeta, error) {
	meta := taskBodyMeta{
		Model:     strings.TrimSpace(gjson.GetBytes(raw, "model").String()),
		Stream:    gjson.GetBytes(raw, "stream").Bool(),
		MaxTokens: int(gjson.GetBytes(raw, "max_tokens").Int()),
	}
	if meta.MaxTokens <= 0 {
		meta.MaxTokens = int(gjson.GetBytes(raw, "max_completion_tokens").Int())
	}
	meta.PromptTokens = len(raw) / 4
	if meta.Model == "" {
		return nil, meta, fmt.Errorf("model is required")
	}
	out := raw
	for _, field := range []string{"mode", "deadline", "deadline_seconds", "external_id", "min_saving_ratio"} {
		if gjson.GetBytes(out, field).Exists() {
			out, _ = sjson.DeleteBytes(out, field)
		}
	}
	if meta.Stream {
		out, _ = sjson.SetBytes(out, "stream", false)
	}
	return out, meta, nil
}

// run 执行一次尝试（尝试号决定 request_id）。
func (e *taskExecutor) run(ctx context.Context, job *service.LanesTaskJob, attemptNo int) attemptOutcome {
	return e.runWithRequestID(ctx, job, taskRequestID(job.ID, attemptNo))
}

// runWithRequestID 执行一次尝试，并显式指定 request_id。
//
// mode=now 时任务行还不存在（成功才落行），拿不到 job.ID，
// 故由调用方先生成一个字符集安全的 id 传进来。
func (e *taskExecutor) runWithRequestID(ctx context.Context, job *service.LanesTaskJob, requestID string) attemptOutcome {
	res := attemptOutcome{RequestID: requestID, StartedAt: time.Now()}
	defer func() { res.EndedAt = time.Now() }()

	// 已经在库里跑不了就别发：gin 的 ServeHTTP **不检查** ctx 取消，
	// 直接调用会让一个已断开的 mode=now 调用继续烧上游与计费。
	// 调度器用的是 Background，永远不受此影响（defer 任务不该被客户端断连取消）。
	if err := ctx.Err(); err != nil {
		res.Outcome = AttemptFailedPermanent
		res.ErrMsg = "request cancelled before execution: " + err.Error()
		return res
	}
	if e.engine == nil {
		res.Outcome = AttemptFailedPermanent
		res.ErrMsg = "task lane executor not wired (engine missing)"
		return res
	}
	if e.resolveKey == nil {
		res.Outcome = AttemptFailedPermanent
		res.ErrMsg = "task lane executor not wired (key resolver missing)"
		return res
	}
	rawKey, err := e.resolveKey(ctx, job.APIKeyID)
	if err != nil || strings.TrimSpace(rawKey) == "" {
		res.Outcome = AttemptFailedPermanent
		res.ErrMsg = ReasonAPIKeyUnavailable + ": cannot resolve api key for execution"
		return res
	}
	// 入库时 body 已被 replayBody 规整过一次（剥控制字段、强制 stream=false），
	// 这里原样送出：重试必须重放**同一份**请求体，否则「同一任务」会变语义。
	body := job.RequestBody
	if len(body) == 0 {
		res.Outcome = AttemptFailedClient
		res.ErrMsg = "empty request body"
		return res
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ciyuan-lane.internal/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		res.Outcome = AttemptFailedPermanent
		res.ErrMsg = "build replay request: " + err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)
	// UA 用可识别值：任务车道的上游日志里要能一眼看出「这是重放，不是客户端直连」。
	// 不存客户端原始 UA —— 它对计费与准入无作用，存下来只是多一份可泄漏的指纹。
	req.Header.Set("User-Agent", "ciyuan-task-lane/1")
	if ip := strings.TrimSpace(job.ClientIP); ip != "" {
		req.Header.Set("X-Forwarded-For", ip)
		req.RemoteAddr = ip + ":0"
	}
	// 血缘：middleware.ClientRequestID 会复用 ctx 里已有的值（不覆盖），
	// 因此这个 id 会一路带到 usage_logs.request_id。
	reqCtx := context.WithValue(req.Context(), ctxkey.ClientRequestID, res.RequestID)
	req = req.WithContext(reqCtx)

	rec := newBufferedResponse()
	e.engine.ServeHTTP(rec, req)

	res.HTTPStatus = rec.Status()
	res.Header = rec.header
	res.Body = rec.body.Bytes()
	res.Outcome = classifyAttempt(res.HTTPStatus)
	if res.Outcome != AttemptCompleted {
		res.ErrMsg = firstNonEmpty(extractErrorMessage(res.Body), fmt.Sprintf("upstream status %d", res.HTTPStatus))
	}
	return res
}

// extractErrorMessage 尽量取出可读错误信息，取不到就返回空（由调用方兜底）。
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if msg := strings.TrimSpace(gjson.GetBytes(body, "error.message").String()); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(gjson.GetBytes(body, "message").String()); msg != "" {
		return msg
	}
	if len(body) <= 200 {
		return strings.TrimSpace(string(body))
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncateJSON 把响应体裁成 result_body 能接受的大小；非 JSON 响应包成
// {"raw": "..."} 以保证列类型合法（不静默丢弃）。
func truncateJSON(body []byte) ([]byte, bool) {
	if len(body) == 0 || !json.Valid(body) {
		// 非 JSON 响应（极少见）包成字符串，保证 result_body 列类型合法。
		wrapped, _ := json.Marshal(map[string]string{"raw": string(body)})
		return truncateResult(wrapped)
	}
	return truncateResult(body)
}
