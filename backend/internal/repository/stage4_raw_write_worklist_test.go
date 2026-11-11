package repository

import (
	"context"
	"database/sql"
	"os"
	"strings"
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
		CREATE TABLE api_keys (
			id bigserial PRIMARY KEY,
			key varchar(128) NOT NULL DEFAULT '',
			key_hash varchar(64),
			key_encrypted text,
			deleted_at timestamptz,
			updated_at timestamptz
		);
		INSERT INTO accounts (id, platform, type, credentials, credentials_hash)
		VALUES (1, 'grok', 'oauth', '{"a":1}'::jsonb, 'h');
		INSERT INTO api_keys (id, key, key_hash) VALUES (1, 'sk-worklist', 'kh');
	`)
	require.NoError(t, err)

	type rawStatement struct {
		name   string
		sql    string
		args   []any
		column string // 这条语句依赖的明文列（accounts=credentials / api_keys=key）
		// drift 是这条语句在**源码里**必须仍然存在的片段（比较时去掉所有空白）。
		// 为什么要有它：作业单是从源码抄下来的，而源码会改 ——
		// 2026-11-11 实测就抓到一次：`UpdateGrokOAuthCredentialsIfUnchanged` 已经改成
		// 写 credentials_meta=$8 / credentials_encrypted=$9 / WHERE credentials_hash=$12，
		// 作业单里还是旧的 `$2` 两参数版本。抄件陈旧之后，这条测试仍然绿，
		// 但它验的已经不是生产里跑的那条语句了。
		drift struct {
			file      string
			mustMatch string
		}
	}
	statements := []rawStatement{
		{
			// 读：account_credentials_crypto.go deriveCredentialColumnsAfterMerge
			// BulkUpdate 在 SQL 侧合并凭据后，同事务回读明文重算派生列。
			name:   "deriveCredentialColumnsAfterMerge.read",
			column: "credentials",
			sql:    `SELECT id, credentials FROM accounts WHERE id = ANY($1) AND deleted_at IS NULL`,
			args:   []any{pq.Array([]int64{1})},
			drift: struct{ file, mustMatch string }{
				file:      "account_credentials_crypto.go",
				mustMatch: "SELECT id, credentials FROM accounts WHERE id = ANY($1) AND deleted_at IS NULL",
			},
		},
		{
			// 写：account_repo.go UpdateGrokOAuthCredentialsIfUnchanged（整份替换）
			//
			// 这里把生产的 WHERE（platform/type/credentials_hash/proxy_id 四个条件）简化成
			// `id = 1`，参数因此重编号为 $1..$6 —— 但**写的那一列没变**（credentials），
			// 这正是本测试要验的东西。生产形态由下面的 drift 片段单独钉住。
			name:   "UpdateGrokOAuthCredentialsIfUnchanged",
			column: "credentials",
			sql: `
			WITH updated AS (
			UPDATE accounts AS a
			SET credentials = $1::jsonb,
				credentials_meta = $2::jsonb,
				credentials_encrypted = $3,
				credentials_hash = $4,
				credentials_api_key_hash = $5,
				updated_at = NOW()
			WHERE a.id = 1 AND a.deleted_at IS NULL
			RETURNING a.id
			)
			INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
			SELECT $6, updated.id, NULL, NULL FROM updated`,
			args: []any{
				[]byte(`{"k":"v"}`), []byte(`{}`), "ciph", "hash", "akhash", "account.changed",
			},
			drift: struct{ file, mustMatch string }{
				file:      "account_repo.go",
				mustMatch: "SET credentials = $1::jsonb, credentials_meta = $8::jsonb, credentials_encrypted = $9, credentials_hash = $10, credentials_api_key_hash = $11",
			},
		},
		{
			// 写：account_repo.go BulkUpdate 的合并子句（占位符是拼出来的）
			name:   "BulkUpdate.merge",
			column: "credentials",
			sql:    `UPDATE accounts SET credentials = COALESCE(credentials, '{}'::jsonb) || $1::jsonb WHERE id = 1`,
			args:   []any{[]byte(`{"k":"v"}`)},
			drift: struct{ file, mustMatch string }{
				file:      "account_repo.go",
				mustMatch: `"credentials = COALESCE(credentials, '{}'::jsonb) || "`,
			},
		},
		{
			// 写：UpdateCredentials 的整份替换
			name:   "UpdateCredentials",
			column: "credentials",
			sql:    `UPDATE accounts SET credentials = $1::jsonb WHERE id = 1`,
			args:   []any{[]byte(`{"k":"v"}`)},
			drift: struct{ file, mustMatch string }{
				file:      "account_repo.go",
				mustMatch: "UPDATE accounts SET credentials = $1::jsonb,",
			},
		},
		{
			// 写：api_key_repo.go deleteWithTombstone —— 软删时把 key 改写为墓碑值。
			// drop `api_keys.key` 之后这条也会报错；改造时要连指纹一起处理
			// （tombstone 必须同时改写 key_hash，否则"已删除的 key 仍能被指纹命中"）。
			name:   "api_keys.deleteWithTombstone",
			column: "key",
			sql:    `UPDATE api_keys SET key = $1, deleted_at = NOW(), updated_at = NOW() WHERE id = $2 AND deleted_at IS NULL`,
			args:   []any{"__deleted__sk-worklist", int64(1)},
			drift: struct{ file, mustMatch string }{
				file:      "api_key_repo.go",
				mustMatch: "UPDATE api_keys SET key = $1, deleted_at = NOW(), updated_at = NOW()",
			},
		},
	}

	// ── 抄件漂移守卫：作业单里的语句必须仍在源码里存在 ──
	// 不在 = 源码已经改过/搬走，抄件陈旧；此时下面那些"drop 之后会炸"的结论
	// 说的已不是生产里跑的那条语句。修法：按源码更新作业单条目（并想清楚新形态是否还依赖明文列）。
	squash := func(s string) string {
		return strings.Join(strings.Fields(s), "")
	}
	for _, stmt := range statements {
		raw, err := os.ReadFile(stmt.drift.file)
		require.NoError(t, err, "读源码文件 %s 失败", stmt.drift.file)
		require.Contains(t, squash(string(raw)), squash(stmt.drift.mustMatch),
			"%s 在源码里已经不存在了 —— 作业单是陈旧的抄件。\n"+
				"按 %s 的当前形态更新这条语句，并确认它还依赖明文列。",
			stmt.name, stmt.drift.file)
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

	_, err = db.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN credentials; ALTER TABLE api_keys DROP COLUMN key`)
	require.NoError(t, err)

	// ── drop 之后：每条都必须失败，且错误点名被 drop 的那一列 ──
	for _, stmt := range statements {
		execErr := probe(stmt)
		require.Error(t, execErr, "%s 在 drop 之后必须失败（这正是阶段四第②步要改掉的地方）", stmt.name)
		// 每条语句只依赖一列，所以期望列名跟着语句走（accounts 是 credentials、api_keys 是 key）。
		// 只断言"点名的列 + 不存在"两段，不锁整句措辞：
		// PG17 是 `column "credentials" of relation "accounts" does not exist`，
		// 老版本没有 `of relation ...` 这一段。锁整句会让升级 PG 时假红。
		require.Contains(t, execErr.Error(), `column "`+stmt.column+`"`,
			"%s 的失败原因必须点名被 drop 的列（%s）", stmt.name, stmt.column)
		require.Contains(t, execErr.Error(), "does not exist",
			"%s 的失败原因必须是列不存在，而不是别的错——否则说明这条语句已不依赖明文列", stmt.name)
	}
}
