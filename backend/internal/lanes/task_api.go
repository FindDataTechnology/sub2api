package lanes

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 任务车道端点（add-ciyuan-task-lane D1/D2）。
//
//	POST /v1/ciyuan/tasks        提交任务（mode=now 直通，mode=defer 入队）
//	GET  /v1/ciyuan/tasks/{id}   查询任务
//
// 与 /v1/chat/completions 的关系：**不改它**。车道只是把同一份请求体重放回去，
// 因此旧调用方零改动（G4）。

// TaskLane 是任务车道的运行时。
type TaskLane struct {
	// exec 保留具体类型：装配期要往里塞 engine。
	exec *taskExecutor
	// runner 是调度与端点实际依赖的接口（生产即 exec，测试可换成桩）。
	runner  taskRunner
	apiKeys *service.APIKeyService

	inflight  atomic.Int64
	startOnce sync.Once
	nowFn     func() time.Time // 测试可注入
	// inlineExec 让调度器在 tick 内同步执行（仅测试用）。
	// 生产必须异步：一个慢上游不能把心跳卡住，那会让 deadline 淘汰停摆。
	inlineExec bool
}

func newTaskLane(exec *taskExecutor, apiKeys *service.APIKeyService) *TaskLane {
	return &TaskLane{exec: exec, runner: exec, apiKeys: apiKeys, nowFn: time.Now}
}

func (t *TaskLane) now() time.Time {
	if t == nil || t.nowFn == nil {
		return time.Now()
	}
	return t.nowFn()
}

// ---- 请求/响应形状 ----

type taskEstimateJSON struct {
	BaseAvgCostUSD *float64 `json:"base_avg_cost_usd_estimate"`
	AtPeakUSD      *float64 `json:"at_peak_cost_usd_estimate"`
	AtOffpeakUSD   *float64 `json:"at_offpeak_cost_usd_estimate"`
	Source         string   `json:"source"`
}

type taskDeferJSON struct {
	Deferred      bool       `json:"deferred"`
	DeferredUntil *time.Time `json:"deferred_until,omitempty"`
	SavingRatio   float64    `json:"saving_ratio"`
	Reason        string     `json:"reason"`
}

type taskSubmitResponse struct {
	TaskID    int64  `json:"task_id"`
	Status    string `json:"status"`
	Mode      string `json:"mode"`
	Duplicate bool   `json:"duplicate"`

	// mode=now 成功时直接带结果（等价于一次同步调用）。
	Result json.RawMessage `json:"result,omitempty"`

	Deadline      *time.Time        `json:"deadline,omitempty"`
	DeferredUntil *time.Time        `json:"deferred_until,omitempty"`
	NextAttemptAt *time.Time        `json:"next_attempt_at,omitempty"`
	Estimate      *taskEstimateJSON `json:"estimate,omitempty"`
	Defer         *taskDeferJSON    `json:"defer_decision,omitempty"`
}

