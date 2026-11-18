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
		if t.runner == nil {
			return
		}
		// 先收回上一个进程留下的 running，再开始领任务。
		//
		// 顺序有讲究：ClaimDue 只捞 pending，孤儿不先收回就会一直"在路上"；
		// 放在 goroutine 之前同步做，第一次 tick 就看到干净状态，失败也可读。
		if store := service.GetLanesTaskStore(); store != nil {
			// 启动时 olderThan=now：本进程刚起，任何 running 都必然是前一个进程留下的。
			now := t.now()
			requeued, failed, err := store.ReclaimInterrupted(context.Background(), now, now, taskMaxAttempts, nil)
			if err != nil {
				logger.LegacyPrintf("lanes.tasks", "reclaim interrupted failed: err=%v", err)
			} else if requeued > 0 || failed > 0 {
				logger.LegacyPrintf("lanes.tasks",
					"reclaimed %d interrupted task(s): requeued=%d failed=%d (上一个进程在跑它们时退出了)",
					requeued+failed, requeued, failed)
			}
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
	// 每 15 拍（约 30s）做一次"运行期孤儿"回收。
	//
	// 为什么不能只在启动时做：Finish/FinishAttempt 写库失败（DB 抖动、连接断开）时
	// 错误只被记日志，行会停在 running —— 进程不重启就永远回不来。
	// 判据用「updated_at 早于 2×attemptTimeout」：领取时写一次时间、执行期间不刷新，
	// 所以这个界必须显著大于单次尝试上限，否则会误判正在跑的尝试。
	t.ticks++
	if t.ticks%15 == 1 {
		t.reclaimStaleRunning(ctx)
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
		if t.inlineExec {
			// 测试路径：同步跑完，调用方 tick 返回即可断言库里的状态。
			t.execute(ctx, job)
			continue
		}
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
	// 登记"本进程正在跑它"，供运行期回收排除（见 reclaimStaleRunning）。
	t.activeTasks.Store(job.ID, struct{}{})
	defer t.activeTasks.Delete(job.ID)

	// ClaimDue 已把状态置 running，但 attempts 还没加：本次尝试号 = attempts+1。
	attemptNo := job.Attempts + 1
	requestID := taskRequestID(job.ID, attemptNo)

	if err := store.BeginAttempt(ctx, job.ID, attemptNo, requestID, t.now()); err != nil {
		logger.LegacyPrintf("lanes.tasks", "begin attempt failed: job=%d err=%v", job.ID, err)
	}

	out := t.runner.runWithRequestID(ctx, job, requestID)
	// defer 路径没有人在等，可以多等几拍让用量行落库。
	cost := waitActualCost(ctx, store, requestID, 6)

	_ = store.FinishAttempt(ctx, job.ID, attemptNo, out.HTTPStatus, out.Outcome, out.ErrMsg, cost, out.EndedAt)

	// 终态后统一补血缘与实扣（defer 路径提交时还没有 request_id）。
	defer func() {
		_ = store.SetLineage(ctx, job.ID, requestID, cost)
	}()

	switch {
	case out.Outcome == AttemptCompleted:
		body, truncated := truncateJSON(out.Body)
		status := out.HTTPStatus
		if err := store.Finish(ctx, job.ID, attemptNo, TaskStatusCompleted, &status, body, truncated, cost, ""); err != nil {
			logger.LegacyPrintf("lanes.tasks", "finish completed failed: job=%d err=%v", job.ID, err)
		}
		return

	case outcomeRetryable(out.Outcome):
		nextAttemptAt := out.EndedAt.Add(backoffDelay(attemptNo))
		// 截止硬约束：重排后的**最早**完成时刻若已越过 deadline，就不要再排 ——
		// 直接 failed(deadline_exceeded)，理由可读。
		if job.DeadlineAt != nil && nextAttemptAt.Add(taskEstDuration).After(*job.DeadlineAt) {
			t.failDeadline(ctx, store, job, out, attemptNo)
			return
		}
		if attemptNo >= job.MaxAttempts {
			t.failPermanent(ctx, store, job, out, attemptNo)
			return
		}
		reason := out.Outcome + ": " + trimOrEmpty(out.ErrMsg)
		if err := store.Reschedule(ctx, job.ID, attemptNo, nextAttemptAt, reason, &requestID); err != nil {
			logger.LegacyPrintf("lanes.tasks", "reschedule failed: job=%d err=%v", job.ID, err)
		}
		return

	default:
		t.failPermanent(ctx, store, job, out, attemptNo)
	}
}

// waitActualCost 等实扣到账。
//
// 用量行由 UsageRecordWorkerPool **异步**写入，紧跟重放去查通常取不到 ——
// 实测就是这里让 actual_cost_usd 落成空。故做有限次短退避重试；
// 到点仍取不到就返回 nil 并留空（端点显示 null，**不编造**）。
func waitActualCost(ctx context.Context, store *service.LanesTaskStore, requestID string, attempts int) *float64 {
	if store == nil || requestID == "" {
		return nil
	}
	for i := 0; i < attempts; i++ {
		if cost, err := store.UsageCostByRequestID(ctx, requestID); err == nil && cost != nil {
			return cost
		}
		if i < attempts-1 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	return nil
}

// reclaimStaleRunning 收回运行期孤儿：状态停在 running 且超过 2×attemptTimeout 没动过。
func (t *TaskLane) reclaimStaleRunning(ctx context.Context) {
	store := service.GetLanesTaskStore()
	if store == nil {
		return
	}
	// 本进程内存里正在跑的任务显式排除：它们的 updated_at 也是领取时刻，
	// 长时间执行时看起来同样"没动过"，只靠时间戳会抢跑自己。
	skip := make([]int64, 0, 8)
	t.activeTasks.Range(func(k, _ any) bool {
		if id, ok := k.(int64); ok {
			skip = append(skip, id)
		}
		return true
	})
	now := t.now()
	requeued, failed, err := store.ReclaimInterrupted(ctx, now, now.Add(-2*taskAttemptTimeout), taskMaxAttempts, skip)
	if err != nil {
		logger.LegacyPrintf("lanes.tasks", "reclaim stale running failed: err=%v", err)
		return
	}
	if requeued > 0 || failed > 0 {
		logger.LegacyPrintf("lanes.tasks",
			"reclaimed %d stale task(s): requeued=%d failed=%d skipped_inflight=%d",
			requeued+failed, requeued, failed, len(skip))
	}
}

func (t *TaskLane) failDeadline(ctx context.Context, store *service.LanesTaskStore, job *service.LanesTaskJob, out attemptOutcome, attemptNo int) {
	reason := ReasonDeadlineExceeded + ": " + out.Outcome
	if out.ErrMsg != "" {
		reason += " (" + out.ErrMsg + ")"
	}
	_ = store.Finish(ctx, job.ID, attemptNo, TaskStatusFailed, nil, nil, false, nil, reason)
}

func (t *TaskLane) failPermanent(ctx context.Context, store *service.LanesTaskStore, job *service.LanesTaskJob, out attemptOutcome, attemptNo int) {
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
	_ = store.Finish(ctx, job.ID, attemptNo, TaskStatusFailed, &status, nil, false, nil, reason)
}
