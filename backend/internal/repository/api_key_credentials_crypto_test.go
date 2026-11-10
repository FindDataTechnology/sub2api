package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// api_keys 派生列的测试（阶段三 3.5a）。
//
// 为什么这条必须有：3.5b 会把鉴权改成"按 key_hash 查"，而回填只补存量。
// 若 Create 不写指纹，新发的 key 在切换那一刻**立刻查不到**，且没有任何报错——
// 症状是"新建的 key 一直 401"。

func TestAPIKeyDerivedColumns(t *testing.T) {
	cryptor := credentialTestEncryptor(t)

	t.Run("空 key 不产生任何派生值", func(t *testing.T) {
		hash, encrypted, err := apiKeyDerivedColumns("", cryptor)
		require.NoError(t, err)
		require.Empty(t, hash, "空串哈希必须为空——key_hash 是部分唯一索引，写空串第二行就撞约束")
		require.Nil(t, encrypted)
	})

	t.Run("无加密器时只出指纹", func(t *testing.T) {
		hash, encrypted, err := apiKeyDerivedColumns("sk-cust-1", nil)
		require.NoError(t, err)
		require.Equal(t, sha256Hex([]byte("sk-cust-1")), hash)
		require.Nil(t, encrypted, "无加密器时密文必须是 nil（写 NULL），不能是空串")
	})

	t.Run("有加密器时密文可解回原文", func(t *testing.T) {
		hash, encrypted, err := apiKeyDerivedColumns("sk-cust-2", cryptor)
		require.NoError(t, err)
		require.Equal(t, sha256Hex([]byte("sk-cust-2")), hash)
		require.NotNil(t, encrypted)
		plaintext, err := cryptor.Decrypt(*encrypted)
		require.NoError(t, err)
		require.Equal(t, "sk-cust-2", plaintext)
	})

	t.Run("同一 key 两次加密密文不同但指纹相同", func(t *testing.T) {
		h1, e1, err := apiKeyDerivedColumns("sk-same", cryptor)
		require.NoError(t, err)
		h2, e2, err := apiKeyDerivedColumns("sk-same", cryptor)
		require.NoError(t, err)
		require.Equal(t, h1, h2, "指纹必须稳定，否则按 hash 查找会时灵时不灵")
		require.NotEqual(t, *e1, *e2, "AES-GCM 带随机 nonce：密文不能用于等值比较")
	})
}

// apiKeyScratchRepo 在一次性真库上建**完整** api_keys schema（用 ent 自己的 migrate），
// 返回 repo 与 *sql.DB。无 DSN 时跳过。
func apiKeyScratchRepo(t *testing.T, cryptor service.SecretEncryptor) (*apiKeyRepository, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；api_keys 派生列验证需要真 PostgreSQL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx), "用 ent 自己的 migrate 建表，避免测试里手写 schema 漂移")

	// api_keys.user_id 有外键指向 users，先造一个最小用户
	_, err = client.User.Create().
		SetEmail("apikey-drill@example.test").
		SetPasswordHash("x").
		SetUsername("apikey-drill").
		Save(ctx)
	if err != nil {
		// users 的必填列随 schema 演进会变；失败时把真实原因带出来，别吞成"测试挂了"
		t.Fatalf("seed user: %v", err)
	}

	return newAPIKeyRepositoryWithSQLCrypto(client, db, cryptor), db
}

func TestAPIKeyRepository_CreateWritesDerivedColumns(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-new-1", Name: "k1", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	var hash string
	var encrypted *string
	require.NoError(t, db.QueryRow(`SELECT key_hash, key_encrypted FROM api_keys WHERE id = $1`, key.ID).
		Scan(&hash, &encrypted))
	require.Equal(t, sha256Hex([]byte("sk-new-1")), hash, "新 key 必须落指纹，否则 3.5b 切换后查不到")
	require.NotNil(t, encrypted)
	plaintext, err := cryptor.Decrypt(*encrypted)
	require.NoError(t, err)
	require.Equal(t, "sk-new-1", plaintext)

	// 新 key 的指纹必须能被查回来（模拟 3.5b 的查找方式）
	var found int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM api_keys WHERE key_hash = $1`, hash).Scan(&found))
	require.Equal(t, 1, found)
}

func TestAPIKeyRepository_DeleteRewritesDerivedColumns(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-to-delete", Name: "k1", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	originalHash := sha256Hex([]byte("sk-to-delete"))
	var before int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM api_keys WHERE key_hash = $1`, originalHash).Scan(&before))
	require.Equal(t, 1, before)

	require.NoError(t, repo.Delete(ctx, key.ID))

	// 关键断言：删除后**原 key 的指纹必须不再命中**。
	// tombstone 改写了明文 key，若指纹不跟着改，等于"已删除的 key 仍能被 key_hash 找到"。
	var after int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM api_keys WHERE key_hash = $1`, originalHash).Scan(&after))
	require.Zero(t, after, "tombstone 后旧指纹 MUST 失效")

	// tombstone 行本身仍应有指纹（指向 tombstone 明文），且密文可解回 tombstone
	var tombstoneHash string
	var tombstoneEncrypted *string
	require.NoError(t, db.QueryRow(`SELECT key_hash, key_encrypted FROM api_keys WHERE id = $1`, key.ID).
		Scan(&tombstoneHash, &tombstoneEncrypted))
	require.NotEmpty(t, tombstoneHash)
	require.NotEqual(t, originalHash, tombstoneHash)
	require.NotNil(t, tombstoneEncrypted)
	decrypted, err := cryptor.Decrypt(*tombstoneEncrypted)
	require.NoError(t, err)
	require.Contains(t, decrypted, "__deleted__")
}