type taskView struct {
	TaskID      int64  `json:"task_id"`
	Status      string `json:"status"`
	Mode        string `json:"mode"`
	Model       string `json:"model"`
	Platform    string `json:"platform"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`

	ExternalID    *string    `json:"external_id,omitempty"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	DeferredUntil *time.Time `json:"deferred_until,omitempty"`
	DeferReason   *string    `json:"defer_reason,omitempty"`

	Result          json.RawMessage `json:"result,omitempty"`
	ResultTruncated bool            `json:"result_truncated"`
	Reason          *string         `json:"reason,omitempty"`
	HTTPStatus      *int            `json:"result_http_status,omitempty"`

	EstimatedCostUSD *float64 `json:"estimated_cost_usd_estimate,omitempty"`
	ActualCostUSD    *float64 `json:"actual_cost_usd,omitempty"`
	PeakMultiplier   *float64 `json:"peak_multiplier_at_submit,omitempty"`
	LastRequestID    *string  `json:"last_request_id,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// ---- 提交 ----

func (t *TaskLane) Submit(c *gin.Context) {
	store := service.GetLanesTaskStore()
	if store == nil {
		taskUnavailable(c)
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		taskUnauthorized(c)
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(body) == 0 {
		taskBadRequest(c, "invalid_request_error", "request body is required")
		return
	}
	replay, meta, err := replayBody(body)
	if err != nil {
		taskBadRequest(c, "invalid_request_error", err.Error())
		return
	}

	mode := strings.TrimSpace(gjson.GetBytes(body, "mode").String())
	if mode == "" {
		mode = TaskModeNow
	}
	if mode != TaskModeNow && mode != TaskModeDefer {
		taskBadRequest(c, "invalid_request_error", "mode must be 'now' or 'defer'")
		return
	}
	externalID := strings.TrimSpace(gjson.GetBytes(body, "external_id").String())
	deadline, derr := parseDeadline(body, t.now())
	if derr != nil {
		taskBadRequest(c, "invalid_request_error", derr.Error())
		return
	}
	minSaving := gjson.GetBytes(body, "min_saving_ratio").Float()
	if minSaving <= 0 {
		minSaving = 0.2 // 见 decideDefer 注释：S0 实测省 47%，门槛定在 20%
	}

	// 幂等：同 key 下 external_id 已存在 → 直接回既有任务，不重复执行。
	if externalID != "" {
		if existing, gerr := store.GetByExternal(c.Request.Context(), apiKey.ID, externalID); gerr == nil && existing != nil {
			c.JSON(http.StatusOK, taskSubmitResponse{
				TaskID: existing.ID, Status: existing.Status, Mode: existing.Mode, Duplicate: true,
				Result: json.RawMessage(existing.ResultBody), Deadline: existing.DeadlineAt,
				DeferredUntil: existing.DeferredUntil,
			})
			return
		}
	}

	group := apiKey.Group
	snap := priceSnapshotAt(group, t.now())
	platform := ""
	if group != nil {
		platform = group.Platform
	}

	job := &service.LanesTaskJob{
		APIKeyID:       apiKey.ID,
		UserID:         apiKey.UserID,
		Platform:       platform,
		Model:          meta.Model,
		Mode:           mode,
		RequestBody:    replay,
		DeadlineAt:     deadline,
		MaxAttempts:    taskMaxAttempts,
		ClientIP:       c.ClientIP(),
		PeakMultiplier: &snap.PeakMultiplier,
		CreatedAt:      t.now(),
	}
	if group != nil {
		gid := group.ID
		job.GroupID = &gid
	}
	if externalID != "" {
		job.ExternalID = &externalID
	}

	base, _ := store.AvgCostPerRequest(c.Request.Context(), meta.Model, t.now().Add(-7*24*time.Hour))
	estimate := buildCostEstimate(base, snap)

	if mode == TaskModeNow {
		t.submitNow(c, store, job, estimate)
		return
	}
	t.submitDefer(c, store, job, estimate, snap, minSaving)
}

// submitNow：立刻重放一次。成功才落行（容量不足返回 429 且不建行，见 task 2.2）。
func (t *TaskLane) submitNow(c *gin.Context, store *service.LanesTaskStore, job *service.LanesTaskJob, estimate costEstimate) {
	requestID := taskNowRequestID()
	out := t.runner.runWithRequestID(c.Request.Context(), job, requestID)
	status := out.HTTPStatus

	resultBody, truncated := truncateJSON(out.Body)
	// mode=now 有人等，只等 3 拍（≤300ms）；取不到就留空，绝不编造。
	cost := waitActualCost(c.Request.Context(), store, requestID, 3)

	switch out.Outcome {
	case AttemptCompleted:
		job.Status = TaskStatusCompleted
		job.Attempts = 1
		job.ResultStatus = &status
		job.ResultBody = resultBody
		job.ResultTruncated = truncated
		job.LastRequestID = &out.RequestID
		job.ActualCostUSD = cost
		job.FinishedAt = ptrTime(out.EndedAt)
		job.EstimatedCostUSD = estimate.AtPeakUSD
		id, dup, err := store.Create(c.Request.Context(), job)
		if err != nil {
			taskStoreError(c, err)
			return
		}
		c.JSON(http.StatusOK, taskSubmitResponse{
			TaskID: id, Status: TaskStatusCompleted, Mode: TaskModeNow, Duplicate: dup,
			Result: json.RawMessage(resultBody), Estimate: toEstimateJSON(estimate),
		})
	default:
		// 未成功：**不建行**（调用方拿到与今天同步路径一致的错误面）。
		copyUpstreamHeaders(c, out)
		if status == 0 {
			status = http.StatusBadGateway
		}
		c.Data(status, "application/json", out.Body)
	}
}

// submitDefer：可行性判定 → 价格决策 → 入队。
func (t *TaskLane) submitDefer(c *gin.Context, store *service.LanesTaskStore, job *service.LanesTaskJob, estimate costEstimate, snap priceSnapshot, minSaving float64) {
	ctx := c.Request.Context()
	now := t.now()

	// 队列上界：单 key 未完成任务数不得无限增长（Risks「队满风暴转移」）。
	active, err := store.CountActiveForKey(ctx, job.APIKeyID)
	if err != nil {
		taskStoreError(c, err)
		return
	}
	if active >= taskQueueLimitPerKey {
		taskReject(c, http.StatusTooManyRequests, ReasonQueueLimit,
			"too many unfinished tasks for this key; poll existing tasks or raise deadline awareness")
		return
	}

	// 可行性：收下就必须跑得完（task 3.4）。不可行当场拒，不落库。
	pending, err := store.CountPending(ctx)
	if err != nil {
		taskStoreError(c, err)
		return
	}
	completed, err := store.CompletedSince(ctx, now.Add(-taskThroughputWindow))
	if err != nil {
		taskStoreError(c, err)
		return
	}
	var observedRPS float64
	if completed > 0 {
		observedRPS = float64(completed) / taskThroughputWindow.Seconds()
	}
	feasible, queueWait, available := feasibleByDeadline(now, job.DeadlineAt, pending, observedRPS)
	if !feasible {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": gin.H{
				"type":    "invalid_request_error",
				"code":    ReasonInfeasibleDeadline,
				"message": "task cannot be guaranteed to finish before deadline; loosen the deadline or use mode=now",
			},
			"queue_wait_estimate_seconds": int(queueWait.Seconds()),
			"estimated_duration_seconds":  int(taskEstDuration.Seconds()),
			"available_seconds":           int(available.Seconds()),
			"observed_rps":                observedRPS,
		})
		return
	}

	decision := decideDefer(now, job.DeadlineAt, snap, minSaving)
	job.Status = TaskStatusPending
	job.NextAttemptAt = now
	job.DeferredUntil = decision.DeferredUntil
	if decision.DeferredUntil != nil {
		job.NextAttemptAt = *decision.DeferredUntil
	}
	reason := decision.Reason
	job.DeferReason = &reason
	job.EstimatedCostUSD = estimate.AtPeakUSD

	id, dup, err := store.Create(ctx, job)
	if err != nil {
		taskStoreError(c, err)
		return
	}
	t.startScheduler()

	resp := taskSubmitResponse{
		TaskID: id, Status: TaskStatusPending, Mode: TaskModeDefer, Duplicate: dup,
		Deadline: job.DeadlineAt, DeferredUntil: decision.DeferredUntil,
		NextAttemptAt: &job.NextAttemptAt, Estimate: toEstimateJSON(estimate),
		Defer: &taskDeferJSON{
			Deferred: decision.Defer, DeferredUntil: decision.DeferredUntil,
			SavingRatio: decision.SavingRatio, Reason: decision.Reason,
		},
	}
	c.JSON(http.StatusAccepted, resp)
}

