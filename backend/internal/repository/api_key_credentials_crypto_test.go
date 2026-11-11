package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"

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

// ── 3.5b：鉴权查找改按 key_hash ──────────────────────────────────────────────
//
// 这组测试守三条互补的语义，任何一条退化成"查不到"或"都能查到"都是事故。

func TestLookupAPIKey_HashPathFindsKey(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, _ := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-hash-hit", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	found, err := repo.GetByKeyForAuth(ctx, "sk-hash-hit")
	require.NoError(t, err)
	require.Equal(t, key.ID, found.ID)
	require.Equal(t, "sk-hash-hit", found.Key, "命中后返回体仍带明文（调用方要拿它做掩码/日志）")
}

func TestLookupAPIKey_WrongKeyNotFound(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, _ := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-right", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	_, err := repo.GetByKeyForAuth(ctx, "sk-wrong")
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound, "错 key 必须查不到")
}

// 指纹为 NULL 的行（回填窗口内）仍必须能认证到，且计数 +1。
func TestLookupAPIKey_FallsBackWhenHashMissing(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-legacy", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))
	// 模拟"回填还没跑到这一行"
	_, err := db.Exec(`UPDATE api_keys SET key_hash = NULL WHERE id = $1`, key.ID)
	require.NoError(t, err)

	resetAPIKeyPlaintextLookupFallbackCount()
	found, err := repo.GetByKeyForAuth(ctx, "sk-legacy")
	require.NoError(t, err)
	require.Equal(t, key.ID, found.ID)
	require.Equal(t, int64(1), APIKeyPlaintextLookupFallbackCount(), "回落必须可见")
}

