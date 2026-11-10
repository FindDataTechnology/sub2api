package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 存量回填的真 PG 测试（无 DSN 时跳过）。
//
// 运行：
//
//	createdb sub2api_backfill_drill
//	SUB2API_CREDENTIAL_DIFF_DSN='postgres:///sub2api_backfill_drill?sslmode=disable' \
//	  go test ./internal/repository/ -run 'CredentialBackfill' -v

// 每个用例都从"干净 schema"开始：这些 scratch 测试共用一个库，
// 而建过完整 ent schema 的用例会留下外键，届时 `DROP TABLE accounts`（无 CASCADE）
// 会被依赖对象挡住。整 schema 重置比逐表删更稳，也不依赖表之间的依赖顺序。
const backfillDrillDDL = `
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
CREATE TABLE accounts (
	id                       bigserial PRIMARY KEY,
	platform                 text NOT NULL DEFAULT 'openai',
	deleted_at               timestamptz,
	type                     text NOT NULL DEFAULT 'apikey',
	credentials              jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials_meta         jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials_encrypted    text,
	credentials_hash         varchar(64),
	credentials_api_key_hash varchar(64)
);
CREATE TABLE api_keys (
	id            bigserial PRIMARY KEY,
	key           varchar(128) NOT NULL,
	key_hash      varchar(64),
	key_encrypted text
);
`

func backfillDrillDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；回填演练需要真 PostgreSQL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(backfillDrillDDL)
	require.NoError(t, err)
	return db
}

