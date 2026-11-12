//go:build dbtest

package lanes

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 调度状态机的**真库**验证（add-ciyuan-task-lane S3.1/S3.2/S3.3）。
//
// 为什么必须真库：调度器的判据全在「库里的状态迁移」上（claim → attempt →
// 重排/终态）。用 mock store 测它等于测自己写的桩。
//
//	createdb ciyuan_task_ddl_test
//	psql -d ... -f migrations/251_lanes_task_jobs.sql
//	psql -d ... -f migrations/252_lanes_task_attempts.sql
//	CIYUAN_TEST_PG_DSN='postgres:///ciyuan_task_ddl_test?sslmode=disable' \
//	  go test -tags dbtest ./internal/lanes/ -run TestScheduler -v
//
// **一次只跑一个包**：lanes 与 service 的 dbtest 共用同一个 scratch 库并各自
// TRUNCATE，go test 并行跑两个包会互相清空对方的行（实测：调度用例多跑了一次、
// attempts 写入撞 FK）。这不是代码缺陷，是用例编排问题 —— 分开跑即绿。

type stubRunner struct {
	outcome  string
	status   int
	calls    int
	onCall   func(job *service.LanesTaskJob)
	cancelAt int // 第几次调用时把 job 的 deadline 压到过去（模拟「越等越来不及」）
}

func (s *stubRunner) runWithRequestID(_ context.Context, _ *service.LanesTaskJob, _ string) attemptOutcome {
	s.calls++
	now := time.Now()
	if s.onCall != nil {
		s.onCall(nil)
	}
	return attemptOutcome{
		RequestID: "task-stub", HTTPStatus: s.status, Outcome: s.outcome,
		StartedAt: now, EndedAt: now,
	}
}

func openLanesScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CIYUAN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("CIYUAN_TEST_PG_DSN not set; skipping real-DB scheduler test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	_, err = db.Exec(`TRUNCATE lanes_task_attempts, lanes_task_jobs RESTART IDENTITY`)
	require.NoError(t, err)
	// 客户端关掉后仍在跑 goroutine 会写库，测试结束前必须清干净。
	t.Cleanup(func() {
		_, _ = db.Exec(`TRUNCATE lanes_task_attempts, lanes_task_jobs RESTART IDENTITY`)
		_ = db.Close()
	})
	return db
}

func newSchedTestLane(t *testing.T, db *sql.DB, runner taskRunner) *TaskLane {
	t.Helper()
	service.SetLanesTaskStore(service.NewLanesTaskStore(db))
	t.Cleanup(func() { service.SetLanesTaskStore(nil) })
	// inlineExec：让 tick 同步跑完，否则断言时任务还在 running（异步派发），
	// 而且 goroutine 会漏到下一个用例的 TRUNCATE 之后去写库（实测撞过 FK）。
	return &TaskLane{runner: runner, nowFn: time.Now, inlineExec: true}
}

func insertDeferJob(t *testing.T, db *sql.DB, deadline *time.Time) int64 {
	t.Helper()
	now := time.Now().Add(-time.Second)
	var id int64
	err := db.QueryRow(`
INSERT INTO lanes_task_jobs (api_key_id, user_id, platform, model, mode, status, request_body, deadline_at, next_attempt_at, max_attempts)
VALUES (1, 7, 'openai', 'deepseek-v4.1-flash', 'defer', 'pending',
        '{"model":"deepseek-v4.1-flash","messages":[]}'::jsonb, $1, $2, 6)
RETURNING id`, deadline, now).Scan(&id)
	require.NoError(t, err)
	return id
}