// 权威源的顺序被翻转了：**密文 → 明文列**（drop 之后前者是唯一副本）。
// 所以"明文列被人改成别的值"不再等于"key 被轮换"——指纹和密文都还指着原值。
// 这条测试钉住这个顺序：用原 key 查必须通过，且不许退到明文回落。
func TestAuth_StalePlaintextColumnDoesNotBlockTheRealKey(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-victim", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	// 只改明文列（模拟人工改库/旧写入漂移）。指纹与密文仍指向 sk-victim。
	// 注意不能改成"两行共享同一指纹"——key_hash 上有唯一索引，那样根本写不进去。
	_, err := db.Exec(`UPDATE api_keys SET key = 'sk-rotated-out-of-band' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	resetAPIKeyPlaintextLookupFallbackCount()
	resetAPIKeyCiphertextMissingLookupCount()
	found, err := repo.GetByKeyForAuth(ctx, "sk-victim")
	require.NoError(t, err, "指纹与密文都指向 sk-victim，明文列漂移不应阻断真实 key")
	require.Equal(t, key.ID, found.ID)
	require.Equal(t, "sk-victim", found.Key, "返回的明文应当来自密文，而不是那列脏值")
	require.Zero(t, APIKeyPlaintextLookupFallbackCount(), "这条路径是命中指纹的，不该走明文回落")
}

// 把上一条反过来看：**用被写进明文列的那个值**去查，会发生什么。
//
// 指纹命中不了（哈希是原 key 的），于是落到"按明文列等值查"的回落分支 ——
// 它会命中并放行。也就是说：只要回落分支还活着，一条被人工改过明文列的行
// 就能让那个新值通过认证。
//
// 这不是复核分支的漏洞（复核只信任指纹+密文），而是**回落分支**的固有代价。
// 它正是阶段 3.4（摘回落）与阶段 5（drop 明文列）要消掉的东西：
// 明文列没了，回落分支就没有可查的列。所以这条测试是**特性化测试**：
// 它记录现在的行为，并在回落被摘掉时翻转成"查不到"。
func TestAuth_StalePlaintextColumnIsStillReachableViaFallback_WILL_FLIP_AT_STAGE3(t *testing.T) {
	const fallbackStillAlive = true // 阶段 3.4 摘掉回落时改成 false，本测试随即断言"查不到"

	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-victim", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))
	_, err := db.Exec(`UPDATE api_keys SET key = 'sk-rotated-out-of-band' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	resetAPIKeyPlaintextLookupFallbackCount()
	found, err := repo.GetByKeyForAuth(ctx, "sk-rotated-out-of-band")

	if fallbackStillAlive {
		require.NoError(t, err, "回落分支还在：明文列里的值能命中（这是要摘掉它的理由）")
		require.Equal(t, key.ID, found.ID)
		require.Equal(t, int64(1), APIKeyPlaintextLookupFallbackCount(), "这类命中必须计数可见")
	} else {
		require.ErrorIs(t, err, service.ErrAPIKeyNotFound,
			"回落已摘：只有指纹+密文能认证，明文列里的脏值查不到")
	}
}

// ── 阶段四前置：复核的明文必须来自密文，而不是明文列 ────────────────────────
//
// drop 明文列之后 `key_encrypted` 是唯一副本。若复核仍读 `key` 列：
//   - drop 当天：`column "key" does not exist`（ent 会先炸，属可见故障）；
//   - 更糟的改法：读不到就当空 → 每行都"明文不等" → 全站 401。
// 所以这组测试在**列还在的时候**就把最终形态钉住：把 `key` 清成空串模拟 drop，
// 鉴权与取 key 都必须照常工作。

func TestAuth_UsesCiphertextWhenPlaintextColumnIsGone(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-post-drop", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	// 模拟 drop：明文列清空，指纹与密文保持不动（drop 后就是这个状态）
	_, err := db.Exec(`UPDATE api_keys SET key = '' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	resetAPIKeyCiphertextMissingLookupCount()
	found, err := repo.GetByKeyForAuth(ctx, "sk-post-drop")
	require.NoError(t, err, "明文列没了也必须能认证——drop 之后只剩密文")
	require.Equal(t, key.ID, found.ID)
	require.Equal(t, "sk-post-drop", found.Key, "返回体的 Key 必须是解密出来的明文，掩码/日志要用")
	require.Zero(t, APIKeyCiphertextMissingLookupCount(), "这一行有密文，不该走缺密文兜底")

	// 取 key 的旁路（删除路径用）同样不能依赖明文列
	got, owner, err := repo.GetKeyAndOwnerID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, "sk-post-drop", got)
	require.Equal(t, int64(1), owner)
}

// 负例一：密文解出来的东西与入参不符 → 拒绝。
// 这条防"复核不小心比的是列里的旧明文"：此例里列已被清空，
// 只有真正拿密文比对才会判不等。
func TestAuth_RejectsWhenCiphertextDisagreesWithInput(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-original", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	other, err := cryptor.Encrypt("sk-someone-else")
	require.NoError(t, err)
	// 指纹仍指向 sk-original（所以定位能命中），但密文里装的是别的 key
	_, err = db.Exec(`UPDATE api_keys SET key = '', key_encrypted = $1 WHERE id = $2`, other, key.ID)
	require.NoError(t, err)

	_, lookupErr := repo.GetByKeyForAuth(ctx, "sk-original")
	require.ErrorIs(t, lookupErr, service.ErrAPIKeyNotFound,
		"密文与入参不符必须拒绝——指纹只是索引，不是凭据")
}

// 负例二：密文坏到解不开 → 硬错误，且**不许**被当成"没有密文"退回明文列。
// 否则一条损坏行会在回落里活下来，把"数据坏了"伪装成"还没回填"。
func TestAuth_DecryptFailureIsNotTreatedAsMissingCiphertext(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-corrupt", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))

	// 明文列**保留**成正确值，只把密文弄坏。若实现"解不开就退回明文列"，这条会误放行。
	_, err := db.Exec(`UPDATE api_keys SET key_encrypted = 'bm90LWEtY2lwaGVydGV4dA==' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	resetAPIKeyCiphertextMissingLookupCount()
	_, lookupErr := repo.GetByKeyForAuth(ctx, "sk-corrupt")
	require.ErrorIs(t, lookupErr, service.ErrAPIKeyNotFound,
		"密文解不开必须判查不到（fail closed），不能退回明文列凑答案")
	require.Zero(t, APIKeyCiphertextMissingLookupCount(),
		"解不开不等于缺失：不许计进缺密文兜底，否则坏了会被当成没回填")
}

// 非鉴权的读路径（详情/列表/配额状态）同样不能依赖明文列。
//
// 这些接口返回的 `Key` 会被掩码显示、也会被拿去算缓存键，所以 drop 之后必须能从
// 密文解出来。把明文列清空＝模拟 drop，三条路径都要照常返回明文。
func TestReadPaths_ReturnPlaintextWithoutPlaintextColumn(t *testing.T) {
	cryptor := credentialTestEncryptor(t)
	repo, db := apiKeyScratchRepo(t, cryptor)
	ctx := context.Background()

	key := &service.APIKey{UserID: 1, Key: "sk-readpath", Name: "k", Status: service.StatusActive, Quota: 5}
	require.NoError(t, repo.Create(ctx, key))
	require.NoError(t, repo.Update(ctx, key, service.APIKeyUpdateFields{Quota: true}))

	// 模拟 drop：明文列清空
	_, err := db.Exec(`UPDATE api_keys SET key = '' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, "sk-readpath", got.Key, "详情路径的 Key 必须来自解密")

	list, _, err := repo.ListByUserID(ctx, key.UserID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "sk-readpath", list[0].Key, "列表路径的 Key 也必须来自解密，否则掩码会显示空")

	// 配额状态里的 Key 直接喂给 InvalidateAuthCacheByKey（缓存键 = sha256(明文)）：
	// 取不到明文会让"配额耗尽"的缓存失效被静默跳过。
	state, err := repo.IncrementQuotaUsedAndGetState(ctx, key.ID, 1)
	require.NoError(t, err)
	require.Equal(t, "sk-readpath", state.Key, "配额状态的 Key 必须来自解密，否则缓存失效会被静默跳过")
	require.Equal(t, sha256Hex([]byte("sk-readpath")), sha256Hex([]byte(state.Key)),
		"喂给缓存失效的必须是明文（缓存键是 sha256(明文)）")
}
