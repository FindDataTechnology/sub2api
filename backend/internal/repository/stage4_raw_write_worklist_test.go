package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 阶段四第②步的**作业单**：drop 明文列之后，哪些语句会炸、炸在哪里。
//
// 为什么要单独一条：ent 那边的依赖是**编译期**拦下的（schema 删字段 → 全站编译失败），
// 而裸 SQL 字符串没有任何编译期保护 —— 它们只在真正执行时才报
// `column "credentials" does not exist`。也就是说，如果只靠"编译过了就以为改完了"，
// 这三处会在 drop 之后的消息流里逐个暴露，而那时已经不可回退。
//
// 所以这里把三条语句按当前代码原样抄下来，在**已 drop 的库**上执行，
// 断言它们确实失败、且错误里点名了被 drop 的列。drop 那次 release 的验收标准就是：
// 本测试里的每一条都已被改造成不再引用明文列（届时本测试整体删除）。
func TestStage4_RawSQLWritesToPlaintextColumnsFailAfterDrop(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；阶段四作业单演练需要真 PostgreSQL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `
		DROP SCHEMA public CASCADE; CREATE SCHEMA public;
		CREATE TABLE accounts (
			id bigserial PRIMARY KEY,
			deleted_at timestamptz,
			platform text NOT NULL,
			type text NOT NULL,
			proxy_id bigint,
			updated_at timestamptz,
			credentials jsonb NOT NULL DEFAULT '{}'::jsonb,
			credentials_meta jsonb NOT NULL DEFAULT '{}'::jsonb,
			credentials_encrypted text,
			credentials_hash varchar(64),
			credentials_api_key_hash varchar(64)
		);
		CREATE TABLE scheduler_outbox (
			id bigserial PRIMARY KEY, event_type text, account_id bigint, group_id bigint, payload jsonb
		);
		INSERT INTO accounts (id, platform, type, credentials, credentials_hash)
		VALUES (1, 'grok', 'oauth', '{"a":1}'::jsonb, 'h');
	`)
	require.NoError(t, err)

	// 三条生产语句（只保留与明文列有关的最小骨架，其它列值不重要）
	type rawWrite struct {
		name string
		sql  string
		args []any
	}
	statements := []rawWrite{
		{
			// account_repo.go:1718 UpdateGrokOAuthCredentialsIfUnchanged
			name: "UpdateGrokOAuthCredentialsIfUnchanged",
			sql: `
			WITH updated AS (
			UPDATE accounts AS a
			SET credentials = $1::jsonb,
				credentials_meta = $2::jsonb,
				updated_at = NOW()
			WHERE a.id = 1 AND a.deleted_at IS NULL
			RETURNING a.id
			)
			INSERT INTO scheduler_outbox (event_type, account_id)
			SELECT 'account.changed', updated.id FROM updated`,
			args: []any{[]byte(`{"k":"v"}`), []byte(`{}`)},
		},
		{
			// account_repo.go:3228 BulkUpdate 的合并子句
			name: "BulkUpdate.merge",
			sql:  `UPDATE accounts SET credentials = COALESCE(credentials, '{}'::jsonb) || $1::jsonb WHERE id = 1`,
			args: []any{[]byte(`{"k":"v"}`)},
		},
		{
			// account_repo.go 的 UpdateCredentials：整份替换
			name: "UpdateCredentials",
			sql:  `UPDATE accounts SET credentials = $1::jsonb WHERE id = 1`,
			args: []any{[]byte(`{"k":"v"}`)},
		},
	}

	// 先在**没有 drop** 的库上跑一遍，证明语句本身是有效的（否则下面的失败可能只是语法错）
	for _, stmt := range statements {
		_, err := db.ExecContext(ctx, stmt.sql, stmt.args...)
		require.NoError(t, err, "%s 在 drop 之前必须能执行", stmt.name)
	}

	_, err = db.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN credentials`)
	require.NoError(t, err)

	for _, stmt := range statements {
		_, execErr := db.ExecContext(ctx, stmt.sql, stmt.args...)
		require.Error(t, execErr, "%s 在 drop 之后必须失败（这正是阶段四第②步要改掉的地方）", stmt.name)
		// 只断言"点名的列 + 不存在"两段，不锁整句措辞：
		// PG17 是 `column "credentials" of relation "accounts" does not exist`，
		// 老版本没有 `of relation ...` 这一段。锁整句会让升级 PG 时假红。
		require.Contains(t, execErr.Error(), `column "credentials"`,
			"%s 的失败原因必须点名明文列", stmt.name)
		require.Contains(t, execErr.Error(), "does not exist",
			"%s 的失败原因必须是列不存在，而不是别的错——否则说明这条语句已不依赖明文列", stmt.name)
	}
}
