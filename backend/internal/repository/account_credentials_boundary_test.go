package repository

import (
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// 边界解密（design D1 读侧）的测试。
//
// 阶段 5.2 起明文列 `accounts.credentials` **已物理删除**，所以这一组只守两件事：
//  1. 有密文 → 必须解密出凭据（且解不开要报错，不能静默返回空）；
//  2. 没有密文 → 就是**没有凭据**（合法终态），返回 nil 且不报错。
//
// 一个刻意的取舍：第 2 条**不计数、不打日志**。drop 之后"空账号"与"密文丢了"
// 在数据上不可区分，而调度会周期性全量读所有账号 —— 在这里计数只会把计数器
// 刷成常数噪音，反而掩盖真正的异常。密文完整性由写侧不变量 + 备份 + 5.3 判据保证。

func strPtr(s string) *string { return &s }

// 1) 有密文 -> 解密，返回密文里的内容。
func TestCredentialsFromEntity_DecryptsCiphertext(t *testing.T) {
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)

	ciphertext, err := encryptor.Encrypt(`{"api_key":"sk-encrypted-truth"}`)
	require.NoError(t, err)

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   7,
		CredentialsEncrypted: strPtr(ciphertext),
	})
	require.NoError(t, err)
	require.Equal(t, "sk-encrypted-truth", got["api_key"])
}

// 2) 没有密文 -> 没有凭据（合法终态）：返回 nil、不报错、不计数。
//
// 这条在阶段 3.4 时断言的是"硬错误"（当时还有明文列，能区分"有明文无密文"）。
// 5.2 drop 掉明文列之后那个状态不再可表达，硬错误会把**空账号**也打成故障：
// 调度建桶读到一行空账号 → 整批重建失败 → 全站 503（本 change 真实发生过一次）。
func TestCredentialsFromEntity_NoCiphertextMeansNoCredentials(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, credentialTestEncryptor(t))

	got, err := repo.credentialsFromEntity(&dbent.Account{ID: 8})
	require.NoError(t, err, "空账号必须安静地返回空，不能报错")
	require.Nil(t, got)
	require.Zero(t, CredentialPlaintextFallbackCount(), "空账号不该计数（否则计数器是常数噪音）")
}

// 3) 空密文串等同于"没有密文"——空串不是有效密文，不能拿去解密。
func TestCredentialsFromEntity_EmptyCiphertextIsTreatedAsAbsent(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, credentialTestEncryptor(t))

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   9,
		CredentialsEncrypted: strPtr(""),
	})
	require.NoError(t, err)
	require.Nil(t, got)
	require.Zero(t, CredentialPlaintextFallbackCount())
}

// 4) D1a 的负例：有密文但没配加密器 -> **必须报错**。
//
// 这条是本 change 里最重要的一条负例。若实现"没加密器就当空"，线上会把
// 每个账号都读成"没配密钥"，症状退化成上游 401，而真因（加密器没接上）看不见。
func TestCredentialsFromEntity_RefusesCiphertextWithoutEncryptor(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // cryptor = nil

	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:                   10,
		CredentialsEncrypted: strPtr("b64-ciphertext"),
	})
	require.Error(t, err, "有密文却没有加密器时必须报错，不能当成空凭据")
	require.Nil(t, got)
	require.Contains(t, err.Error(), "refusing to treat ciphertext as plaintext")
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

// 7) 边界包装会把错误透出去（调用方必须看得到，而不是拿到一个空实体）。
func TestAccountEntityToService_PropagatesDecryptError(t *testing.T) {
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // 无加密器

	account, err := repo.accountEntityToService(&dbent.Account{
		ID:                   14,
		CredentialsEncrypted: strPtr("b64-ciphertext"),
	})
	require.Error(t, err)
	require.Nil(t, account)
}

// 8) 用量日志那条路刻意不解密：凭据为 nil。
// 它的 DTO 只取 ID 与 Name（"避免泄露敏感字段"），所以根本不需要凭据。
func TestAccountEntityToServiceWithoutCredentials_NeverDecrypts(t *testing.T) {
	account := accountEntityToServiceWithoutCredentials(&dbent.Account{
		ID:                   15,
		Name:                 "acc-15",
		CredentialsEncrypted: strPtr("b64-ciphertext"),
	})
	require.NotNil(t, account)
	require.Equal(t, int64(15), account.ID)
	require.Equal(t, "acc-15", account.Name)
	require.Nil(t, account.Credentials, "用量日志路径不该带凭据")
}

// 9) 计数器在阶段 5.2 之后**恒为 0**。
//
// 保留这条断言是为了让"回落在代码里已经不存在"有一个可执行的证据：
// 若有人重新引入"读不到密文就找别的来源"，这里会红。
func TestCredentialPlaintextFallbackCount_IsAlwaysZeroAfterStage52(t *testing.T) {
	resetCredentialPlaintextFallbackCount()
	encryptor := credentialTestEncryptor(t)
	repo := newAccountRepositoryWithSQLCrypto(nil, nil, nil, encryptor)

	ciphertext, err := encryptor.Encrypt(`{"api_key":"sk-x"}`)
	require.NoError(t, err)
	_, err = repo.credentialsFromEntity(&dbent.Account{ID: 90, CredentialsEncrypted: strPtr(ciphertext)})
	require.NoError(t, err)
	_, err = repo.credentialsFromEntity(&dbent.Account{ID: 91})
	require.NoError(t, err)

	require.Zero(t, CredentialPlaintextFallbackCount())
}
