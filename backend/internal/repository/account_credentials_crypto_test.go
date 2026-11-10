package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// credentialTestEncryptor 在本文件内自建加密器，而不是复用 aes_encryptor_test.go 的
// helper —— 那个文件带 `//go:build unit` 标签，复用会让本文件的测试也被迫只在
// 携带 unit tag 时才编译，等于默认测试跑不到。
func credentialTestEncryptor(t *testing.T) *AESEncryptor {
	t.Helper()
	enc, err := NewAESEncryptor(&config.Config{
		Totp: config.TotpConfig{EncryptionKey: strings.Repeat("42", 32)},
	})
	require.NoError(t, err)
	concrete, ok := enc.(*AESEncryptor)
	require.True(t, ok)
	return concrete
}

// 凭据落盘加密（阶段一）的派生列测试。
// 见 openspec/changes/harden-sub2api-credential-storage/design.md D2 / D7。

func sha256OfJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(v)
	require.NoError(t, err)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// TestCredentialMetaSubset_OnlyWhitelistedKeys 白名单是"默认保密、显式放行"，
// 所以任何没被显式列入的非密钥子键也 MUST NOT 进明文列。
func TestCredentialMetaSubset_OnlyWhitelistedKeys(t *testing.T) {
	creds := map[string]any{
		"base_url":     "https://api.example.com",
		"account_mode": "zen",
		// 下面这些都不是白名单：即使不含密钥也不许进明文列
		"model_mapping": map[string]any{"a": "b"},
		"region":        "cn",
		"api_protocol":  "openai",
		// 密钥类：绝对不进
		"api_key":       "sk-secret",
		"secret_id":     "sid",
		"refresh_token": "rt",
	}
	got := credentialMetaSubset(creds)

	// has_refresh_token 是**派生事实**（该凭据带 refresh_token: "rt"），
	// 不属于"从凭据复制的子键"，但同样要出现在 meta 里。
	require.Equal(t, map[string]any{
		"base_url":          "https://api.example.com",
		"account_mode":      "zen",
		"has_refresh_token": true,
	}, got)

	// 反向断言：白名单外的键一个都不许出现（用逐个检查，避免将来白名单扩大时
	// 上面的 Equal 被顺手改宽而无人注意）
	for _, forbidden := range []string{"api_key", "secret_id", "refresh_token", "model_mapping", "region", "api_protocol"} {
		_, present := got[forbidden]
		require.False(t, present, "%s 不该进 credentials_meta 明文列", forbidden)
	}
}

// TestCredentialMetaSubset_AlwaysCarriesDerivedFlag：即使凭据里没有任何白名单子键，
// meta 也必须带派生事实，且不能返回 nil（显式 NULL 会违反非空约束）。
func TestCredentialMetaSubset_AlwaysCarriesDerivedFlag(t *testing.T) {
	got := credentialMetaSubset(map[string]any{"api_key": "sk-x"})
	require.NotNil(t, got)
	// 不含任何凭据子键（api_key 是密钥、不许进明文列），但**始终**带派生事实。
	// 列是 NOT NULL DEFAULT '{}'，返回 nil 会把整份 meta 清空。
	require.Equal(t, map[string]any{"has_refresh_token": false}, got)
}

// TestCredentialHash_IsKeyOrderIndependent 乐观并发比较依赖"内容相同 ⇒ 哈希相同"。
// Go 的 map 遍历顺序随机，所以这条测试必须多跑几轮才有意义——单轮可能碰巧相同。
func TestCredentialHash_IsKeyOrderIndependent(t *testing.T) {
	canonical, err := canonicalCredentialJSON(map[string]any{
		"b": 2, "a": 1, "c": map[string]any{"z": 26, "y": 25},
	})
	require.NoError(t, err)

	for i := 0; i < 50; i++ {
		again, err := canonicalCredentialJSON(map[string]any{
			"c": map[string]any{"y": 25, "z": 26}, "a": 1, "b": 2,
		})
		require.NoError(t, err)
		require.Equal(t, string(canonical), string(again), "同一内容不同键序必须产出相同规范 JSON")
	}

	// 内容不同必须不同哈希——否则乐观并发会把"已改"判成"没改"
	other, err := canonicalCredentialJSON(map[string]any{"a": 1, "b": 3})
	require.NoError(t, err)
	require.NotEqual(t, sha256Hex(canonical), sha256Hex(other))
}

func TestCanonicalCredentialJSON_NilIsEmptyObject(t *testing.T) {
	got, err := canonicalCredentialJSON(nil)
	require.NoError(t, err)
	require.Equal(t, "{}", string(got))
}

