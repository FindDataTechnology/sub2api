//go:build dbtest

package service

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 任务车道存储层的**真库**验证（add-ciyuan-task-lane task 2.1）。
//
// 为什么不进 CI：CI 没有 PG。SQL 是这一层最容易错的部分（部分唯一索引的
// ON CONFLICT 推断、CTE + RETURNING、行值比较），而 Go 编译对它们一无所知。
// 所以给它一个显式入口：
//
//	createdb ciyuan_task_ddl_test
//	psql -d ciyuan_task_ddl_test -f migrations/251_lanes_task_jobs.sql
//	psql -d ciyuan_task_ddl_test -f migrations/252_lanes_task_attempts.sql
//	CIYUAN_TEST_PG_DSN='postgres:///ciyuan_task_ddl_test?sslmode=disable' \
//	  go test -tags dbtest ./internal/service/ -run TestLanesTaskStore -v
//
// 这个 tag 的存在本身就是一条纪律：**没跑过真库就不许说 DDL/SQL 验过了**。

func openScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CIYUAN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("CIYUAN_TEST_PG_DSN not set; skipping real-DB store test")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.Exec(`TRUNCATE lanes_task_attempts, lanes_task_jobs RESTART IDENTITY`)
		_ = db.Close()
	})
	require.NoError(t, db.Ping())
	return db
}

func newJobFor(apiKeyID int64, externalID *string, mode string) *LanesTaskJob {
	now := time.Now()
	return &LanesTaskJob{
		ExternalID:    externalID,
		APIKeyID:      apiKeyID,
		UserID:        7,
		Platform:      "openai",
		Model:         "deepseek-v4.1-flash",
		Mode:          mode,
		Status:        "pending",
		RequestBody:   []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":false}`),
		NextAttemptAt: now,
		MaxAttempts:   6,
		ClientIP:      "10.0.0.1",
		CreatedAt:     now,
	}
}

func TestLanesTaskStore_CreateAndIdempotency(t *testing.T) {
	store := NewLanesTaskStore(openScratchDB(t))
	ctx := context.Background()

	ext := "ext-1"
	id, dup, err := store.Create(ctx, newJobFor(1, &ext, "defer"))
	require.NoError(t, err)
	require.False(t, dup)
	require.NotZero(t, id)

	// 同 key 同 external_id：必须判重并回既有 id（调用方能继续轮询）。
	id2, dup2, err := store.Create(ctx, newJobFor(1, &ext, "defer"))
	require.NoError(t, err)
	require.True(t, dup2)
	require.Equal(t, id, id2)

	// 不同 key 同 external_id：不冲突。
	id3, dup3, err := store.Create(ctx, newJobFor(2, &ext, "defer"))
	require.NoError(t, err)
	require.False(t, dup3)
	require.NotEqual(t, id, id3)

	// external_id 为 NULL：部分唯一索引必须放行（不传幂等键 = 每次都是新任务）。
	for i := 0; i < 3; i++ {
		_, d, err := store.Create(ctx, newJobFor(3, nil, "now"))
		require.NoError(t, err)
		require.False(t, d, "NULL external_id 不得参与去重")
	}
}

func TestLanesTaskStore_ClaimDueRespectsDeferAndDeadline(t *testing.T) {
	store := NewLanesTaskStore(openScratchDB(t))
	ctx := context.Background()
	now := time.Now()

	future := now.Add(time.Hour)
	soon := now.Add(-time.Minute)
	lateDeadline := now.Add(30 * time.Minute)
	earlyDeadline := now.Add(10 * time.Minute)

	ready := newJobFor(10, nil, "defer")
	ready.DeadlineAt = &lateDeadline
	// newJobFor 里的 NextAttemptAt 是「调用时刻」，比本测试的 now 更晚，
	// 必须显式拉回过去才是「已就绪」——否则测的是自己的时钟偏序。
	ready.NextAttemptAt = soon

	notYet := newJobFor(10, nil, "defer")
	notYet.DeferredUntil = &future
	notYet.NextAttemptAt = future

	dueSoon := newJobFor(10, nil, "defer")
	dueSoon.DeadlineAt = &earlyDeadline
	dueSoon.NextAttemptAt = soon

	for _, j := range []*LanesTaskJob{ready, notYet, dueSoon} {
		_, _, err := store.Create(ctx, j)
		require.NoError(t, err)
	}

	got, err := store.ClaimDue(ctx, now, 5)
	require.NoError(t, err)
	require.Len(t, got, 2, "推迟期未到的那条不得被领取")

	// EDF：deadline 早的先领。
	require.NotNil(t, got[0].DeadlineAt)
	require.Equal(t, earlyDeadline.UTC().Truncate(time.Second), got[0].DeadlineAt.UTC().Truncate(time.Second))

	// 领到即 running，第二次领取返回空（不会重复跑）。
	again, err := store.ClaimDue(ctx, now, 5)
	require.NoError(t, err)
	require.Empty(t, again, "已被领取的任务不得被再次领取")
}

