package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// BulkUpdate 的凭据是 **SQL 侧合并**（`credentials = COALESCE(credentials,'{}') || $n::jsonb`），
// Go 端拿不到合并后的完整文档，所以派生列算不出真值。
//
// 刻意的处理是：把派生列显式置回"未派生"状态（密文与两个指纹 NULL、meta 置空对象），
// 让读路径回落明文列。**留着过期的派生值比置空危险**——那会让阶段二按密文读到一份
// 与明文不一致的旧凭据。
//
// 这条测试守的就是"置空而不是留着"这个决定。若将来有人图省事把这段删掉，
// 全量测试仍会全绿（没有任何别的地方断言它），只有这条会红。
func TestBulkUpdate_ClearsDerivedCredentialColumnsOnMerge(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	exec.queryRows = emptyCredentialQueryRows(t)
	_, err := repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{
		Credentials: map[string]any{"api_key": "sk-rotated"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)

	query := normalizeSQLWhitespace(exec.execQueries[0])

	// 前提：合并本身仍是 SQL 侧做的
	require.Contains(t, query, "credentials = COALESCE(credentials, '{}'::jsonb) ||")

	// 合并前必须先把派生列置为"未派生"，保证期间不会留下与明文不一致的过期值
	require.Contains(t, query, "credentials_hash = NULL")
	require.Contains(t, query, "credentials_api_key_hash = NULL")
	require.Contains(t, query, "credentials_meta = '{}'::jsonb")

	// 明文列 MUST 没被改成"整份替换"——那会丢掉未在 updates 里出现的子键
	require.NotContains(t, query, "credentials = $")

	// 2.4：合并语句之后 MUST 在同一事务里回读明文并重算派生列。
	// 只置空不回填等于"每次批量编辑都把密文抹掉"，阶段四之后就读不到凭据了。
	require.GreaterOrEqual(t, len(exec.queryQueries), 1, "合并后必须回读明文")
	require.Contains(t, normalizeSQLWhitespace(exec.queryQueries[0]), "SELECT id, credentials")
}

// 不传 Credentials 时不该无端清空派生列——否则任何一次 status/priority 批量更新
// 都会把所有行的密文抹掉，双写就白做了。
func TestBulkUpdate_DoesNotClearDerivedColumnsWhenCredentialsUnspecified(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	priority := 5
	_, err := repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{
		Priority: &priority,
	})
	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)

	query := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, query, "priority = $")
	require.NotContains(t, query, "credentials_encrypted = NULL")
	require.NotContains(t, query, "credentials_hash = NULL")
}
