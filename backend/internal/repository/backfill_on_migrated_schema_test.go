package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 在**真实迁移链**建出来的 schema 上跑「造存量 → 回填 → 判据归零」。
//
// 之前的回填测试用的是手搓的两张表：列对得上，但**索引/非空/默认值/约束**都对不上。
// 回填工具恰恰是靠 `key <> ''`、`NOT NULL DEFAULT '{}'` 这类真实约束工作的，
// 所以只在玩具表上绿，不能说明它在生产库上会绿。
// 这里用 ApplyMigrations 把整条迁移链（303 个）全跑一遍，再插"迁移前形态"的行。
//
// 判据与 `scripts/credential-migration-status.sh` 的 A/B/C/D 四项完全一致
// —— 那个脚本的阈值就是这几条 SQL，两条路径必须给出同一个结论。
// restoreLegacyPlaintextColumns 在迁移链建出的 schema 上把 5.2 之前的两列**加回来**。
//
// 这几条用例验的是 **drop 之前**的形态，而不是 drop 之后：
//   - 回填工具（backfill）的输入就是明文列，它只在这个形态上有意义；
//   - 恢复演练的备份来自 drop 之前；
//   - 状态脚本的 pre 分支要求能读到这两列。
//
// 迁移 251 之后真实迁移链不再建这两列，所以在这里显式补回来 —— 而不是让用例
// 去"假装"它们还在（假装的话 SQL 会直接 `column "credentials" does not exist`）。
// 只补列、不补唯一索引：唯一性已经由 key_hash 上的部分唯一索引承担，
// 这几条用例也不依赖明文 key 的唯一约束。
func restoreLegacyPlaintextColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials jsonb NOT NULL DEFAULT '{}'::jsonb;
		ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key varchar(128) NOT NULL DEFAULT '';`)
	require.NoError(t, err, "把 drop 之前的明文列加回来（本组用例验的是 drop 之前的形态）")
}

func TestBackfillOnMigratedSchema_DrivesStatusGatesToZero(t *testing.T) {
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
	require.NoError(t, ApplyMigrations(ctx, db), "把真实迁移链跑完（含 249 派生列 / 250 meta 事实）")
	restoreLegacyPlaintextColumns(t, db) // 本用例验的是 drop 之前的形态

	// 造"迁移前形态"：明文在、派生列全空、meta 缺 has_refresh_token
	// 真实 schema 上 api_keys.user_id 有外键，先造一个最小用户（其余 NOT NULL 列都有默认值）。
	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('backfill-drill@example.test', 'x') RETURNING id`).
		Scan(&userID))
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials, credentials_meta) VALUES
		 ('acc-oauth', 'openai', 'oauth', '{"refresh_token":"r1","access_token":"a1","base_url":"https://x"}', '{}'::jsonb),
		 ('acc-key',   'openai', 'apikey', '{"api_key":"sk-live-1","account_mode":"codex"}', '{}'::jsonb),
		 ('acc-empty', 'openai', 'apikey', '{}'::jsonb, '{}'::jsonb)`)
	require.NoError(t, err, "按真实约束插账号存量")
	_, err = db.ExecContext(ctx, `
		INSERT INTO api_keys (user_id, key, name, status) VALUES
		 ($1, 'sk-live-1', 'k1', 'active'),
		 ($1, '', 'empty', 'active')`, userID)
	require.NoError(t, err, "按真实约束插 key 存量（外键/非空列都由迁移定义）")

	// ── 判据（与状态脚本 A/B/C/D 四项逐字一致）──
	count := func(q string) int64 {
		var n int64
		require.NoError(t, db.QueryRowContext(ctx, q).Scan(&n), "判据查询失败: %s", q)
		return n
	}
	gates := func() (accPending, apiPending, metaMissing, keyNoHash int64) {
		return count(`SELECT count(*) FROM accounts WHERE (credentials_encrypted IS NULL OR credentials_hash IS NULL)`),
			count(`SELECT count(*) FROM api_keys WHERE (key <> '' AND (key_encrypted IS NULL OR key_hash IS NULL))`),
			count(`SELECT count(*) FROM accounts WHERE NOT (credentials_meta ? 'has_refresh_token')`),
			count(`SELECT count(*) FROM api_keys WHERE key_hash IS NULL AND key <> '' AND deleted_at IS NULL`)
	}

	// ① 回填前：三项必须是非零（否则下面的"归零"没有意义 —— 空跑也能过）
	accPending, apiPending, metaMissing, keyNoHash := gates()
	require.NotZero(t, accPending, "回填前 accounts 待回填必须非零")
	require.NotZero(t, apiPending, "回填前 api_keys 待回填必须非零")
	require.NotZero(t, metaMissing, "回填前 meta 缺事实必须非零")
	require.NotZero(t, keyNoHash, "回填前缺指纹必须非零")

	pendingAcc, pendingKey, err := CountCredentialBackfillPending(ctx, db)
	require.NoError(t, err)
	require.Equal(t, accPending, pendingAcc, "工具自己的待办计数与判据必须一致")
	require.Equal(t, apiPending, pendingKey, "同上（api_keys）")

	// ② dry-run：**不写库**，但会把"会更新多少"计进 Updated。
	//
	// 这里差点写错：我原本断言 Updated == 0，跑出来是 3。查实现才发现
	// `if opts.DryRun { result.AccountsUpdated++; continue }` —— 它的语义是
	// "would update"，不是"已写入"。所以：
	//   - 安全判据看**库里的计数没变**（下面这条才是真的）
	//   - Updated 要与"回填前的待办数"相等，否则这个数字没有意义
	//     （比如恒为 0 也能让上面的"不写库"看起来成立）
	result, err := BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, accPending, int64(result.AccountsUpdated),
		"dry-run 的 Updated 是「会更新多少」，应等于回填前的待办数")
	require.Equal(t, apiPending, int64(result.APIKeysUpdated), "同上（api_keys）")
	accPendingD, apiPendingD, metaMissingD, keyNoHashD := gates()
	require.Equal(t, accPending, accPendingD, "dry-run 之后 accounts 判据必须不变（这才是'不写库'）")
	require.Equal(t, apiPending, apiPendingD, "dry-run 之后 api_keys 判据必须不变")
	require.Equal(t, metaMissing, metaMissingD, "dry-run 之后 meta 判据必须不变")
	require.Equal(t, keyNoHash, keyNoHashD, "dry-run 之后指纹判据必须不变")

	// ③ 真跑：四项全部归零，且空 key/空凭据被记为 skipped 而不是 failed
	result, err = BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)
	require.Zero(t, result.AccountsFailed, "不许有失败行")
	require.Zero(t, result.APIKeysFailed, "不许有失败行")
	// 空凭据**不是** skipped：它也在待回填集合里，必须被派生（否则密文列永远 NULL、
	// 闸门永远过不去）。曾经有个 AccountsSkipped 字段描述相反的行为，但从未被自增，
	// 而 CLI 会把 `skipped=0` 打给运维看 —— 一个永远为 0 的计数器比没有更有害，已删除。
	// 空 key 在 api_keys 侧被 `key <> ''` 的判据排除，所以 skipped 也是 0；
	// 那段自增只是防御（查询条件将来若放宽，它会把形态问题与故障分开）。
	require.Zero(t, result.APIKeysSkipped, "空 key 被判据排除，不该进入回填循环")

	accPending, apiPending, metaMissing, keyNoHash = gates()
	require.Zero(t, accPending, "回填后 accounts 待回填必须为 0")
	require.Zero(t, apiPending, "回填后 api_keys 待回填必须为 0")
	require.Zero(t, metaMissing, "回填后 meta 事实必须齐全")
	require.Zero(t, keyNoHash, "回填后缺指纹必须为 0")

	// ④ 幂等：再跑一次不应再有更新，判据仍为 0
	result, err = BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)
	require.Zero(t, result.AccountsUpdated, "第二次回填不应再更新（幂等）")
	require.Zero(t, result.APIKeysUpdated, "第二次回填不应再更新（幂等）")
}
