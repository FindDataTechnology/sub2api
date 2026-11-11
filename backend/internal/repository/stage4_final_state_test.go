package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestStage4FinalState_SQLWorksAfterDroppingPlaintextColumns 是**阶段四最终形态的演练**。
//
// 目的只有一个：证明「把明文列 drop 掉之后，改写过的 SQL 仍然能跑」。
// 这个没验过就 drop，等于拿不可逆动作赌一把——而失败形态是运行期
// "column does not exist"（还好是硬错），或者更糟：把 NULL 当成"没有凭据"（静默）。
//
// 做法：建最小 schema → 用生产里的**同一批 SQL 常量**跑一遍 → DROP 明文列 → 再跑一遍。
// 两次结果必须一致。
func TestStage4FinalState_SQLWorksAfterDroppingPlaintextColumns(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；阶段四最终形态演练需要真 PostgreSQL")
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
			status text NOT NULL DEFAULT 'active',
			extra jsonb NOT NULL DEFAULT '{}'::jsonb,
			credentials jsonb NOT NULL DEFAULT '{}'::jsonb,
			credentials_meta jsonb NOT NULL DEFAULT '{}'::jsonb,
			credentials_encrypted text,
			credentials_hash varchar(64),
			credentials_api_key_hash varchar(64)
		);
		CREATE TABLE api_keys (
			id bigserial PRIMARY KEY,
			key varchar(128) NOT NULL,
			key_hash varchar(64),
			key_encrypted text
		);`)
	require.NoError(t, err)

	// 三种形态：ollama 合格 / opencode 合格 / 都不合格
	repo := newAccountRepositoryWithSQL(nil, nil, nil)
	seeds := map[int64]map[string]any{
		1: {"api_key": "sk-ollama", "base_url": "https://ollama.com"},
		2: {"api_key": "sk-oc", "base_url": "https://opencode.ai/zen/go"},
		3: {"api_key": "sk-plain", "base_url": "https://example.com"},
	}
	for id, creds := range seeds {
		cols, err := repo.credentialCryptoColumnsFor(creds)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `
			INSERT INTO accounts (id, platform, type, credentials, credentials_meta, credentials_hash, credentials_api_key_hash)
			VALUES ($1, 'openai', 'apikey', $2::jsonb, $3::jsonb, $4, $5)`,
			id, mustJSON(t, creds), mustJSON(t, cols.Meta), *cols.Hash, cols.APIKeyHash)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO api_keys (id, key, key_hash) VALUES (1, 'sk-cust', $1)`,
		sha256Hex([]byte("sk-cust")))
	require.NoError(t, err)

	count := func(query string, args ...any) int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, query, args...).Scan(&n))
		return n
	}

	// 生产常量 + 改写后的谓词（这三条是阶段二的核心产物）
	ollamaQ := `SELECT count(*) FROM accounts WHERE deleted_at IS NULL AND ` + ollamaCloudUsageEligibleSQL
	opencodeQ := `SELECT count(*) FROM accounts WHERE deleted_at IS NULL AND ` + opencodeGoUsageEligibleSQL
	casQ := `SELECT count(*) FROM accounts WHERE id = $1 AND credentials_hash = $2`
	refreshQ := `SELECT count(*) FROM accounts WHERE ` + `credentials_meta @> '{"has_refresh_token": true}'`
	apiKeyQ := `SELECT count(*) FROM api_keys WHERE key_hash = $1`

	// ── drop 之前 ──
	require.Equal(t, 1, count(ollamaQ), "ollama 合格行")
	require.Equal(t, 1, count(opencodeQ), "opencode 合格行")
	require.Equal(t, 1, count(casQ, int64(1), *mustCols(t, repo, seeds[1]).Hash), "整文档 CAS")
	require.Equal(t, 0, count(refreshQ), "无 refresh_token 行")
	require.Equal(t, 1, count(apiKeyQ, sha256Hex([]byte("sk-cust"))), "按指纹查 key")

	// ── 阶段四：drop 明文列 ──
	_, err = db.ExecContext(ctx, `
		ALTER TABLE accounts DROP COLUMN credentials;
		ALTER TABLE api_keys DROP COLUMN key;`)
	require.NoError(t, err, "drop 本身必须能执行（说明没有未处理的依赖视图/约束）")

	// ── drop 之后：同样四条必须给出**同样结果** ──
	require.Equal(t, 1, count(ollamaQ), "drop 后 ollama 谓词必须仍工作")
	require.Equal(t, 1, count(opencodeQ), "drop 后 opencode 谓词必须仍工作")
	require.Equal(t, 1, count(casQ, int64(1), *mustCols(t, repo, seeds[1]).Hash), "drop 后 CAS 必须仍工作")
	require.Equal(t, 0, count(refreshQ), "drop 后 meta 谓词必须仍工作")
	require.Equal(t, 1, count(apiKeyQ, sha256Hex([]byte("sk-cust"))), "drop 后按指纹查 key 必须仍工作")
}

func mustCols(t *testing.T, repo *accountRepository, creds map[string]any) *CredentialCryptoColumns {
	t.Helper()
	cols, err := repo.credentialCryptoColumnsFor(creds)
	require.NoError(t, err)
	return &cols
}