// ---- 查询 ----

func (t *TaskLane) Get(c *gin.Context) {
	store := service.GetLanesTaskStore()
	if store == nil {
		taskUnavailable(c)
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok {
		taskUnauthorized(c)
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		taskBadRequest(c, "invalid_request_error", "invalid task id")
		return
	}
	job, err := store.Get(c.Request.Context(), id)
	if err != nil {
		taskStoreError(c, err)
		return
	}
	// 越权与不存在返回同一个 404：不泄漏「某个 id 存在但不属于你」。
	if job == nil || job.APIKeyID != apiKey.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"type": "not_found_error", "message": "task not found"}})
		return
	}
	c.JSON(http.StatusOK, toTaskView(job))
}

func toTaskView(job *service.LanesTaskJob) taskView {
	v := taskView{
		TaskID: job.ID, Status: job.Status, Mode: job.Mode, Model: job.Model, Platform: job.Platform,
		Attempts: job.Attempts, MaxAttempts: job.MaxAttempts,
		ExternalID: job.ExternalID, Deadline: job.DeadlineAt, DeferredUntil: job.DeferredUntil,
		DeferReason: job.DeferReason, ResultTruncated: job.ResultTruncated, Reason: job.Reason,
		HTTPStatus: job.ResultStatus, EstimatedCostUSD: job.EstimatedCostUSD,
		ActualCostUSD: job.ActualCostUSD, PeakMultiplier: job.PeakMultiplier,
		LastRequestID: job.LastRequestID,
		CreatedAt:     job.CreatedAt, UpdatedAt: job.UpdatedAt, FinishedAt: job.FinishedAt,
	}
	if len(job.ResultBody) > 0 {
		v.Result = json.RawMessage(job.ResultBody)
	}
	return v
}

func toEstimateJSON(e costEstimate) *taskEstimateJSON {
	if e.BaseAvgCostUSD == nil && e.AtPeakUSD == nil && e.AtOffpeakUSD == nil {
		return &taskEstimateJSON{Source: e.Source}
	}
	return &taskEstimateJSON{
		BaseAvgCostUSD: e.BaseAvgCostUSD, AtPeakUSD: e.AtPeakUSD,
		AtOffpeakUSD: e.AtOffpeakUSD, Source: e.Source,
	}
}

func parseDeadline(body []byte, now time.Time) (*time.Time, error) {
	if raw := strings.TrimSpace(gjson.GetBytes(body, "deadline").String()); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, errDeadlineFormat
		}
		if !ts.After(now) {
			return nil, errDeadlinePast
		}
		return &ts, nil
	}
	if secs := gjson.GetBytes(body, "deadline_seconds").Int(); secs > 0 {
		ts := now.Add(time.Duration(secs) * time.Second)
		return &ts, nil
	}
	return nil, nil
}

func copyUpstreamHeaders(c *gin.Context, out attemptOutcome) {
	if v := out.Header.Get("Retry-After"); v != "" {
		c.Header("Retry-After", v)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
