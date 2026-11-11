package lanes

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 调度器（add-ciyuan-task-lane S3）。
//
// 它只做四件事，且每一件都对应一条不变量：
//
//	1. 先把越过 deadline 的 pending 落 failed   → I3（没有任务静默超期）
//	2. 原子领取到点任务（EDF）                   → 不饿死早截止的任务
//	3. 重放执行（走既有网关）                    → 不建第二套账（D5）
//	4. 按结论分流：重排 / 退避 / 终态            → I2（每个被接受的任务都有可行排程）
//
// **它不读任何账号状态**：不读账号数量、不读 account.Concurrency、不读槽位。
// 上游是否吃纳由网关自己判断（429 就是它的回答），我们只把 429 当成
// 「现在别发这么快」并退避。因此运维增删账号、改并发、某账号冷却，
// 本文件零改动即自愈 —— 这就是 I4。
//
// 单副本前提：进程内 inflight 计数即真实并发。扩副本时应改为共享计数，
// 届时还要给退避加抖动（现为确定性退避，便于复现）。

func (t *TaskLane) startScheduler() {
	t.startOnce.Do(func() {
		if t.exec == nil || t.apiKeys == nil {
			return
		}
		go t.schedulerLoop()
	})
}

func (t *TaskLane) schedulerLoop() {
	ctx := context.Background()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		t.schedulerTick(ctx)
	}
}

// schedulerTick 是调度器的一次心跳（导出给测试直接驱动，避免等 ticker）。
func (t *TaskLane) schedulerTick(ctx context.Context) {
	store := service.GetLanesTaskStore()
	if store == nil {
		return
	}
	now := t.now()

	// 1) 先淘汰超期：必须在领取之前，否则「已超期但还排着」的任务会被领走再失败。
	if n, err := store.ExpireOverdue(ctx, now); err != nil {
		logger.LegacyPrintf("lanes.tasks", "expire overdue failed: err=%v", err)
	} else if n > 0 {
		logger.LegacyPrintf("lanes.tasks", "expired %d task(s) past deadline", n)
	}

	// 2) 领取。名额 = 并发上限 − 在飞数；没有名额就不查库（不做无效扫描）。
	free := int64(taskWorkerConcurrency) - t.inflight.Load()
	if free <= 0 {
		return
	}
	jobs, err := store.ClaimDue(ctx, now, int(free))
	if err != nil {
		logger.LegacyPrintf("lanes.tasks", "claim due failed: err=%v", err)
		return
	}
	for _, job := range jobs {
		t.inflight.Add(1)
		go func(j *service.LanesTaskJob) {
			defer t.inflight.Add(-1)
			t.execute(ctx, j)
		}(job)
	}
}

// execute 执行一个已领取的任务并落结论。
func (t *TaskLane) execute(ctx context.Context, job *service.LanesTaskJob) {
	store := service.GetLanesTaskStore()
	if store == nil {
		return
	}
	// ClaimDue 已把状态置 running，但 attempts 还没加：本次尝试号 = attempts+1。
	attemptNo := job.Attempts + 1
	requestID := taskRequestID(job.ID, attemptNo)

	if err := store.BeginAttempt(ctx, job.ID, attemptNo, requestID, t.now()); err != nil {
		logger.LegacyPrintf("lanes.tasks", "begin attempt failed: job=%d err=%v", job.ID, err)
	}

	out := t.exec.runWithRequestID(ctx, job, requestID)
	cost, _ := store.UsageCostByRequestID(ctx, requestID)

	_ = store.FinishAttempt(ctx, job.ID, attemptNo, out.HTTPStatus, out.Outcome, out.ErrMsg, cost, out.EndedAt)

	switch {
	case out.Outcome == AttemptCompleted:
		body, truncated := truncateJSON(out.Body)
		status := out.HTTPStatus
		if err := store.Finish(ctx, job.ID, TaskStatusCompleted, &status, body, truncated, cost, ""); err != nil {
			logger.LegacyPrintf("lanes.tasks", "finish completed failed: job=%d err=%v", job.ID, err)
		}
		return

	case outcomeRetryable(out.Outcome):
		nextAttemptAt := out.EndedAt.Add(backoffDelay(attemptNo))
		// 截止硬约束：重排后的**最早**完成时刻若已越过 deadline，就不要再排 ——
		// 直接 failed(deadline_exceeded)，理由可读。
		if job.DeadlineAt != nil && nextAttemptAt.Add(taskEstDuration).After(*job.DeadlineAt) {
			t.failDeadline(ctx, store, job, out)
			return
		}
		if attemptNo >= job.MaxAttempts {
			t.failPermanent(ctx, store, job, out)
			return
		}
		reason := out.Outcome + ": " + trimOrEmpty(out.ErrMsg)
		if err := store.Reschedule(ctx, job.ID, attemptNo, nextAttemptAt, reason, &requestID); err != nil {
			logger.LegacyPrintf("lanes.tasks", "reschedule failed: job=%d err=%v", job.ID, err)
		}
		return

	default:
		t.failPermanent(ctx, store, job, out)
	}
}

func (t *TaskLane) failDeadline(ctx context.Context, store *service.LanesTaskStore, job *service.LanesTaskJob, out attemptOutcome) {
	reason := ReasonDeadlineExceeded + ": " + out.Outcome
	if out.ErrMsg != "" {
		reason += " (" + out.ErrMsg + ")"
	}
	_ = store.Finish(ctx, job.ID, TaskStatusFailed, nil, nil, false, nil, reason)
}

func (t *TaskLane) failPermanent(ctx context.Context, store *service.LanesTaskStore, job *service.LanesTaskJob, out attemptOutcome) {
	status := out.HTTPStatus
	reason := ReasonClientError
	if out.Outcome == AttemptFailedPermanent {
		reason = ReasonUpstreamError
	}
	if out.Outcome == AttemptFailedClient && status >= 500 {
		reason = ReasonUpstreamError
	}
	if out.ErrMsg != "" {
		reason += ": " + out.ErrMsg
	}
	_ = store.Finish(ctx, job.ID, TaskStatusFailed, &status, nil, false, nil, reason)
}