func TestLanesTaskStore_ExpireOverdueEnforcesI3(t *testing.T) {
	store := NewLanesTaskStore(openScratchDB(t))
	ctx := context.Background()
	now := time.Now()

	past := now.Add(-time.Minute)
	j := newJobFor(20, nil, "defer")
	j.DeadlineAt = &past
	id, _, err := store.Create(ctx, j)
	require.NoError(t, err)

	n, err := store.ExpireOverdue(ctx, now)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	job, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "failed", job.Status)
	require.NotNil(t, job.Reason)
	require.Equal(t, "deadline_exceeded", *job.Reason)
	require.NotNil(t, job.FinishedAt)

	// I3：pending 且 deadline<now 的行数必须恒为 0。
	var remaining int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM lanes_task_jobs WHERE status='pending' AND deadline_at < $1`, now).Scan(&remaining))
	require.Zero(t, remaining)
}

func TestLanesTaskStore_AttemptsAndCostLineage(t *testing.T) {
	store := NewLanesTaskStore(openScratchDB(t))
	ctx := context.Background()

	id, _, err := store.Create(ctx, newJobFor(30, nil, "defer"))
	require.NoError(t, err)

	// 每次尝试的 request_id 必须不同，且 (job_id, attempt_no) 唯一。
	require.NoError(t, store.BeginAttempt(ctx, id, 1, "task-1-1", time.Now()))
	require.NoError(t, store.BeginAttempt(ctx, id, 2, "task-1-2", time.Now()))
	require.NoError(t, store.BeginAttempt(ctx, id, 2, "task-1-2", time.Now()), "同尝试号重写必须幂等")

	cost := 0.0123
	require.NoError(t, store.FinishAttempt(ctx, id, 1, 429, "retry_queue_full", "queue full", nil, time.Now()))
	require.NoError(t, store.FinishAttempt(ctx, id, 2, 200, "completed", "", &cost, time.Now()))

	var n int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM lanes_task_attempts WHERE job_id=$1`, id).Scan(&n))
	require.Equal(t, 2, n)

	completed, err := store.CompletedSince(ctx, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, completed, "只有 outcome=completed 计入完成速率")
}

func TestLanesTaskStore_RescheduleAndPendingAhead(t *testing.T) {
	store := NewLanesTaskStore(openScratchDB(t))
	ctx := context.Background()
	now := time.Now()

	var ids []int64
	for i := 0; i < 3; i++ {
		d := now.Add(time.Duration(10+i) * time.Minute)
		j := newJobFor(40, nil, "defer")
		j.DeadlineAt = &d
		id, _, err := store.Create(ctx, j)
		require.NoError(t, err)
		ids = append(ids, id)
	}

	ahead, err := store.PendingAhead(ctx, ids[0])
	require.NoError(t, err)
	require.Zero(t, ahead, "最早截止的任务前面应无人排队")

	ahead, err = store.PendingAhead(ctx, ids[2])
	require.NoError(t, err)
	require.EqualValues(t, 2, ahead)

	require.NoError(t, store.Reschedule(ctx, ids[0], 1, now.Add(time.Minute), "retry_transient: boom", nil))
	job, err := store.Get(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, "pending", job.Status)
	require.Equal(t, 1, job.Attempts)
	require.NotNil(t, job.Reason)

	active, err := store.CountActiveForKey(ctx, 40)
	require.NoError(t, err)
	require.EqualValues(t, 3, active)
}
