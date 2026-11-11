package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 任务车道的持久化层（add-ciyuan-task-lane S2/S3）。
//
// 归属：`internal/service` 新文件（与 key_capacity_gate.go / key_stability_*.go 同例），
// 只增不改上游既有文件。装配点在 cmd/server/wire_gen.go 的既有 `db` 变量旁，
// 经包级 setter 注入（与 service.SetOpenAIGatewayService 同一惯例）。
//
// 三条硬约束（都有测试或索引兜底）：
//
//	1. 幂等键 external_id 在同 api_key_id 下唯一（部分唯一索引，NULL 放行）。
//	2. request_id 必须按尝试号唯一 —— usage_logs 上 (request_id, api_key_id) 有唯一索引，
//	   重试复用同一个 id 会让第二次的用量行静默写不进去（任务成功、账单缺失）。
//	3. pending 且 deadline 已过的行数必须恒为 0 (I3)：调度器每轮先淘汰再领取。

// ErrLanesTaskStoreUnavailable 表示任务车道未装配（未注入 DB）。
// 端点据此返回 503 而不是假装成功 —— 「没跑」和「跑了没走到」必须可分。
var ErrLanesTaskStoreUnavailable = errors.New("lanes task store is not configured")

// LanesTaskJob 是一行任务。
type LanesTaskJob struct {
	ID         int64
	ExternalID *string
	APIKeyID   int64
	UserID     int64
	GroupID    *int64
	Platform   string
	Model      string
	ClientIP   string // 提交时的客户端 IP，重放时携带（保持 IP 白名单语义）
	Mode       string // now | defer
	Status     string // pending | running | completed | failed

	// RequestBody 是客户端原始请求体（不含认证）。重放时原样送出。
	RequestBody []byte

	DeadlineAt    *time.Time
	DeferredUntil *time.Time
	NextAttemptAt time.Time
	Attempts      int
	MaxAttempts   int

	// 峰谷快照与推迟决策原因：两者都必须可读，否则「为什么没等」无从解释。
	PeakMultiplier   *float64
	DeferReason      *string
	EstimatedCostUSD *float64
	ActualCostUSD    *float64

	ResultStatus    *int
	ResultBody      []byte
	ResultTruncated bool
	Reason          *string
	LastRequestID   *string

	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

// LanesTaskAttempt 是一次尝试。outcome 为空表示进行中。
type LanesTaskAttempt struct {
	JobID        int64
	AttemptNo    int
	RequestID    string
	StartedAt    time.Time
	FinishedAt   *time.Time
	HTTPStatus   *int
	Outcome      *string
	ErrorMessage *string
	CostUSD      *float64
}

const lanesTaskColumns = `
	id, external_id, api_key_id, user_id, group_id, platform, model, client_ip, mode, status,
	request_body, deadline_at, deferred_until, next_attempt_at, attempts, max_attempts,
	peak_multiplier, defer_reason, estimated_cost_usd, actual_cost_usd,
	result_status, result_body, result_truncated, reason, last_request_id,
	created_at, updated_at, finished_at`

// lanesTaskColumnsQualified 是同一列清单的别名版本，供 UPDATE … RETURNING 使用
// （子查询与目标表同表，列名必须带别名消歧）。
const lanesTaskColumnsQualified = `
	j.id, j.external_id, j.api_key_id, j.user_id, j.group_id, j.platform, j.model, j.client_ip, j.mode, j.status,
	j.request_body, j.deadline_at, j.deferred_until, j.next_attempt_at, j.attempts, j.max_attempts,
	j.peak_multiplier, j.defer_reason, j.estimated_cost_usd, j.actual_cost_usd,
	j.result_status, j.result_body, j.result_truncated, j.reason, j.last_request_id,
	j.created_at, j.updated_at, j.finished_at`

// LanesTaskStore 是任务车道的存储句柄。
type LanesTaskStore struct {
	db *sql.DB
}

// NewLanesTaskStore 构造存储句柄。db 为 nil 时所有方法返回 ErrLanesTaskStoreUnavailable。
func NewLanesTaskStore(db *sql.DB) *LanesTaskStore {
	return &LanesTaskStore{db: db}
}

var lanesTaskStore *LanesTaskStore

// SetLanesTaskStore 由装配层（cmd/server/wire_gen.go）在启动时注入一次。
func SetLanesTaskStore(s *LanesTaskStore) { lanesTaskStore = s }

// GetLanesTaskStore 返回已装配的存储句柄；未装配时返回 nil（调用方据此 503）。
func GetLanesTaskStore() *LanesTaskStore { return lanesTaskStore }

func (s *LanesTaskStore) ready() error {
	if s == nil || s.db == nil {
		return ErrLanesTaskStoreUnavailable
	}
	return nil
}

func scanLanesTaskJob(sc interface{ Scan(...any) error }) (*LanesTaskJob, error) {
	var j LanesTaskJob
	err := sc.Scan(
		&j.ID, &j.ExternalID, &j.APIKeyID, &j.UserID, &j.GroupID, &j.Platform, &j.Model, &j.ClientIP,
		&j.Mode, &j.Status, &j.RequestBody, &j.DeadlineAt, &j.DeferredUntil, &j.NextAttemptAt,
		&j.Attempts, &j.MaxAttempts, &j.PeakMultiplier, &j.DeferReason,
		&j.EstimatedCostUSD, &j.ActualCostUSD,
		&j.ResultStatus, &j.ResultBody, &j.ResultTruncated, &j.Reason, &j.LastRequestID,
		&j.CreatedAt, &j.UpdatedAt, &j.FinishedAt,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// Create 插入一行任务。若 (api_key_id, external_id) 已存在则返回 duplicate=true 与既有 id。
//
// 幂等判定的口径：只有 external_id 非 NULL 才去重 —— 不传幂等键的调用方
// 明确表示「这次提交就是一次新任务」。
func (s *LanesTaskStore) Create(ctx context.Context, j *LanesTaskJob) (id int64, duplicate bool, err error) {
	if err := s.ready(); err != nil {
		return 0, false, err
	}
	body, err := json.Marshal(json.RawMessage(j.RequestBody))
	if err != nil {
		return 0, false, fmt.Errorf("marshal request body: %w", err)
	}
	var resultBody any
	if len(j.ResultBody) > 0 {
		resultBody = json.RawMessage(j.ResultBody)
	}
	row := s.db.QueryRowContext(ctx, `
INSERT INTO lanes_task_jobs (
	external_id, api_key_id, user_id, group_id, platform, model, client_ip, mode, status,
	request_body, deadline_at, deferred_until, next_attempt_at, attempts, max_attempts,
	peak_multiplier, defer_reason, estimated_cost_usd, result_status, result_body,
	result_truncated, reason, created_at, updated_at, finished_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20::jsonb,$21,$22,$23,$23,$24)
ON CONFLICT (api_key_id, external_id) WHERE external_id IS NOT NULL DO NOTHING
RETURNING id`,
		j.ExternalID, j.APIKeyID, j.UserID, j.GroupID, j.Platform, j.Model, j.ClientIP, j.Mode, j.Status,
		string(body), j.DeadlineAt, j.DeferredUntil, j.NextAttemptAt, j.Attempts, j.MaxAttempts,
		j.PeakMultiplier, j.DeferReason, j.EstimatedCostUSD, j.ResultStatus, resultBody,
		j.ResultTruncated, j.Reason, j.CreatedAt, j.FinishedAt,
	)
	if err := row.Scan(&id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		// 冲突：回查既有行，把 duplicate 与既有 id 一起交回（调用方要能继续轮询）。
		if j.ExternalID != nil {
			existing, gerr := s.GetByExternal(ctx, j.APIKeyID, *j.ExternalID)
			if gerr != nil {
				return 0, false, gerr
			}
			return existing.ID, true, nil
		}
		return 0, true, nil
	}
	return id, false, nil
}

// Get 按 id 取任务。
func (s *LanesTaskStore) Get(ctx context.Context, id int64) (*LanesTaskJob, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	job, err := scanLanesTaskJob(s.db.QueryRowContext(ctx,
		`SELECT `+lanesTaskColumns+` FROM lanes_task_jobs WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// GetByExternal 按幂等键取任务。
func (s *LanesTaskStore) GetByExternal(ctx context.Context, apiKeyID int64, externalID string) (*LanesTaskJob, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	job, err := scanLanesTaskJob(s.db.QueryRowContext(ctx,
		`SELECT `+lanesTaskColumns+` FROM lanes_task_jobs
		 WHERE api_key_id = $1 AND external_id = $2`, apiKeyID, externalID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// ClaimDue 原子领取到点可跑的任务：pending → running。
//
// 用 FOR UPDATE SKIP LOCKED，故即使将来扩副本也不会两条进程跑同一个任务
// （v1 生产单副本，此处是给扩副本留的正确语义，不是当前必需）。
// 排序 = EDF（deadline 早的先跑），无 deadline 的排最后。
func (s *LanesTaskStore) ClaimDue(ctx context.Context, now time.Time, limit int) ([]*LanesTaskJob, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 4
	}
	rows, err := s.db.QueryContext(ctx, `
WITH ready AS (
	SELECT id FROM lanes_task_jobs
	WHERE status = 'pending'
	  AND next_attempt_at <= $1
	  AND (deferred_until IS NULL OR deferred_until <= $1)
	ORDER BY deadline_at IS NULL, deadline_at, next_attempt_at, id
	LIMIT $2
	FOR UPDATE SKIP LOCKED
)
UPDATE lanes_task_jobs j
SET status = 'running', updated_at = now()
FROM ready
WHERE j.id = ready.id
RETURNING `+lanesTaskColumnsQualified+``, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LanesTaskJob
	for rows.Next() {
		job, serr := scanLanesTaskJob(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// BeginAttempt 写一条进行中的尝试行。request_id 必须按尝试号唯一（见文件头约束 2）。
func (s *LanesTaskStore) BeginAttempt(ctx context.Context, jobID int64, attemptNo int, requestID string, startedAt time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO lanes_task_attempts (job_id, attempt_no, request_id, started_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (job_id, attempt_no) DO UPDATE
SET request_id = EXCLUDED.request_id, started_at = EXCLUDED.started_at`,
		jobID, attemptNo, requestID, startedAt)
	return err
}

// FinishAttempt 回填一次尝试的结果。
func (s *LanesTaskStore) FinishAttempt(ctx context.Context, jobID int64, attemptNo int, status int, outcome string, errMsg string, cost *float64, finishedAt time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	var emsg any
	if strings.TrimSpace(errMsg) != "" {
		emsg = errMsg
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE lanes_task_attempts
SET finished_at = $3, http_status = $4, outcome = $5, error_message = $6, cost_usd = $7
WHERE job_id = $1 AND attempt_no = $2
`, jobID, attemptNo, finishedAt, status, outcome, emsg, cost)
	return err
}

// Reschedule 把任务放回 pending 并推进 attempts / next_attempt_at。
//
// retryRequestID 为空时保留原值（用于「本轮完全没发起」的推迟，没产生尝试号）。
func (s *LanesTaskStore) Reschedule(ctx context.Context, jobID int64, attempts int, nextAttemptAt time.Time, reason string, retryRequestID *string) error {
	if err := s.ready(); err != nil {
		return err
	}
	var reasonArg any
	if strings.TrimSpace(reason) != "" {
		reasonArg = reason
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE lanes_task_jobs
SET status = 'pending', attempts = $2, next_attempt_at = $3, reason = $4,
    last_request_id = COALESCE($5, last_request_id), updated_at = now()
WHERE id = $1`, jobID, attempts, nextAttemptAt, reasonArg, retryRequestID)
	return err
}

// SetDeferredUntil 只推进推迟时刻（不消耗尝试号，不改变状态）。
func (s *LanesTaskStore) SetDeferredUntil(ctx context.Context, jobID int64, at time.Time, reason string) error {
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE lanes_task_jobs
SET deferred_until = $2, next_attempt_at = $2, defer_reason = $3, updated_at = now()
WHERE id = $1`, jobID, at, reason)
	return err
}

// Finish 落终态。终态只有 completed / failed 两种，且必须带可读 reason（失败时）。
func (s *LanesTaskStore) Finish(ctx context.Context, jobID int64, status string, resultStatus *int, resultBody []byte, truncated bool, cost *float64, reason string) error {
	if err := s.ready(); err != nil {
		return err
	}
	var body any
	if len(resultBody) > 0 {
		body = json.RawMessage(resultBody)
	}
	var reasonArg any
	if strings.TrimSpace(reason) != "" {
		reasonArg = reason
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE lanes_task_jobs
SET status = $2, result_status = $3, result_body = $4::jsonb, result_truncated = $5,
    actual_cost_usd = COALESCE($6, actual_cost_usd), reason = $7,
    finished_at = now(), updated_at = now()
WHERE id = $1`, jobID, status, resultStatus, body, truncated, cost, reasonArg)
	return err
}

// ExpireOverdue 把 pending 且 deadline 已过的任务落 failed(deadline_exceeded)。
//
// 这是 I3 的执法点：调用方看到的每个「活着」的任务都确实还来得及。
// 返回被淘汰的行数（有数才说明这条路径真的在跑，而不是「没跑」）。
func (s *LanesTaskStore) ExpireOverdue(ctx context.Context, now time.Time) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE lanes_task_jobs
SET status = 'failed', reason = 'deadline_exceeded', finished_at = now(), updated_at = now()
WHERE status = 'pending' AND deadline_at IS NOT NULL AND deadline_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountActiveForKey 返回该 key 名下仍在排队的任务数（队列上界判定用）。
func (s *LanesTaskStore) CountActiveForKey(ctx context.Context, apiKeyID int64) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `
SELECT count(*) FROM lanes_task_jobs
WHERE api_key_id = $1 AND status IN ('pending', 'running')`, apiKeyID).Scan(&n)
	return n, err
}

// PendingAhead 返回排在它前面（EDF 更早）的 pending 任务数，用于等待时间估算。
func (s *LanesTaskStore) PendingAhead(ctx context.Context, id int64) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `
WITH me AS (
	SELECT deadline_at, next_attempt_at, id FROM lanes_task_jobs WHERE id = $1
)
SELECT count(*) FROM lanes_task_jobs j, me
WHERE j.status = 'pending' AND j.id <> me.id
  AND (j.deadline_at IS NULL, j.deadline_at, j.next_attempt_at, j.id)
    < (me.deadline_at IS NULL, me.deadline_at, me.next_attempt_at, me.id)`, id).Scan(&n)
	return n, err
}

// CompletedSince 返回 since 之后完成的尝试数 —— 队列等待估算的唯一容量来源。
//
// 刻意**不**读 account.Concurrency / 账号数量：账号会变（增删/换号/改并发/冷却），
// 以账号为基准的判据会随拓扑漂移而失真（design D5 / I4）。
func (s *LanesTaskStore) CompletedSince(ctx context.Context, since time.Time) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `
SELECT count(*) FROM lanes_task_attempts
WHERE outcome = 'completed' AND finished_at >= $1`, since).Scan(&n)
	return n, err
}
