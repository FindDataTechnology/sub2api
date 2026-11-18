package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 阶段 5.2 的**最终形态演练**：在一个已经 drop 掉明文列的**真实生产备份恢复库**上，
// 只用密文列解出凭据，并断言解出来的还是可用 JSON。
//
// 为什么必须单独一条（而不是复用别的演练）：
//   - `TestProdRehearsal_DecryptsBackToPlaintext` 要读明文列做逐字节比对 ——
//     drop 之后那列不存在，它连编译期 SQL 都过不去（这正说明它验不了"drop 之后"）。
//   - `credential-migration-status.sh` 只回答"派生列齐不齐"，不回答"密文还能不能解开"。
//     drop 之后**唯一**的可用性证据就是"解得开、且结构正确"。
//
// 判据（全部满足才算过）：
//  1. 明文列确实不存在（否则这条测试测的不是最终形态）；
//  2. 每个 accounts.credentials_encrypted 都能解密且是**非空 JSON 对象**；
//  3. 每个 api_keys.key_encrypted 都能解出非空字符串；
//  4. 样本数非零，且至少有一个账号解出 api_key/refresh_token 这类真实字段。
//
// 用法：
//
//	TOTP_ENCRYPTION_KEY=<生产密钥> SUB2API_PROD_REHEARSAL_DSN='postgres:///sub2api_restore?sslmode=disable' \
//	  go test -tags=unit ./internal/repository/ -run TestProdRehearsal_DecryptsAfterPlaintextColumnsDropped -count=1 -v
func TestProdRehearsal_DecryptsAfterPlaintextColumnsDropped(t *testing.T) {
	dsn := os.Getenv("SUB2API_PROD_REHEARSAL_DSN")
	if dsn == "" {
		t.Skip("需要 SUB2API_PROD_REHEARSAL_DSN（已 drop 明文列的生产备份恢复库）")
	}
	key := os.Getenv("TOTP_ENCRYPTION_KEY")
	require.Len(t, key, 64, "需要 TOTP_ENCRYPTION_KEY（64 位 hex）")
	raw, err := NewAESEncryptor(&config.Config{Totp: config.TotpConfig{EncryptionKey: key}})
	require.NoError(t, err)
	cryptor, ok := raw.(*AESEncryptor)
	require.True(t, ok)

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	// ── 1. 明文列必须真的不在了 ────────────────────────────────────────────
	for _, c := range []struct{ table, column string }{
		{"accounts", "credentials"},
		{"api_keys", "key"},
	} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = $1 AND column_name = $2`, c.table, c.column).Scan(&n))
		require.Zero(t, n, "%s.%s 仍存在 —— 这条测试要在 drop 之后跑，否则它证明不了最终形态",
			c.table, c.column)
	}

	// ── 2. 账号：密文必须解得开且是结构正确的 JSON ─────────────────────────
	rows, err := db.QueryContext(ctx, `
		SELECT id, credentials_encrypted FROM accounts
		WHERE credentials_encrypted IS NOT NULL ORDER BY id`)
	require.NoError(t, err)
	accounts, withAPIKey, withRefresh := 0, 0, 0
	for rows.Next() {
		var id int64
		var cipher string
		require.NoError(t, rows.Scan(&id, &cipher))
		dec, derr := cryptor.Decrypt(cipher)
		require.NoError(t, derr, "account %d 在 drop 之后解不开 = 这条线没有退路", id)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(dec), &m), "account %d 解出来不是 JSON 对象", id)
		require.NotEmpty(t, m, "account %d 解出来是空对象", id)
		if _, ok := m["api_key"]; ok {
			withAPIKey++
		}
		if _, ok := m["refresh_token"]; ok {
			withRefresh++
		}
		accounts++
	}
	require.NoError(t, rows.Close())
	require.NotZero(t, accounts, "一行都没查到就没有资格说验过")
	require.NotZero(t, withAPIKey+withRefresh,
		"解出来的账号里既没有 api_key 也没有 refresh_token —— 形状可能不对（写错字段？）")

	// ── 3. API key：密文必须解出非空明文 ───────────────────────────────────
	krows, err := db.QueryContext(ctx, `
		SELECT id, key_encrypted FROM api_keys
		WHERE key_encrypted IS NOT NULL ORDER BY id`)
	require.NoError(t, err)
	keys := 0
	for krows.Next() {
		var id int64
		var cipher string
		require.NoError(t, krows.Scan(&id, &cipher))
		dec, derr := cryptor.Decrypt(cipher)
		require.NoError(t, derr, "api key %d 在 drop 之后解不开", id)
		require.NotEmpty(t, dec, "api key %d 解出空串", id)
		keys++
	}
	require.NoError(t, krows.Close())
	require.NotZero(t, keys, "没有任何 key 密文就没资格说验过")

	t.Logf("drop 明文列之后仍可解：accounts %d 行（api_key %d / refresh_token %d）、api_keys %d 行",
		accounts, withAPIKey, withRefresh, keys)
}
