package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 这是一个**给手工演练用的建库入口**，不是一个断言型测试。
//
// 目的：让 `scripts/credential-restore-drill.sh` 能在一个由**真实迁移链**
// （302 个迁移、真外键/索引/约束）建出来的库上跑一遍。
// 演练脚本此前只在手搓的两三张表上验过 —— 而它是 drop 之后**唯一退路**的证据来源，
// 在一个 300 表的真库上能不能完整 dump/restore，是玩具库回答不了的问题。
//
// 用法（两步，中间不要动这个库）：
//
//	SUB2API_DRILL_SEED=1 SUB2API_CREDENTIAL_DIFF_DSN='postgres:///sub2api_drill_real?sslmode=disable' \
//	  go test ./internal/repository/ -run TestDrillSeed_RealSchema -count=1
//	PG_BIN=/opt/homebrew/opt/postgresql@17/bin \
//	  ./scripts/credential-restore-drill.sh 'postgres:///sub2api_drill_real?sslmode=disable' /tmp/drill-real
//
// 不设置 SUB2API_DRILL_SEED 时直接跳过，所以它不会污染常规测试跑批。
func TestDrillSeed_RealSchema(t *testing.T) {
	if os.Getenv("SUB2API_DRILL_SEED") == "" {
		t.Skip("需要 SUB2API_DRILL_SEED=1：这一步会给手工演练准备一个真实 schema 的库")
	}
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	require.NotEmpty(t, dsn, "需要 SUB2API_CREDENTIAL_DIFF_DSN")
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db), "真实迁移链（302 个迁移）")

	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('drill@example.test', 'x') RETURNING id`).Scan(&userID))

	// 造几条"已回填"的账号与 key：密文列非空，演练脚本的判据 3 才有东西可比。
	seedAccount := func(name string, creds map[string]any) {
		cols, err := CredentialCryptoColumnsFor(creds, cryptor)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `
			INSERT INTO accounts (name, platform, type, credentials, credentials_meta,
				credentials_hash, credentials_encrypted, credentials_api_key_hash)
			VALUES ($1, 'openai', 'oauth', $2::jsonb, $3::jsonb, $4, $5, $6)`,
			name, mustJSON(t, creds), mustJSON(t, cols.Meta), *cols.Hash, *cols.Encrypted, cols.APIKeyHash)
		require.NoError(t, err)
	}
	seedAccount("drill-oauth", map[string]any{"refresh_token": "r-1", "access_token": "sk-drill-aaaaaaaaaaaaaaaa", "base_url": "https://x"})
	seedAccount("drill-apikey", map[string]any{"api_key": "sk-drill-bbbbbbbbbbbbbbbb", "account_mode": "codex"})

	for _, key := range []string{"sk-drill-cccccccccccccccc", "sk-drill-dddddddddddddddd"} {
		hash, encrypted, err := apiKeyDerivedColumns(key, cryptor)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `
			INSERT INTO api_keys (user_id, key, name, status, key_hash, key_encrypted)
			VALUES ($1, $2, 'drill', 'active', $3, $4)`, userID, key, hash, *encrypted)
		require.NoError(t, err)
	}

	t.Logf("已准备：真实 schema + 2 账号 + 2 key（dsn=%s）", dsn)
	t.Log("下一步跑演练脚本（见本文件顶部注释），不要在中间改这个库")
}