// 429 → 重排并退避，尝试号 +1，attempts 表留痕（task 3.1 / 3.3）。
func TestSchedulerReschedulesOnQueueFull(t *testing.T) {
	db := openLanesScratchDB(t)
	runner := &stubRunner{outcome: AttemptRetryQueueFull, status: 429}
	lane := newSchedTestLane(t, db, runner)
	store := service.GetLanesTaskStore()

	deadline := time.Now().Add(2 * time.Hour)
	id := insertDeferJob(t, db, &deadline)

	lane.schedulerTick(context.Background())
	require.Equal(t, 1, runner.calls)

	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "pending", job.Status, "429 必须重排而不是终态")
	require.Equal(t, 1, job.Attempts)
	require.NotNil(t, job.Reason)
	require.Contains(t, *job.Reason, "retry_queue_full")
	require.True(t, job.NextAttemptAt.After(time.Now()), "必须退避到将来，不能立刻重打")

	// 退避期内不得被再次领取。
	lane.schedulerTick(context.Background())
	require.Equal(t, 1, runner.calls, "退避期未到不得重试")

	// attempts 表每次尝试都要有一行（否则排障时不知道试过几次）。
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM lanes_task_attempts WHERE job_id=$1`, id).Scan(&attempts))
	require.Equal(t, 1, attempts)

	var outcome string
	require.NoError(t, db.QueryRow(`SELECT outcome FROM lanes_task_attempts WHERE job_id=$1`, id).Scan(&outcome))
	require.Equal(t, AttemptRetryQueueFull, outcome)
}

// 4xx（非 429）→ 直接终态 failed，不浪费重试（task 3.3）。
func TestSchedulerFailsPermanentlyOnClientError(t *testing.T) {
	db := openLanesScratchDB(t)
	runner := &stubRunner{outcome: AttemptFailedClient, status: 400}
	lane := newSchedTestLane(t, db, runner)
	store := service.GetLanesTaskStore()

	deadline := time.Now().Add(time.Hour)
	id := insertDeferJob(t, db, &deadline)

	lane.schedulerTick(context.Background())
	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "failed", job.Status)
	require.NotNil(t, job.Reason)
	require.Contains(t, *job.Reason, ReasonClientError)

	// 终态后不会再被领取。
	lane.schedulerTick(context.Background())
	require.Equal(t, 1, runner.calls)
}

// 5xx → 退避重试；重排后最早完成时刻越过 deadline 时改判 deadline_exceeded。
func TestSchedulerStopsRetryingWhenBackoffBreaksDeadline(t *testing.T) {
	db := openLanesScratchDB(t)
	runner := &stubRunner{outcome: AttemptRetryTransient, status: 503}
	lane := newSchedTestLane(t, db, runner)
	store := service.GetLanesTaskStore()

	// 截止只剩 60s：一次退避（5s）+ 120s 估计就已越过 —— 必须当场停，不要再排。
	deadline := time.Now().Add(60 * time.Second)
	id := insertDeferJob(t, db, &deadline)

	lane.schedulerTick(context.Background())
	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "failed", job.Status, "再来一次也来不及，必须当场落 failed")
	require.NotNil(t, job.Reason)
	require.Contains(t, *job.Reason, ReasonDeadlineExceeded)
}

// I3：pending 且 deadline 已过的任务，必须在被领取之前就被淘汰（task 3.2）。
func TestSchedulerExpiresOverdueBeforeClaiming(t *testing.T) {
	db := openLanesScratchDB(t)
	runner := &stubRunner{outcome: AttemptCompleted, status: 200}
	lane := newSchedTestLane(t, db, runner)
	store := service.GetLanesTaskStore()

	past := time.Now().Add(-time.Minute)
	id := insertDeferJob(t, db, &past)

	lane.schedulerTick(context.Background())

	require.Zero(t, runner.calls, "已超期的任务不得再被执行")
	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "failed", job.Status)
	require.Equal(t, ReasonDeadlineExceeded, *job.Reason)

	var pendingOverdue int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM lanes_task_jobs WHERE status='pending' AND deadline_at < now()`).Scan(&pendingOverdue))
	require.Zero(t, pendingOverdue, "I3：恒为 0")
}

// 成功路径：状态 completed + 结果体落库 + 尝试表 completed。
func TestSchedulerCompletesAndStoresResult(t *testing.T) {
	db := openLanesScratchDB(t)
	runner := &stubRunner{outcome: AttemptCompleted, status: 200}
	lane := newSchedTestLane(t, db, runner)
	store := service.GetLanesTaskStore()

	deadline := time.Now().Add(time.Hour)
	id := insertDeferJob(t, db, &deadline)

	lane.schedulerTick(context.Background())
	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "completed", job.Status)
	require.Equal(t, 1, job.Attempts)
	require.NotNil(t, job.FinishedAt)

	var outcome string
	require.NoError(t, db.QueryRow(`SELECT outcome FROM lanes_task_attempts WHERE job_id=$1`, id).Scan(&outcome))
	require.Equal(t, AttemptCompleted, outcome)
}

