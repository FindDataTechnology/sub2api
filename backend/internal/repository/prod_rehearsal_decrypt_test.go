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

// 生产数据副本上的**解密回读**校验：密文解出来必须与明文列逐字节一致。
//
// 为什么单独一条：回填只证明"写进去了"，判据只证明"派生列不空了"，
// 两者都不能证明"写进去的密文是对的"——用错密钥、错编码、错字段，
// 三种情况下 A/B/C/D 依然全为 0。只有真解一遍才算验完。
//
// 用法（两步都在本机，生产只读）：
//
//	# 1. 取生产快照（只读）：schema 全量 + accounts/api_keys/users/schema_migrations 数据
//	kubectl -n sub2api port-forward svc/sub2api-postgres 15432:5432 &
//	PGOPTIONS='-c default_transaction_read_only=on' pg_dump -Fc --schema-only \
//	  'postgres://sub2api@127.0.0.1:15432/sub2api?sslmode=disable' -f /tmp/pr/schema.dump
//	PGOPTIONS='-c default_transaction_read_only=on' pg_dump -Fc --data-only \
//	  -t accounts -t api_keys -t users -t schema_migrations \
//	  'postgres://sub2api@127.0.0.1:15432/sub2api?sslmode=disable' -f /tmp/pr/data.dump
//	createdb sub2api_prod_r && pg_restore -d sub2api_prod_r --no-owner --no-privileges /tmp/pr/schema.dump
//	pg_restore -d sub2api_prod_r --data-only --disable-triggers -t accounts -t api_keys /tmp/pr/data.dump
//
//	# 2. 应用 249/250 → 回填 → 本测试（密钥只进环境变量，不落盘不打印）
//	TOTP_ENCRYPTION_KEY=$(kubectl -n sub2api get secret sub2api-secrets \
//	  -o jsonpath='{.data.TOTP_ENCRYPTION_KEY}' | base64 -d) \
//	SUB2API_PROD_REHEARSAL_DSN='postgres:///sub2api_prod_r?sslmode=disable' \
//	  go test ./internal/repository/ -run TestProdRehearsal_DecryptsBackToPlaintext -count=1 -v
//
// 两个环境变量缺一即 skip，所以它不会污染常规跑批。
func TestProdRehearsal_DecryptsBackToPlaintext(t *testing.T) {
	dsn := os.Getenv("SUB2API_PROD_REHEARSAL_DSN")
	if dsn == "" {
		t.Skip("需要 SUB2API_PROD_REHEARSAL_DSN（生产数据副本）")
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

	// accounts：JSON 比较用规范化后的 Marshal，避免键序/空格造成假不一致。
	rows, err := db.QueryContext(ctx, `
		SELECT id, credentials, credentials_encrypted
		FROM accounts WHERE credentials_encrypted IS NOT NULL`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	checked, mismatch := 0, 0
	for rows.Next() {
		var id int64
		var plain []byte
		var cipher string
		require.NoError(t, rows.Scan(&id, &plain, &cipher))
		dec, err := cryptor.Decrypt(cipher)
		require.NoError(t, err, "account %d 解密失败（密钥不对或密文损坏）", id)
		var a, b map[string]any
		require.NoError(t, json.Unmarshal(plain, &a))
		require.NoError(t, json.Unmarshal([]byte(dec), &b))
		aj, err := json.Marshal(a)
		require.NoError(t, err)
		bj, err := json.Marshal(b)
		require.NoError(t, err)
		if string(aj) != string(bj) {
			mismatch++
			t.Logf("account %d：解密结果与明文列不一致", id)
		}
		checked++
	}
	require.NoError(t, rows.Err())
	t.Logf("accounts 校验 %d 行，不一致 %d 行", checked, mismatch)
	require.NotZero(t, checked, "一行都没查到就没有资格说验过")
	require.Zero(t, mismatch)

	krows, err := db.QueryContext(ctx, `SELECT id, key, key_encrypted FROM api_keys WHERE key_encrypted IS NOT NULL`)
	require.NoError(t, err)
	defer func() { _ = krows.Close() }()
	kchecked, kmismatch := 0, 0
	for krows.Next() {
		var id int64
		var plain, cipher string
		require.NoError(t, krows.Scan(&id, &plain, &cipher))
		dec, err := cryptor.Decrypt(cipher)
		require.NoError(t, err, "api_key %d 解密失败", id)
		if dec != plain {
			kmismatch++
			t.Logf("api_key %d：解密结果与明文列不一致", id)
		}
		kchecked++
	}
	require.NoError(t, krows.Err())
	t.Logf("api_keys 校验 %d 行，不一致 %d 行", kchecked, kmismatch)
	require.NotZero(t, kchecked)
	require.Zero(t, kmismatch)
}