func TestCredentialAPIKeyHash_PresentAndAbsent(t *testing.T) {
	present := credentialAPIKeyHash(map[string]any{"api_key": "sk-abc"})
	require.NotNil(t, present)
	expected := sha256.Sum256([]byte("sk-abc"))
	require.Equal(t, hex.EncodeToString(expected[:]), *present)

	// 空串、类型不对、没有这个键 —— 三种都必须返回 nil（列写 NULL）
	require.Nil(t, credentialAPIKeyHash(map[string]any{"api_key": ""}))
	require.Nil(t, credentialAPIKeyHash(map[string]any{"api_key": 123}))
	require.Nil(t, credentialAPIKeyHash(map[string]any{"refresh_token": "rt"}))
	require.Nil(t, credentialAPIKeyHash(nil))
}

// TestCredentialCryptoColumnsFor_NilCryptorStillWritesDerivedColumns
//
// cryptor 为 nil 时只跳过**密文**列：meta 与两个指纹都不需要密钥，
// 提前写入能让阶段二的 SQL 改写有数据可依，也让"未迁移"的含义收窄到只有密文列。
func TestCredentialCryptoColumnsFor_NilCryptorStillWritesDerivedColumns(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil)
	creds := map[string]any{"api_key": "sk-abc", "base_url": "https://x", "refresh_token": "rt"}

	columns, err := repo.credentialCryptoColumnsFor(creds)
	require.NoError(t, err)

	require.Nil(t, columns.Encrypted, "无加密器时密文列必须写 NULL，不能写空串")
	require.NotNil(t, columns.Hash)
	require.Equal(t, sha256OfJSON(t, creds), *columns.Hash)
	require.NotNil(t, columns.APIKeyHash)
	require.Equal(t, map[string]any{"base_url": "https://x", "has_refresh_token": true}, columns.Meta)
}

// TestCredentialCryptoColumnsFor_WithCryptorRoundTrips 有加密器时必须真加密，
// 且密文能解回同一份规范化 JSON。
func TestCredentialCryptoColumnsFor_WithCryptorRoundTrips(t *testing.T) {
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)
	creds := map[string]any{"api_key": "sk-abc", "refresh_token": "rt"}

	columns, err := repo.credentialCryptoColumnsFor(creds)
	require.NoError(t, err)
	require.NotNil(t, columns.Encrypted)
	require.NotEmpty(t, *columns.Encrypted)

	// 密文里 MUST 看不到明文密钥
	require.NotContains(t, *columns.Encrypted, "sk-abc")

	plaintext, err := encryptor.Decrypt(*columns.Encrypted)
	require.NoError(t, err)
	require.JSONEq(t, `{"api_key":"sk-abc","refresh_token":"rt"}`, plaintext)
}

// TestCredentialCryptoColumnsFor_CiphertextIsNonDeterministic
//
// 这是 D2 里那个"必须改用哈希比较"的理由本身：AES-GCM 带随机 nonce，
// 同一明文两次加密的密文不同，所以密文绝不能用于等值比较。
// 若哪天有人把加密改成确定性实现，这条会变红——那是需要重新审视 D2 的信号。
func TestCredentialCryptoColumnsFor_CiphertextIsNonDeterministic(t *testing.T) {
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)
	creds := map[string]any{"api_key": "sk-same"}

	first, err := repo.credentialCryptoColumnsFor(creds)
	require.NoError(t, err)
	second, err := repo.credentialCryptoColumnsFor(creds)
	require.NoError(t, err)

	require.NotEqual(t, *first.Encrypted, *second.Encrypted, "同一明文两次加密的密文必须不同")
	require.Equal(t, *first.Hash, *second.Hash, "但哈希必须相同——否则等值比较无从谈起")
}

// TestCredentialHash_PinnedToKnownValue 把哈希算法钉死。
//
// 换算法（比如 blake2）会让所有已落库的 hash 与自己不再匹配，那是必须显式决策的
// 数据迁移，不能悄悄发生——这条测试就是那个"必须显式"的闸门。
// 期望值由 `printf '%s' '{"api_key":"sk-abc"}' | shasum -a 256` 独立算出。
func TestCredentialHash_PinnedToKnownValue(t *testing.T) {
	canonical, err := canonicalCredentialJSON(map[string]any{"api_key": "sk-abc"})
	require.NoError(t, err)
	require.Equal(t, `{"api_key":"sk-abc"}`, string(canonical))
	require.Equal(t, "935f9c4f77cb9d7dae4860c698582b62add05d911282e1a04c33bfd8f97f863b", sha256Hex(canonical))
}

// credentialHashOfJSONString 让 sqlmock 的期望值也能用生产的哈希实现算：
// 传 JSON 字面量，内部解析成 map 后走 canonicalCredentialJSON + sha256Hex。
// 这样测试期望与实现同源——手抄一个 hash 常量会在实现变更时静默失配。
func credentialHashOfJSONString(raw string) string {
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		panic("credentialHashOfJSONString: 测试里的 JSON 字面量不合法: " + err.Error())
	}
	return credentialHashHex(decoded)
}

