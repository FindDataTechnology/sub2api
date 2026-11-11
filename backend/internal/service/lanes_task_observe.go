package service

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// 任务车道的**观测量**（add-ciyuan-task-lane S3.4 / S4.1 / S1.2）。
//
// 纪律：这里的每一个数字都来自「已经发生的事实」（usage_logs / lanes_task_*），
// **没有一个来自配置**（账号数量、account.Concurrency、槽位数都不读）。
// 这就是 I4：运维增删账号、改并发、某账号冷却，本文件不需要任何改动。

// CountPending 返回全库仍在排队的任务数（队首等待估算的分子）。
func (s *LanesTaskStore) CountPending(ctx context.Context) (int64, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM lanes_task_jobs WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// AvgCostPerRequest 返回某模型近 since 时间内**实扣单次均值**（USD）。
//
// 为什么用它当估价基数，而不是按价卡重算：车道拿不到 BillingService，
// 按价卡再算一遍等于造第二套计价；用实扣均值则口径唯一、来源可查。
// 无样本时返回 nil（**不返回 0**：0 会被读成「免费」）。
func (s *LanesTaskStore) AvgCostPerRequest(ctx context.Context, model string, since time.Time) (*float64, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var avg sql.NullFloat64
	var samples int64
	err := s.db.QueryRowContext(ctx, `
SELECT avg(actual_cost), count(*) FROM usage_logs
WHERE model = $1 AND created_at >= $2 AND actual_cost > 0`, model, since).Scan(&avg, &samples)
	if err != nil {
		return nil, err
	}
	if !avg.Valid || samples == 0 {
		return nil, nil
	}
	v := avg.Float64
	return &v, nil
}

// UsageCostByRequestID 按 request_id 取该次请求的实扣（USD）。
// 无行或金额为 0 时返回 nil —— 「没查到」与「扣了 0 元」不可混。
func (s *LanesTaskStore) UsageCostByRequestID(ctx context.Context, requestID string) (*float64, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var cost sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
SELECT actual_cost FROM usage_logs
WHERE request_id = $1 ORDER BY id DESC LIMIT 1`, "client:"+requestID).Scan(&cost)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !cost.Valid {
		return nil, nil
	}
	v := cost.Float64
	return &v, nil
}

// ClassThroughput 返回某模型近 since 内的观测可持续速率（requests/second）
// 与样本量（活跃分钟数）。
//
// 口径 = 分钟级请求数的 p95 / 60。用 p95 而不是均值，是因为我们要的是
// 「这类模型跑得动多快」而不是「平均多闲」；均值会被夜间空窗拉低，
// 进而低估容量、把可行任务误判为不可行。
//
// 复用 usage_logs（全量流量）而不是 lanes_task_attempts（只有车道自己的量），
// 否则新上线时样本必然不足，capacity 段永远 null。
func (s *LanesTaskStore) ClassThroughput(ctx context.Context, model string, since time.Time) (rps float64, activeMinutes int64, err error) {
	if err := s.ready(); err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, `
WITH per_minute AS (
	SELECT date_trunc('minute', created_at) AS m, count(*) AS cnt
	FROM usage_logs
	WHERE model = $1 AND created_at >= $2
	GROUP BY 1
)
SELECT COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY cnt), 0) / 60.0, count(*)
FROM per_minute`, model, since).Scan(&rps, &activeMinutes)
	if err != nil {
		return 0, 0, err
	}
	return rps, activeMinutes, nil
}

// ErrorRatioForModel 返回某模型近 since 内的失败占比（ops_error_logs 侧）。
//
// 分母含成功：只用 ops_error_logs 会恒得 100%，只用 usage_logs 会恒得 0%
// —— 本仓「读不到当成不存在」的同族陷阱，两边必须同时取。
func (s *LanesTaskStore) ErrorRatioForModel(ctx context.Context, model string, since time.Time) (*float64, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	var failures, successes int64
	if err := s.db.QueryRowContext(ctx, `
SELECT count(*) FROM ops_error_logs WHERE model = $1 AND created_at >= $2`, model, since).Scan(&failures); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT count(*) FROM usage_logs WHERE model = $1 AND created_at >= $2`, model, since).Scan(&successes); err != nil {
		return nil, err
	}
	total := failures + successes
	if total == 0 {
		return nil, nil
	}
	v := float64(failures) / float64(total)
	return &v, nil
}

// TopModelForKey 返回该 key 近 since 内用得最多的模型（按请求数）。
// 无样本时返回空串（调用方退回能力快照的可用模型列表）。
func (s *LanesTaskStore) TopModelForKey(ctx context.Context, apiKeyID int64, since time.Duration) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	var model sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT model FROM usage_logs
WHERE api_key_id = $1 AND created_at >= $2
GROUP BY model ORDER BY count(*) DESC LIMIT 1`, apiKeyID, time.Now().Add(-since)).Scan(&model)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !model.Valid {
		return "", nil
	}
	return model.String, nil
}
