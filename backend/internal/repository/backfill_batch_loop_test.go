package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 回填工具的**分批循环**：真实 schema + 生产形状的规模。
//
// 已有的 `TestCredentialBackfill_LimitAndBatching` 在 4 行玩具表上验过 keyset 分页
// （batch=1、Limit=2），但它验的是 **Limit > Batch**；发布清单建议生产"先 `-limit 50`"，
// 而默认 batch 是 200，实际形状是 **Limit < Batch** —— 即"取一批 200 行、只写 50 行
// 就停"的截断路径，玩具用例一次都没走到。另外它只有 4 行，跨批从不跨过一整批。
//
// 做法：真实迁移链建 250+250 行，三档走完，每档都同时断言"处理了多少"与"还剩多少"
// —— 只断言其中一个，恒为 0 或恒为全量都能骗过。
func TestBackfillOnMigratedSchema_BatchLoopAndLimit(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db))

	const total = 250
	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('batch@example.test', 'x') RETURNING id`).Scan(&userID))
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials, credentials_meta)
		SELECT 'bulk-' || g, 'openai', 'apikey',
		       jsonb_build_object('api_key', 'sk-bulk-' || g), '{}'::jsonb
		FROM generate_series(1, $1::int) g`, total)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO api_keys (user_id, key, name, status)
		SELECT $1, 'sk-bulk-key-' || g, 'bulk-' || g, 'active'
		FROM generate_series(1, $2::int) g`, userID, total)
	require.NoError(t, err)

	pending := func() (int64, int64) {
		acc, keys, err := CountCredentialBackfillPending(ctx, db)
		require.NoError(t, err)
		return acc, keys
	}
	accPending, keyPending := pending()
	require.Equal(t, int64(total), accPending, "起始待办必须等于种入行数，否则这一组断言没有意义")
	require.Equal(t, int64(total), keyPending, "同上（api_keys）")

	// 重置派生列，让三档从同一个起点开始。
	reset := func() {
		_, err := db.ExecContext(ctx, `
			UPDATE accounts SET credentials_encrypted = NULL, credentials_hash = NULL,
			                    credentials_api_key_hash = NULL;
			UPDATE api_keys SET key_encrypted = NULL, key_hash = NULL`)
		require.NoError(t, err)
		a, k := pending()
		require.Equal(t, int64(total), a, "重置后应回到全量待办")
		require.Equal(t, int64(total), k, "同上（api_keys）")
	}

	// ── 档 1：BatchSize=100，跑完全量 → 必须跨 3 批，扫过的行数恰好等于总行数 ──
	// scanned 恰好 250 同时证明两件事：没有漏行（<250）也没有重复处理（>250）。
	result, err := BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{BatchSize: 100})
	require.NoError(t, err)
	require.Equal(t, total, result.AccountsScanned, "跨批扫描必须一次不漏、一次不重")
	require.Equal(t, total, result.APIKeysScanned, "同上（api_keys）")
	require.Equal(t, total, result.AccountsUpdated)
	require.Equal(t, total, result.APIKeysUpdated)
	require.Zero(t, result.AccountsFailed)
	require.Zero(t, result.APIKeysFailed)
	accPending, keyPending = pending()
	require.Zero(t, accPending, "全量跑完剩余必须为 0")
	require.Zero(t, keyPending, "同上（api_keys）")

	// ── 档 2：Limit < BatchSize → 一批就截断，剩余 = 总量 − Limit ──
	// 这是清单里"生产先跑 -limit 50"的形状：截断必须发生在**写完之后立刻退出**，
	// 而不是"取了一批（100 行）就全写进去"。
	reset()
	const lim = 7
	result, err = BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{BatchSize: 100, Limit: lim})
	require.NoError(t, err)
	require.Equal(t, lim, result.AccountsUpdated, "Limit 必须是硬上限")
	require.Equal(t, lim, result.APIKeysUpdated, "Limit 对 api_keys 同样生效")
	accPending, keyPending = pending()
	require.Equal(t, int64(total-lim), accPending, "剩余 = 总量 − Limit")
	require.Equal(t, int64(total-lim), keyPending, "同上（api_keys）")

	// ── 档 3：BatchSize 与 Limit 都很小 → 跨多批且最终正好停在 Limit ──
	// 2+2+1 三批，证明"跨批"与"截断"能同时成立（档 1 只验跨批、档 2 只验截断）。
	reset()
	result, err = BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{BatchSize: 2, Limit: 5})
	require.NoError(t, err)
	require.Equal(t, 5, result.AccountsScanned, "跨 3 批但只处理 Limit 行（不能多扫）")
	require.Equal(t, 5, result.AccountsUpdated)
	require.Equal(t, 5, result.APIKeysScanned)
	accPending, keyPending = pending()
	require.Equal(t, int64(total-5), accPending)
	require.Equal(t, int64(total-5), keyPending)
}
