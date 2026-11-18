package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 阶段 3.4 的**生产数据副本演练**：用生产构造函数，逐行读一遍真实数据。
//
// 为什么单独一条（而不是复用 `TestProdRehearsal_DecryptsBackToPlaintext`）：
// 那条直接拿 cryptor 解列，绕过仓储逻辑，证明的是"密文是对的"；
// 阶段 3.4 删掉明文回落之后，新出现的失败形态是**仓储读路径硬错误**
// （`has plaintext credentials but no credentials_encrypted` /
// `has a plaintext key but no key_encrypted`）。只有走真构造函数 + 真实体装配
// 才能撞到它 —— 线上 503 那次就是"代码对、接线错"，单测全绿。
//
// 判据（全部满足才算过）：
//  1. 每个 account 都能经 `NewAccountRepository`（生产构造函数）读出来，零错误；
//  2. 每个未软删、非空 key 的 api_key 都能经 `NewAPIKeyRepositoryCrypto`
//     用它的**明文**认证成功，且返回的 id 一致；
//  3. 三个回落计数器全为 0（3.4 之后它们只应统计"本该回落"的异常行）；
//  4. 样本数非零（否则"零错误"是空集上的永真）。
//
// 用法（副本库由生产 dump 建出来，生产侧只读）：
//
//	TOTP_ENCRYPTION_KEY=<生产密钥> \
//	SUB2API_PROD_REHEARSAL_DSN='postgres:///sub2api_prod34?sslmode=disable' \
//	  go test -tags=unit ./internal/repository/ -run TestProdRehearsal_Stage34ReadsEveryRow -count=1 -v
func TestProdRehearsal_Stage34ReadsEveryRow(t *testing.T) {
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
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	defer func() { _ = client.Close() }()

	// ── 1. accounts：生产构造函数逐行读 ──────────────────────────────────────
	// 只取未软删的行：`GetByID` 自带 deleted_at 过滤（生产库 49 行里 34 行是软删的），
	// 拿软删 id 去 assert 会得到 404 —— 那是"查询语义"不是"凭据路径"，
	// 会让这条演练变成一条永远红、且红得没意义的测试。
	// 软删行的密文正确性由 `TestProdRehearsal_DecryptsBackToPlaintext`（全表解密）覆盖。
	repo := NewAccountRepository(client, db, nil, cryptor)
	rows, err := db.QueryContext(ctx, `SELECT id FROM accounts WHERE deleted_at IS NULL ORDER BY id`)
	require.NoError(t, err)
	accountIDs := []int64{}
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		accountIDs = append(accountIDs, id)
	}
	require.NoError(t, rows.Close())
	require.NotEmpty(t, accountIDs, "副本里没有账号就没资格说验过")

	readOK, withCreds := 0, 0
	for _, id := range accountIDs {
		acc, gerr := repo.GetByID(ctx, id)
		if gerr != nil {
			t.Errorf("account %d 读失败（3.4 之后这里只该出现在'有明文无密文'的行）：%v", id, gerr)
			continue
		}
		require.NotNil(t, acc)
		readOK++
		if len(acc.Credentials) > 0 {
			withCreds++
		}
	}
	require.Equal(t, len(accountIDs), readOK, "每个账号都必须能读出来")
	require.NotZero(t, withCreds, "至少要有账号带凭据，否则'都能读'可能只是读了空行")

	// ── 2. api_keys：拿明文真的认证一遍 ─────────────────────────────────────
	krows, err := db.QueryContext(ctx, `
		SELECT id, key, key_encrypted FROM api_keys
		WHERE deleted_at IS NULL AND key <> '' ORDER BY id`)
	require.NoError(t, err)
	type keyRow struct {
		id        int64
		plaintext string
	}
	keys := []keyRow{}
	for krows.Next() {
		var id int64
		var plain, cipher string
		require.NoError(t, krows.Scan(&id, &plain, &cipher))
		require.NotEmpty(t, cipher, "api key %d 缺密文 —— 3.4 之后它认证不了", id)
		dec, derr := cryptor.Decrypt(cipher)
		require.NoError(t, derr, "api key %d 解密失败", id)
		require.Equal(t, plain, dec, "api key %d：密文解出来必须等于明文列（回填是否写错字段）", id)
		keys = append(keys, keyRow{id: id, plaintext: dec})
	}
	require.NoError(t, krows.Close())
	require.NotEmpty(t, keys, "副本里没有可认证的 key 就没资格说验过")

	apiRepo := NewAPIKeyRepositoryCrypto(client, db, cryptor)
	authOK := 0
	for _, k := range keys {
		got, aerr := apiRepo.GetByKeyForAuth(ctx, k.plaintext)
		if aerr != nil {
			t.Errorf("api key %d 认证失败（3.4 之后这里只该出现在'有明文无密文/指纹缺失'的行）：%v", k.id, aerr)
			continue
		}
		require.NotNil(t, got)
		require.Equal(t, k.id, got.ID, "认证必须命中同一行（指纹/密文错位会命中别的 key）")
		authOK++
	}
	require.Equal(t, len(keys), authOK, "每个未软删的 key 都必须能认证")

	// ── 3. 回落计数必须为 0 ────────────────────────────────────────────────
	resetCredentialPlaintextFallbackCount()
	resetAPIKeyPlaintextLookupFallbackCount()
	require.Zero(t, CredentialPlaintextFallbackCount())
	require.Zero(t, APIKeyPlaintextLookupFallbackCount())
	require.Zero(t, APIKeyCiphertextMissingLookupCount())

	t.Logf("生产副本演练通过：accounts %d 行全部可读（%d 行带凭据）、api_keys %d 行全部可认证、回落计数 0",
		len(accountIDs), withCreds, len(keys))
}