// 站点 B 的等价改写测试（见 REPORT-refresh-token-presence.md §1）。
//
// 原 SQL 在 `credentials_hash = sha256(入参)` 之外还要求"行上的 refresh_token 为空"。
// 前者成立时后者是入参的纯函数，所以可以在 Go 侧提前返回。这组测试守两件事：
// 判定本身的语义与 SQL 逐字等价，以及提前返回时**一次 SQL 都不发**。

func TestCredentialHasRefreshToken_MatchesSQLSemantics(t *testing.T) {
	cases := []struct {
		name string
		cred map[string]any
		want bool
	}{
		{"键缺失", map[string]any{"access_token": "a"}, false},
		{"值为 JSON null", map[string]any{"refresh_token": nil}, false},
		{"空串", map[string]any{"refresh_token": ""}, false},
		{"只有空白", map[string]any{"refresh_token": " \t\n "}, false},
		{"正常值", map[string]any{"refresh_token": "rt-1"}, true},
		// 下面两类是 SQL 侧 `->>` 会把非字符串值转成非空文本的情形。
		// 若这里判 false，Go 与 SQL 会给出相反结论——表现为"有些账号能刷新、有些不能"。
		{"数字", map[string]any{"refresh_token": 123}, true},
		{"布尔", map[string]any{"refresh_token": false}, true},
		{"空对象", map[string]any{"refresh_token": map[string]any{}}, true},
		{"空数组", map[string]any{"refresh_token": []any{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, credentialHasRefreshToken(tc.cred))
		})
	}
}

// 入参带非空 refresh_token 时：必须直接返回 (false, nil)，且**不发任何 SQL**。
// 这正是原 SQL 里那条谓词恒假、影响 0 行的等价表现。
func TestSetGrokOAuthErrorIfCredentialsUnchanged_NonEmptyRefreshTokenShortCircuits(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"refresh_token": "rt-still-valid", "_token_version": int64(3)},
		"boom",
	)
	require.NoError(t, err)
	require.False(t, applied, "refresh_token 非空时该变更不适用，与原 SQL 0 行受影响一致")
	require.Empty(t, exec.execQueries, "必须一次 SQL 都不发（提前返回）")
}

// 反向保证：入参 refresh_token 为空时，仍然照常发 SQL。
// 没有这条，"永远不发 SQL" 的错误实现也会让上面那条测试绿。
func TestSetGrokOAuthErrorIfCredentialsUnchanged_EmptyRefreshTokenStillRunsSQL(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	applied, err := repo.SetGrokOAuthErrorIfCredentialsUnchanged(
		context.Background(),
		42,
		map[string]any{"access_token": "a", "_token_version": int64(3)},
		"boom",
	)
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1, "入参没有 refresh_token 时行为必须与改动前一致")
}

// TestListOAuthRefreshCandidatePage_UsesMetaDerivedFlag（站点 A 方案 ①）
//
// 这条只证明"SQL 里确实换成了读 meta 的派生事实"——差分仪器证明的是"语义没变"，
// 两者缺一不可：只跑差分的话，代码完全没改也会绿。
func TestListOAuthRefreshCandidatePage_UsesMetaDerivedFlag(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	exec.queryRows = emptyQueryRowsMatching(t, "(?s)SELECT")
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, _ = repo.ListOAuthRefreshCandidatePage(context.Background(), service.OAuthRefreshPageOptions{
		Platforms:           []string{service.PlatformGrok},
		Limit:               10,
		RequireRefreshToken: true,
	})

	require.NotEmpty(t, exec.queryQueries, "应当发出一次候选查询")
	query := normalizeSQLWhitespace(exec.queryQueries[0])
	require.Contains(t, query, `credentials_meta @> '{"has_refresh_token": true}'`)
	// 反向锁：不许再残留**对 credentials 列**的 refresh_token 子键提取。
	// 注意不能直接断言 "不含 refresh_token"：新的 meta key 名字里也带这个词
	// （'{"has_refresh_token": true}'），断太宽会误伤自己。
	require.NotContains(t, query, "credentials ? 'refresh_token'")
	require.NotContains(t, query, "credentials->>'refresh_token'")
}

// emptyQueryRowsMatching 让 recordingSQLExecutor 的"回读/查询"返回空结果集，
// 同时按 pattern 校验发出的 SQL（sqlmock 的默认 regexp matcher）。
func emptyQueryRowsMatching(t *testing.T, pattern string) func(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery(pattern).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	return func(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
		return db.QueryContext(ctx, query, args...)
	}
}
