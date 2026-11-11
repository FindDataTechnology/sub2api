package repository

import (
	"context"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"database/sql"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
)

// 生产接线回归：**用生产构造函数**读一条"只有密文"的账号，必须能解出来。
//
// 为什么必须有这一条（2026-11-11 全站 503 事故）：
// `wire_gen.go` 里 account repository 曾用 `cryptor=nil` 的构造器（api_key/usage_log
// 都换了 Crypto 版，唯独它没换）。回填之后每个账号都有密文，于是调度快照重建时
// 读到第一行就硬失败（"has encrypted credentials but no encryptor is configured"），
// 整批 bucket 重建失败 → **全站推理 503 约 75 分钟**。
//
// 当时的测试全都是用 `newAccountRepositoryWithSQLCrypto` 明确传了 cryptor 构造的，
// 所以"少接一路"永远测不出来。这条测试刻意**只用导出构造函数**（= 生产用的那个），
// 现在它要求 cryptor 必填，少传参数编译不过 —— 类型系统是第一道闸，这条是第二道。
func TestProductionAccountRepository_DecryptsBackfilledAccount(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db))

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	defer func() { _ = client.Close() }()

	// 用生产构造函数（不是 internal 的 crypto 版）。
	repo := NewAccountRepository(client, db, nil, cryptor)

	// 造一条"已回填"的账号：密文非空。
	cols, err := CredentialCryptoColumnsFor(map[string]any{"api_key": "sk-prod-wire"}, cryptor)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials, credentials_meta,
			credentials_hash, credentials_encrypted, credentials_api_key_hash)
		VALUES ('wire-check', 'openai', 'apikey', '{"api_key":"sk-prod-wire"}'::jsonb,
			$1::jsonb, $2, $3, $4)`,
		mustJSON(t, cols.Meta), *cols.Hash, *cols.Encrypted, cols.APIKeyHash)
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, 1)
	require.NoError(t, err, "生产构造函数必须能解密回填后的账号；"+
		"若这里报 'has encrypted credentials but no encryptor is configured'，说明又有一路接线漏了 cryptor")
	require.Equal(t, "sk-prod-wire", got.Credentials["api_key"],
		"读出来的必须是解密后的明文（不是密文，也不是空）")
}

// 反向对照：**cryptor 为 nil** 时，同一个构造函数必须硬失败而不是静默回落明文。
// 这条是"故意坏掉"的对照，证明上一条不是永真条件：如果哪天有人把 nil 分支改成
// "读明文凑一个值"，上一条仍然绿，而这一条会红。
func TestInternalAccountRepository_NilCryptorRefusesCiphertext(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	defer func() { _ = client.Close() }()

	// 内部构造器仍允许 nil（测试要用），但"有密文"时必须拒绝。
	nilRepo := newAccountRepositoryWithSQL(client, db, nil)
	cols, err := CredentialCryptoColumnsFor(map[string]any{"api_key": "sk-nil"}, cryptor)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials, credentials_meta,
			credentials_hash, credentials_encrypted, credentials_api_key_hash)
		VALUES ('nil-check', 'openai', 'apikey', '{"api_key":"sk-nil"}'::jsonb,
			$1::jsonb, $2, $3, $4)`,
		mustJSON(t, cols.Meta), *cols.Hash, *cols.Encrypted, cols.APIKeyHash)
	require.NoError(t, err)

	_, err = nilRepo.GetByID(ctx, 1)
	require.Error(t, err, "有密文 + 无加密器时必须硬失败")
	require.Contains(t, err.Error(), "no encryptor is configured")
}
