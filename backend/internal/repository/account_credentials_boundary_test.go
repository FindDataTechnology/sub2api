package repository

import (
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// 边界解密（design D1 读侧）与回落计数的测试。
//
// 这组测试守的是三条互斥的读法，任何一条退化成"静默返回空"都会让线上以
// "上游 401 / 该账号没配密钥"的形态暴露，而真因查不到。

func strPtr(s string) *string { return &s }

// 1) 有密文 -> 解密，绝不读明文列。
// 这里特意把明文列塞成一份**不同的**内容：如果实现偷偷回落明文，断言会红。
func TestCredentialsFromEntity_PrefersCiphertextOverPlaintext(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)

	ciphertext, err := encryptor.Encrypt(`{"api_key":"sk-encrypted-truth"}`)
	require.NoError(t, err)

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   7,
		Credentials:          map[string]any{"api_key": "sk-stale-plaintext"},
		CredentialsEncrypted: strPtr(ciphertext),
	})
	require.NoError(t, err)
	require.Equal(t, "sk-encrypted-truth", got["api_key"])
	require.Equal(t, int64(0), CredentialPlaintextFallbackCount(), "走了密文就不该计回落")
}

// 2) 无密文 -> 回落明文 + 计数。
func TestCredentialsFromEntity_FallsBackToPlaintextAndCounts(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // cryptor = nil

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:          8,
		Credentials: map[string]any{"api_key": "sk-legacy"},
	})
	require.NoError(t, err)
	require.Equal(t, "sk-legacy", got["api_key"])
	require.Equal(t, int64(1), CredentialPlaintextFallbackCount())
}

// 3) 空密文串等同于"没有密文"——空串不是有效密文，不能拿去解密。
func TestCredentialsFromEntity_EmptyCiphertextCountsAsMissing(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   9,
		Credentials:          map[string]any{"api_key": "sk-legacy"},
		CredentialsEncrypted: strPtr(""),
	})
	require.NoError(t, err)
	require.Equal(t, "sk-legacy", got["api_key"])
	require.Equal(t, int64(1), CredentialPlaintextFallbackCount())
}

// 4) D1a 的负例：有密文但没配加密器 -> **必须报错**。
//
// 这条是本 change 里最重要的一条负例。若实现"没加密器就回落明文"，
// 线上会把 base64 密文当 API key 发给上游，得到的是"上游 401"，
// 而真正的病因（加密器没配上）完全看不见。
func TestCredentialsFromEntity_RefusesCiphertextWithoutEncryptor(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // cryptor = nil

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   10,
		Credentials:          map[string]any{"api_key": "sk-legacy"},
		CredentialsEncrypted: strPtr("b64-ciphertext"),
	})
	require.Error(t, err, "有密文却没有加密器时必须报错，不能回落明文")
	require.Nil(t, got)
	require.Contains(t, err.Error(), "refusing to treat ciphertext as plaintext")
	require.Equal(t, int64(0), CredentialPlaintextFallbackCount(), "报错路径不该计回落")
}

// 5) 解密失败 -> 报错，不能返回空 map。空 map 会被上层读成"该账号没配密钥"。
func TestCredentialsFromEntity_DecryptFailureIsAnError(t *testing.T) {
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, credentialTestEncryptor(t))

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   11,
		CredentialsEncrypted: strPtr("not-a-valid-ciphertext"),
	})
	require.Error(t, err)
	require.Nil(t, got)
	require.Contains(t, err.Error(), "decrypt credentials for account 11")
}

// 6) 密文合法但解出来不是 JSON 对象 -> 报错。
func TestCredentialsFromEntity_DecryptedNonObjectIsAnError(t *testing.T) {
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)

	ciphertext, err := encryptor.Encrypt(`["not","an","object"]`)
	require.NoError(t, err)

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   12,
		CredentialsEncrypted: strPtr(ciphertext),
	})
	require.Error(t, err)
	require.Nil(t, got)
	require.Contains(t, err.Error(), "unmarshal decrypted credentials for account 12")
}

// 7) 没有任何凭据的账号回落时**不计数**——否则空账号多的实例会永远显示"没迁完"。
func TestCredentialsFromEntity_EmptyPlaintextDoesNotCountFallback(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	repo := newAccountRepositoryWithSQL(nil, nil, nil)

	got, err := repo.credentialsFromEntity(&dbent.Account{ID: 13})
	require.NoError(t, err)
	require.Nil(t, got)
	require.Equal(t, int64(0), CredentialPlaintextFallbackCount())
}

