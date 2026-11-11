package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 阶段四第②步的**作业单**：drop 明文列之后，哪些裸 SQL 会炸、炸在哪里。
//
// 为什么要单独一条：ent 那边的依赖是**编译期**拦下的（schema 删字段 → 全站编译失败），
// 而裸 SQL 字符串没有任何编译期保护 —— 它们只在真正执行时才报
// `column "credentials" does not exist`。也就是说，如果只靠"编译过了就以为改完了"，
// 这些语句会在 drop 之后的消息流里逐个暴露，而那时已经不可回退。
//
// 覆盖面：**写**（三条 SET/合并）与**读**（合并后事务内回读明文重算派生列）。
// 早先只登记了写，读那一条是后来扫裸列引用才发现的 —— 一类语句只登记一半，
// 和"守卫只认一种写法"是同一个毛病。
//
// 做法：把每条语句按当前代码原样抄下来，先在**未 drop** 的库上跑通（证明语句本身有效，
// 排除语法错冒充依赖缺失），再 drop 明文列后逐条断言失败且点名被 drop 的列。
// drop 那次 release 的验收标准：本测试里的每一条都已被改造成不再引用明文列，然后整体删除。
func TestStage4_RawSQLOnPlaintextColumnsFailsAfterDrop(t *testing.T) {
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

	type rawStatement struct {
		name string
		sql  string
		args []any
	}
	statements := []rawStatement{
		{
			// 读：account_credentials_crypto.go:322 deriveCredentialColumnsAfterMerge
			// BulkUpdate 在 SQL 侧合并凭据后，同事务回读明文重算派生列。
			name: "deriveCredentialColumnsAfterMerge.read",
			sql:  `SELECT id, credentials FROM accounts WHERE id = ANY($1) AND deleted_at IS NULL`,
			args: []any{pq.Array([]int64{1})},
		},
		{
			// 写：account_repo.go:1718 UpdateGrokOAuthCredentialsIfUnchanged
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
			// 写：account_repo.go:3228 BulkUpdate 的合并子句
			name: "BulkUpdate.merge",
			sql:  `UPDATE accounts SET credentials = COALESCE(credentials, '{}'::jsonb) || $1::jsonb WHERE id = 1`,
			args: []any{[]byte(`{"k":"v"}`)},
		},
		{
			// 写：UpdateCredentials 的整份替换
			name: "UpdateCredentials",
			sql:  `UPDATE accounts SET credentials = $1::jsonb WHERE id = 1`,
			args: []any{[]byte(`{"k":"v"}`)},
		},
	}

	// 读与写用同一个探针：Query 既能跑 SELECT 也能跑 UPDATE（不取结果即可）。
	probe := func(stmt rawStatement) error {
		rows, qErr := db.QueryContext(ctx, stmt.sql, stmt.args...)
		if qErr != nil {
			return qErr
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
		}
		return rows.Err()
	}

	// ── drop 之前：每条都必须能跑通（证明语句有效） ──
	for _, stmt := range statements {
		require.NoError(t, probe(stmt), "%s 在 drop 之前必须能执行", stmt.name)
	}

	_, err = db.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN credentials`)
	require.NoError(t, err)

	// ── drop 之后：每条都必须失败，且错误点名被 drop 的列 ──
	for _, stmt := range statements {
		execErr := probe(stmt)
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