func seedBackfillDrill(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO accounts (id, platform, type, credentials) VALUES
			(1, 'openai', 'apikey', '{"api_key":"sk-a","base_url":"https://ollama.com"}'),
			(2, 'opencode_go', 'apikey', '{"api_key":"sk-b","account_mode":"go"}'),
			(3, 'anthropic', 'oauth', '{"access_token":"at","refresh_token":"rt","base_url":"https://x"}'),
			(4, 'openai', 'apikey', '{}');
		INSERT INTO api_keys (id, key) VALUES (10, 'sk-cust-1'), (11, 'sk-cust-2');
	`)
	require.NoError(t, err)

	// 先造一行"schema 里不存在"的坏数据是不可行的（jsonb 会拒绝），
	// 所以坏行用 api_keys 的空 key 表示，见下面的断言。
}

func countBackfillPending(t *testing.T, db *sql.DB) (accounts, apiKeys int) {
	t.Helper()
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM accounts WHERE `+credentialNeedsBackfillSQL).Scan(&accounts))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM api_keys WHERE `+apiKeyNeedsBackfillSQL).Scan(&apiKeys))
	return accounts, apiKeys
}

// TestCredentialBackfill_FillsAllMissingColumns 核心正向：回填后不再有"缺派生列"的行，
// 且哈希与运行时计算完全一致（这是阶段二切比较的前提）。
func TestCredentialBackfill_FillsAllMissingColumns(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)
	cryptor := credentialTestEncryptor(t)

	beforeAccounts, beforeKeys := countBackfillPending(t, db)
	require.Equal(t, 4, beforeAccounts)
	require.Equal(t, 2, beforeKeys)

	result, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)
	require.Equal(t, 4, result.AccountsUpdated)
	require.Equal(t, 2, result.APIKeysUpdated)
	require.Zero(t, result.AccountsFailed)
	require.Zero(t, result.APIKeysFailed)

	afterAccounts, afterKeys := countBackfillPending(t, db)
	require.Zero(t, afterAccounts, "回填后不该还有缺派生列的账号")
	require.Zero(t, afterKeys, "回填后不该还有缺派生列的 api_key")

	// 哈希必须与运行时同一套实现算出的一致
	var hash string
	require.NoError(t, db.QueryRow(`SELECT credentials_hash FROM accounts WHERE id = 1`).Scan(&hash))
	expected, err := CredentialCryptoColumnsFor(map[string]any{
		"api_key": "sk-a", "base_url": "https://ollama.com",
	}, nil)
	require.NoError(t, err)
	require.Equal(t, *expected.Hash, hash)

	// meta 只带 base_url（account_mode 不存在于 id=1）+ 派生事实 has_refresh_token=false
	// （id=1 的凭据里没有 refresh_token）。回填工具走同一个 credentialMetaSubset，
	// 所以它也必然带上这个事实——这正是"回填与运行时同源"的一个可观测断言。
	var meta string
	require.NoError(t, db.QueryRow(`SELECT credentials_meta::text FROM accounts WHERE id = 1`).Scan(&meta))
	require.JSONEq(t, `{"base_url":"https://ollama.com","has_refresh_token":false}`, meta)

	// 密文必须能解回原文档
	var encrypted string
	require.NoError(t, db.QueryRow(`SELECT credentials_encrypted FROM accounts WHERE id = 1`).Scan(&encrypted))
	plaintext, err := cryptor.Decrypt(encrypted)
	require.NoError(t, err)
	require.JSONEq(t, `{"api_key":"sk-a","base_url":"https://ollama.com"}`, plaintext)

	// 空凭据账号：hash 是 `{}` 的哈希（不是 NULL），meta 为空对象
	var emptyHash string
	require.NoError(t, db.QueryRow(`SELECT credentials_hash FROM accounts WHERE id = 4`).Scan(&emptyHash))
	emptyExpected, err := CredentialCryptoColumnsFor(map[string]any{}, nil)
	require.NoError(t, err)
	require.Equal(t, *emptyExpected.Hash, emptyHash)

	// api_key 指纹
	var keyHash string
	require.NoError(t, db.QueryRow(`SELECT key_hash FROM api_keys WHERE id = 10`).Scan(&keyHash))
	require.Equal(t, sha256Hex([]byte("sk-cust-1")), keyHash)
}

// TestCredentialBackfill_IsIdempotent 重复执行必须不再改动任何行——
// 否则每次运维都重写一遍密文（非确定性）会让所有乐观并发比较失效。
func TestCredentialBackfill_IsIdempotent(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)
	cryptor := credentialTestEncryptor(t)

	first, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)
	require.Equal(t, 4, first.AccountsUpdated)

	var encBefore string
	require.NoError(t, db.QueryRow(`SELECT credentials_encrypted FROM accounts WHERE id = 1`).Scan(&encBefore))

	second, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)
	require.Zero(t, second.AccountsScanned, "第二次不该扫到任何待回填行")
	require.Zero(t, second.AccountsUpdated)
	require.Zero(t, second.APIKeysUpdated)

	var encAfter string
	require.NoError(t, db.QueryRow(`SELECT credentials_encrypted FROM accounts WHERE id = 1`).Scan(&encAfter))
	require.Equal(t, encBefore, encAfter, "幂等：密文不该被重写（非确定性加密会变）")
}

// TestCredentialBackfill_DryRunWritesNothing 演练模式必须零写入。
func TestCredentialBackfill_DryRunWritesNothing(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)
	cryptor := credentialTestEncryptor(t)

	result, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, 4, result.AccountsUpdated, "dry-run 也要报告将处理多少行")

	accounts, keys := countBackfillPending(t, db)
	require.Equal(t, 4, accounts, "dry-run 不该写库")
	require.Equal(t, 2, keys, "dry-run 不该写库")
}

// TestCredentialBackfill_RequiresEncryptor 没有加密器时必须直接拒绝：
// 否则会补上 meta/指纹但密文仍 NULL，跑出一个"看起来迁了一半"的状态，
// 阶段二切换后读路径永远回落明文。
func TestCredentialBackfill_RequiresEncryptor(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)

	_, err := BackfillCredentialColumns(context.Background(), db, nil, CredentialBackfillOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "encryptor is required")

	accounts, keys := countBackfillPending(t, db)
	require.Equal(t, 4, accounts)
	require.Equal(t, 2, keys)
}

// TestCredentialBackfill_LimitAndBatching 分页 + 限制：小批量能跑完，Limit 能截断。
func TestCredentialBackfill_LimitAndBatching(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)
	cryptor := credentialTestEncryptor(t)

	// 批大小 1 + 限 2 行：只回填 2 个账号，剩 2 个仍未迁
	result, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{
		BatchSize: 1,
		Limit:     2,
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.AccountsUpdated)

	accounts, _ := countBackfillPending(t, db)
	require.Equal(t, 2, accounts)

	// 再跑一次不限量，必须把剩下的补完（证明 keyset 分页没有跳过行）
	rest, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{BatchSize: 1})
	require.NoError(t, err)
	require.Equal(t, 2, rest.AccountsUpdated)
	remaining, _ := countBackfillPending(t, db)
	require.Zero(t, remaining)
}

// TestDeriveCredentialColumnsAfterMerge_RepairsDerivedColumns 是 2.4 的真 PG 验收：
// 合并语句把派生列置空后，回读重算必须把它们补回**正确**的值（而不是留在 NULL）。
//
// 为什么这条必须有真库：这段逻辑是"写完之后再读一次再算一次"，纯 sqlmock 只能证明
// SQL 文本里出现了 SELECT，证明不了它读到的到底是合并前还是合并后的明文。
func TestDeriveCredentialColumnsAfterMerge_RepairsDerivedColumns(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)
	cryptor := credentialTestEncryptor(t)

	// 模拟"合并已发生、派生列被置空"的中间状态
	_, err := db.Exec(`
		UPDATE accounts
		SET credentials = COALESCE(credentials, '{}'::jsonb) || '{"api_key":"sk-rotated"}'::jsonb,
			credentials_meta = '{}'::jsonb,
			credentials_encrypted = NULL,
			credentials_hash = NULL,
			credentials_api_key_hash = NULL
		WHERE id = 1`)
	require.NoError(t, err)

	require.NoError(t, deriveCredentialColumnsAfterMerge(context.Background(), db, cryptor, []int64{1, 2, 3}))

	// id=1：合并后是 {"api_key":"sk-rotated","base_url":"https://ollama.com"}
	expectedMerged := map[string]any{"api_key": "sk-rotated", "base_url": "https://ollama.com"}
	want, err := CredentialCryptoColumnsFor(expectedMerged, cryptor)
	require.NoError(t, err)

	var hash string
	var apiKeyHash, encrypted *string
	require.NoError(t, db.QueryRow(
		`SELECT credentials_hash, credentials_api_key_hash, credentials_encrypted FROM accounts WHERE id = 1`,
	).Scan(&hash, &apiKeyHash, &encrypted))

	require.Equal(t, *want.Hash, hash, "哈希必须按**合并后**的明文算")
	require.NotNil(t, apiKeyHash)
	require.Equal(t, *want.APIKeyHash, *apiKeyHash)
	require.NotNil(t, encrypted, "密文列必须被补上，不能留 NULL")

	plaintext, err := cryptor.Decrypt(*encrypted)
	require.NoError(t, err)
	require.JSONEq(t, `{"api_key":"sk-rotated","base_url":"https://ollama.com"}`, plaintext)

	// 顺带确认它没有把没被合并的行写坏
	var untouched string
	require.NoError(t, db.QueryRow(`SELECT credentials_hash FROM accounts WHERE id = 2`).Scan(&untouched))
	expected2, err := CredentialCryptoColumnsFor(map[string]any{"api_key": "sk-b", "account_mode": "go"}, cryptor)
	require.NoError(t, err)
	require.Equal(t, *expected2.Hash, untouched)
}

// 没有加密器时密文列必须写 NULL（而不是留着过期密文）。
func TestDeriveCredentialColumnsAfterMerge_NilCryptorClearsCiphertext(t *testing.T) {
	db := backfillDrillDB(t)
	seedBackfillDrill(t, db)
	cryptor := credentialTestEncryptor(t)

	_, err := BackfillCredentialColumns(context.Background(), db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)

	var before string
	require.NoError(t, db.QueryRow(`SELECT credentials_encrypted FROM accounts WHERE id = 1`).Scan(&before))
	require.NotEmpty(t, before)

	require.NoError(t, deriveCredentialColumnsAfterMerge(context.Background(), db, nil, []int64{1}))

	var after *string
	require.NoError(t, db.QueryRow(`SELECT credentials_encrypted FROM accounts WHERE id = 1`).Scan(&after))
	require.Nil(t, after, "无加密器时不留过期密文，读路径回落明文")
}

// TestMigration250_HasRefreshTokenMatchesPlaintext 是站点 A（① 案）的**闸门级**验证，
// 强度对齐 ④（独立列）那条"列值 vs 明文逐行比对"。
//
// 为什么必须跑**迁移文件本身**而不是复算 Go 逻辑：迁移里的 SQL 是手工写的表达式，
// 它与 credentialHasRefreshToken 是两份实现。只测 Go 的话，迁移写错了照样绿——
// 而线上存量行的值恰恰只由迁移决定。
//
// 断言三件事：
//  1. 跑完迁移后，meta 里的值与明文逐行一致（0 处不一致）
//  2. 幂等：再跑一次，值与行数都不变
//  3. 空串 / 纯空白 / JSON null / 数字 这些边界形态都与 SQL 语义一致
func TestMigration250_HasRefreshTokenMatchesPlaintext(t *testing.T) {
	db := backfillDrillDB(t)

	sqlBytes, err := os.ReadFile("../../migrations/250_credentials_meta_has_refresh_token.sql")
	require.NoError(t, err, "迁移文件必须能被读到；路径变了要同步改这里")
	migrationSQL := string(sqlBytes)

	// 边界形态：正常值 / 空串 / 纯空白 / JSON null / 数字 / 键缺失 / 空对象
	_, err = db.Exec(`
		INSERT INTO accounts (id, platform, type, credentials) VALUES
			(1, 'grok', 'oauth', '{"refresh_token":"rt-1","access_token":"at"}'),
			(2, 'grok', 'oauth', '{"refresh_token":"","access_token":"at"}'),
			(3, 'grok', 'oauth', '{"refresh_token":"   ","access_token":"at"}'),
			(4, 'grok', 'oauth', '{"refresh_token":null,"access_token":"at"}'),
			(5, 'grok', 'oauth', '{"refresh_token":123,"access_token":"at"}'),
			(6, 'grok', 'oauth', '{"access_token":"at"}'),
			(7, 'grok', 'oauth', '{}');
	`)
	require.NoError(t, err)

	run := func() {
		_, err := db.Exec(migrationSQL)
		require.NoError(t, err, "迁移必须可执行")
		_, err = db.Exec(migrationSQL) // 连跑两次验幂等
		require.NoError(t, err, "迁移必须幂等")
	}
	run()

	// 逐行比对：meta 里的值 vs 明文表达式。不一致必须为 0。
	var mismatches int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM accounts
		WHERE COALESCE((credentials_meta ->> 'has_refresh_token')::boolean, false)
		   IS DISTINCT FROM COALESCE(NULLIF(BTRIM(credentials ->> 'refresh_token'), '') IS NOT NULL, false)
	`).Scan(&mismatches))
	require.Zero(t, mismatches, "迁移算出的 has_refresh_token 必须与明文逐行一致")

	// 逐 id 钉住期望值，避免"整体一致但全错成同一个值"这种假绿
	want := map[int64]bool{1: true, 2: false, 3: false, 4: false, 5: true, 6: false, 7: false}
	for id, expected := range want {
		var got bool
		require.NoError(t, db.QueryRow(
			`SELECT COALESCE((credentials_meta ->> 'has_refresh_token')::boolean, false) FROM accounts WHERE id = $1`,
			id).Scan(&got))
		require.Equal(t, expected, got, "id=%d 的 has_refresh_token 与 SQL 语义不符", id)
	}

	// 幂等后再确认一次行数没变（防止迁移误删/误插）
	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM accounts`).Scan(&rows))
	require.Equal(t, 7, rows)
}