// 8) 边界包装会把错误透出去（调用方必须看得到，而不是拿到一个空实体）。
func TestAccountEntityToService_PropagatesDecryptError(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // 无加密器

	account, err := repo.accountEntityToService(&dbent.Account{
		ID:                   14,
		CredentialsEncrypted: strPtr("b64-ciphertext"),
	})
	require.Error(t, err)
	require.Nil(t, account)
}

// 9) 用量日志那条路刻意不解密：凭据为 nil，且不触发回落计数。
// 它的 DTO 只取 ID 与 Name（"避免泄露敏感字段"），所以根本不需要凭据。
func TestAccountEntityToServiceWithoutCredentials_NeverDecrypts(t *testing.T) {
	resetCredentialPlaintextFallbackCount()

	account := accountEntityToServiceWithoutCredentials(&dbent.Account{
		ID:                   15,
		Name:                 "acc-15",
		Credentials:          map[string]any{"api_key": "sk-legacy"},
		CredentialsEncrypted: strPtr("b64-ciphertext"),
	})
	require.NotNil(t, account)
	require.Equal(t, int64(15), account.ID)
	require.Equal(t, "acc-15", account.Name)
	require.Nil(t, account.Credentials, "用量日志路径不该带凭据")
	require.Equal(t, int64(0), CredentialPlaintextFallbackCount())
}

// ── 阶段三 3.4 的「契约翻转」测试 ────────────────────────────────────────────
//
// 任务原文要求：删回落分支之前，新负例 MUST 表现为"回落成功"——否则那负例证明不了
// 它测的是新契约（很可能测的是别的路径，删完照样绿）。
//
// 所以这里把两种行为都写成显式断言，并在测试名与注释里标明"该翻转"：
//   - 现在（Stage 3 之前）：密文为空 ⇒ 回落明文，成功，且计数 +1
//   - Stage 3.4 之后：密文为空 ⇒ **硬错误**（`credentialsFromEntity` 返回 error）
//
// 翻转方法：把 fallbackExpected 由 "ok" 改成 "error"，并把下面那段断言改成
// require.Error。若翻转后仍绿，说明回落分支真的被删掉了；若翻转后红，
// 说明还有路径在回落——那就是没删干净。

func TestCredentialFallbackContract_CurrentlyFallsBack_WILL_FLIP_AT_STAGE3(t *testing.T) {
	const fallbackCurrentlyAllowed = true // ← Stage 3.4 之后改成 false

	resetCredentialPlaintextFallbackCount()
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // 无加密器

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:          88,
		Credentials: map[string]any{"api_key": "sk-legacy"},
	})

	if fallbackCurrentlyAllowed {
		require.NoError(t, err, "Stage 3 之前：没有密文 ⇒ 回落明文是**当前契约**")
		require.Equal(t, "sk-legacy", got["api_key"])
		require.Equal(t, int64(1), CredentialPlaintextFallbackCount(),
			"回落必须可观测——计数器就是 Stage 3.4 判断'能不能删'的唯一依据")
		return
	}

	// Stage 3.4 之后应走到这里
	require.Error(t, err, "Stage 3.4 之后：密文缺失 MUST 硬失败，不再回落")
	require.Nil(t, got)
	require.Contains(t, err.Error(), "has no encrypted credentials")
}

// 与之配套：**密文存在**时，无论哪个阶段都必须走解密路径（不得回落）。
// 这条是不随 Stage 3 翻转的那一半，用来确认"删回落"不会顺手把正常路径也删了。
func TestCredentialFallbackContract_CiphertextPathUnaffectedByStage3(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)

	ciphertext, err := encryptor.Encrypt(`{"api_key":"sk-encrypted"}`)
	require.NoError(t, err)

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   89,
		Credentials:          map[string]any{"api_key": "sk-stale-plaintext"},
		CredentialsEncrypted: strPtr(ciphertext),
	})
	require.NoError(t, err)
	require.Equal(t, "sk-encrypted", got["api_key"], "有密文时永远读密文")
	require.Zero(t, CredentialPlaintextFallbackCount(), "有密文就不该记回落")
}