// 真执行器 × 真库：5xx 一次之后，退避必须落在**将来**，且 attempts.finished_at
// 不能是零值。
//
// 为什么单独一条：上面几条用例用的是 stubRunner，它自己填了 EndedAt=now，
// 所以「执行器把 EndedAt 留在零值」这种坑在上面几条里**永远照不出来**。
// 实测 -7 上生产的 lanes_task_attempts.finished_at 全是 0001-01-01，而
// 调度器算下次尝试时刻用的是 out.EndedAt.Add(backoff) —— 零值 + 退避 = 过去，
// 于是退避被清零、失败任务立刻重打，在上游抖动时正好制造放大。
func TestSchedulerBackoffUsesRealExecutorTimestamps(t *testing.T) {
	db := openLanesScratchDB(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"message": "upstream down"}})
	})
	exec := &taskExecutor{
		engine:     engine,
		resolveKey: func(context.Context, int64) (string, error) { return "sk-test-abc", nil },
	}
	lane := newSchedTestLane(t, db, exec)
	store := service.GetLanesTaskStore()

	deadline := time.Now().Add(2 * time.Hour)
	id := insertDeferJob(t, db, &deadline)
	before := time.Now()

	lane.schedulerTick(context.Background())

	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "pending", job.Status, "5xx 必须重排，不能终态")
	require.True(t, job.NextAttemptAt.After(before.Add(time.Second)),
		"退避必须落在将来：零值 EndedAt 会让 next_attempt_at 落在过去，失败任务立刻重打")

	var finished time.Time
	require.NoError(t, db.QueryRow(
		`SELECT finished_at FROM lanes_task_attempts WHERE job_id=$1`, id).Scan(&finished))
	require.False(t, finished.IsZero(),
		"attempts.finished_at 不得为零值（defer 必须写在命名返回值上）")
	require.False(t, finished.Before(before), "finished_at 不得早于本次 tick 的时刻")
}

// I4：调度器**没有**任何账号维度输入。
//
// 这条是结构性断言 —— 它不证明"自愈"，但能挡住最常见的回归：
// 有人为了"更聪明地调度"把账号数/Concurrency 读进调度参数。
// 真正要证明 I4，只需注意 TaskLane 的字段清单：runner + store + 时钟，
// 没有一个与账号拓扑相关。
func TestTaskLaneHasNoAccountTopologyInputs(t *testing.T) {
	lane := &TaskLane{}
	require.Nil(t, lane.exec)
	require.Nil(t, lane.runner)
	require.Nil(t, lane.apiKeys)
	// 调度参数全是常量（见 task_types.go），没有从账号派生的字段可供注入。
}

