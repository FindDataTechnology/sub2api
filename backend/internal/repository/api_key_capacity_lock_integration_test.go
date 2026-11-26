//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// key 容量分配的行锁（add-key-capacity-contract 2b.3）必须**真在真实数据库上演一遍**。
//
// 为什么单测不够：单测用内存桩实现 WithUserRowLock，桩里"锁"是本地 mutex，
// 永远不会暴露"锁内某条写路径绕开事务"这种缺陷。2026-11-26 生产上撞到的正是这类：
// 外层事务对 users 行持 FOR UPDATE，而 INSERT api_keys 走 r.client 另开连接，
// 外键检查要取父行 FOR KEY SHARE ⇒ 被自己的外层事务挡住 ⇒ 应用层自死锁。
// 该缺陷在任何桩测试里都不可见，只有真 Postgres + 真 ent 事务能照出来。
func TestWithUserRowLock_CreateInsideLockDoesNotDeadlock(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	userRepo := newUserRepositoryWithSQL(client, integrationDB)
	// cryptor 传 nil 是合法值（只写 key_hash，密文列留 NULL）——本测试不关心加密。
	keyRepo := newAPIKeyRepositoryWithSQLCrypto(client, integrationDB, nil)

	suffix := time.Now().UnixNano()
	user, err := client.User.Create().
		SetEmail(fmt.Sprintf("lock-test-%d@example.com", suffix)).
		SetPasswordHash("x").
		SetRole("user").
		SetStatus(service.StatusActive).
		SetConcurrency(5).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM api_keys WHERE user_id = $1", user.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})

	// 超时必须短于测试框架的 30s：自死锁时这里会以 ctx 超时失败，而不是把 CI 挂住。
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- userRepo.WithUserRowLock(lockCtx, user.ID, func(txCtx context.Context) error {
			// 锁内读池容量（走事务）
			if _, gerr := userRepo.GetByID(txCtx, user.ID); gerr != nil {
				return gerr
			}
			// 锁内写 api_keys —— 就是这一步在 r.client 版本下会自死锁
			return keyRepo.Create(txCtx, &service.APIKey{
				UserID:             user.ID,
				Key:                fmt.Sprintf("sk-lock-%d", suffix),
				Name:               "lock-probe",
				Status:             service.StatusActive,
				ConcurrencyRequest: 1,
				ConcurrencyLimit:   1,
			})
		})
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "锁内创建不得失败（自死锁会表现为超时或 deadlock detected）")
	case <-time.After(12 * time.Second):
		t.Fatal("锁内创建超时 12s —— 疑似应用层自死锁：外层 FOR UPDATE 挡住自己的 INSERT 外键检查")
	}

	// 落库确认
	var n int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT count(*) FROM api_keys WHERE user_id = $1 AND deleted_at IS NULL", user.ID).Scan(&n))
	require.Equal(t, 1, n, "锁内创建应真的写入一行")
}

// 并发分配不得超卖：池=5，20 个并发请求各要 1，恰好 5 个成功。
func TestWithUserRowLock_ConcurrentAllocationDoesNotOversell(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	userRepo := newUserRepositoryWithSQL(client, integrationDB)
	// cryptor 传 nil 是合法值（只写 key_hash，密文列留 NULL）——本测试不关心加密。
	keyRepo := newAPIKeyRepositoryWithSQLCrypto(client, integrationDB, nil)

	suffix := time.Now().UnixNano()
	user, err := client.User.Create().
		SetEmail(fmt.Sprintf("oversell-%d@example.com", suffix)).
		SetPasswordHash("x").
		SetRole("user").
		SetStatus(service.StatusActive).
		SetConcurrency(5).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM api_keys WHERE user_id = $1", user.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})

	const wanted = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0

	for i := 0; i < wanted; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			err := userRepo.WithUserRowLock(ctx, user.ID, func(txCtx context.Context) error {
				// 锁内复核 Σrequest ≤ 池
				keys, _, lerr := keyRepo.ListByUserID(txCtx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 1000}, service.APIKeyListFilters{})
				if lerr != nil {
					return lerr
				}
				used := 0
				for j := range keys {
					if keys[j].Status == service.StatusActive {
						used += keys[j].ConcurrencyRequest
					}
				}
				if used+1 > 5 {
					return fmt.Errorf("would exceed pool: used=%d", used)
				}
				return keyRepo.Create(txCtx, &service.APIKey{
					UserID:             user.ID,
					Key:                fmt.Sprintf("sk-over-%d-%d", suffix, i),
					Name:               fmt.Sprintf("probe-%d", i),
					Status:             service.StatusActive,
					ConcurrencyRequest: 1,
					ConcurrencyLimit:   1,
				})
			})
			if err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	require.Equal(t, 5, granted, "池=5 时 20 个并发分配必须恰好放行 5 个")
	var sum int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT COALESCE(sum(concurrency_request),0) FROM api_keys WHERE user_id=$1 AND status='active' AND deleted_at IS NULL",
		user.ID).Scan(&sum))
	require.Equal(t, 5, sum, "库内 Σrequest 必须等于池容量，不得超卖")
}