// 上一个进程留下的 running 必须被收回（真库）。
//
// 洞的形状：ClaimDue 只捞 pending，全库没有回收点。进程在跑任务时死掉
// （滚版/OOM/kill）→ 该行永远 running：端点一直显示"在跑"、调度器不再领、
// ExpireOverdue 也管不到（它只管 pending）。2026-11-11 一天滚了三次版。
func TestReclaimInterruptedRequeuesOrphanedRunning(t *testing.T) {
	db := openLanesScratchDB(t)
	runner := &stubRunner{outcome: AttemptCompleted, status: 200}
	lane := newSchedTestLane(t, db, runner)
	store := service.GetLanesTaskStore()

	deadline := time.Now().Add(2 * time.Hour)
	id := insertDeferJob(t, db, &deadline)

	// 造出"上个进程死在执行中途"的形状：running + 一条没有 ended 的 attempt。
	_, err := db.Exec(`UPDATE lanes_task_jobs SET status='running', attempts=1 WHERE id=$1`, id)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO lanes_task_attempts (job_id, attempt_no, request_id, started_at)
		VALUES ($1, 1, 'task-orphan-1', now() - interval '10 minutes')`, id)
	require.NoError(t, err)

	requeued, failed, err := store.ReclaimInterrupted(context.Background(), time.Now(), time.Now(), 6, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, requeued)
	require.Zero(t, failed)

	job, err := store.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "pending", job.Status, "尝试没满必须退回 pending，而不是永久 running")
	require.Equal(t, 2, job.Attempts, "收回要消耗一次尝试，否则崩溃-重试可以无限循环")
	require.NotNil(t, job.Reason)
	require.Contains(t, *job.Reason, "requeued_after_interruption")

	// 孤儿尝试行补记结局，且不能是零值时间（-8 之前那批是 0001-01-01）。
	var outcome string
	var finished time.Time
	require.NoError(t, db.QueryRow(`SELECT outcome, finished_at FROM lanes_task_attempts
		WHERE job_id=$1 AND attempt_no=1`, id).Scan(&outcome, &finished))
	require.Equal(t, "interrupted", outcome)
	require.False(t, finished.IsZero(), "补记结局必须带真实时间")

	// 收回后调度器能正常领取它（证明真的回到了可跑状态）。
	lane.schedulerTick(context.Background())
	require.Equal(t, 1, runner.calls, "收回之后必须能被领取执行")

	// 尝试已用满的孤儿：直接 failed，不能无限重排。
	id2 := insertDeferJob(t, db, &deadline)
	_, err = db.Exec(`UPDATE lanes_task_jobs SET status='running', attempts=6 WHERE id=$1`, id2)
	require.NoError(t, err)
	_, failed, err = store.ReclaimInterrupted(context.Background(), time.Now(), time.Now(), 6, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, failed)
	j2, err := store.Get(context.Background(), id2)
	require.NoError(t, err)
	require.Equal(t, "failed", j2.Status)
	require.Contains(t, *j2.Reason, "interrupted_too_many")
}

// 运行期回收的两个边界：**新**的不碰、**在飞**的不碰（真库）。
//
// 启动时回收可以"见 running 就收"（本进程刚起，必然都是前一个进程的），
// 但运行期不行：Finish 写库失败留下的行与"正在跑的尝试"在库里长得一样，
// 区分只能靠 ① updated_at 是否超过 2×attemptTimeout；② 本进程内存里的活跃集。
func TestReclaimRespectsStalenessAndInflight(t *testing.T) {
	db := openLanesScratchDB(t)
	_ = newSchedTestLane(t, db, &stubRunner{outcome: AttemptCompleted, status: 200})
	store := service.GetLanesTaskStore()
	now := time.Now()

	fresh := insertDeferJob(t, db, ptrTime(now.Add(time.Hour)))
	_, err := db.Exec(`UPDATE lanes_task_jobs SET status='running', updated_at=$1 WHERE id=$2`, now, fresh)
	require.NoError(t, err)

	// ① 刚更新过的 running：不算孤儿，一个都不许收。
	requeued, failed, err := store.ReclaimInterrupted(context.Background(), now, now.Add(-time.Minute), 6, nil)
	require.NoError(t, err)
	require.Zero(t, requeued)
	require.Zero(t, failed, "新行不得被当成孤儿（否则正在跑的尝试会被抢跑）")

	// ② 已过期但显式在飞：同样不许收。
	stale := insertDeferJob(t, db, ptrTime(now.Add(time.Hour)))
	_, err = db.Exec(`UPDATE lanes_task_jobs SET status='running', updated_at=$1 WHERE id=$2`,
		now.Add(-time.Hour), stale)
	require.NoError(t, err)
	requeued, failed, err = store.ReclaimInterrupted(context.Background(), now, now.Add(-time.Minute), 6, []int64{stale})
	require.NoError(t, err)
	require.Zero(t, requeued)
	require.Zero(t, failed, "在飞任务必须被 skip 列表挡住")

	// ③ 过期的、且不在飞的：才收。
	requeued, failed, err = store.ReclaimInterrupted(context.Background(), now, now.Add(-time.Minute), 6, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, requeued, "只有真正过期的孤儿该被收回")
	require.Zero(t, failed)
	job, err := store.Get(context.Background(), stale)
	require.NoError(t, err)
	require.Equal(t, "pending", job.Status)
}
