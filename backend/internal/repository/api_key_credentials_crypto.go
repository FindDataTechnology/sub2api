package repository

import (
	"crypto/subtle"
	"fmt"
	"sync/atomic"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// api_keys 的派生列（阶段三 3.5a）。
//
// 背景：阶段一给 `api_keys` 建了 `key_hash` / `key_encrypted` 两列，但写路径一直没接，
// 只有回填工具在填存量。后果是**增量空洞**：一旦按 3.5b 把鉴权改成"按 key_hash 查"，
// 新建的 key 会立刻查不到（回填补不了未来）。所以必须先补写入，再改查找。
//
// 与 accounts 侧的差异：api_keys 没有子键、没有整文档哈希，只有"一个字面 key"。
// 所以派生就是 sha256(key) + 加密 key 本身。

// apiKeyDerivedColumns 由明文 key 算出落库需要的两个派生值。
//
// 返回值约定：
//   - hash 为空串表示"没有指纹可写"（空 key）→ 调用方 MUST 写 NULL 而不是空串：
//     key_hash 上是**部分唯一索引**（WHERE key_hash IS NOT NULL），
//     写空串会在第二行就撞唯一约束。
//   - encrypted 为 nil 表示没有加密器（未配置/测试路径）→ 同样写 NULL，
//     绝不写空串（空串会被当成密文去解密）。
func apiKeyDerivedColumns(plaintext string, cryptor service.SecretEncryptor) (hash string, encrypted *string, err error) {
	if plaintext == "" {
		return "", nil, nil
	}
	hash = sha256Hex([]byte(plaintext))
	if cryptor == nil {
		return hash, nil, nil
	}
	ciphertext, encErr := cryptor.Encrypt(plaintext)
	if encErr != nil {
		return "", nil, fmt.Errorf("encrypt api key: %w", encErr)
	}
	return hash, &ciphertext, nil
}

// apiKeyPlaintextLookupFallbacks 统计"指纹没命中、回落明文"的次数（阶段三 3.5b）。
//
// 与 credentials 的回落计数同一个用途：双写/回填窗口内，尚未写指纹的行仍能被认证到，
// 但这个数字必须能被看见——它是"回填还没跑完"或"有新写入没带指纹"的唯一信号。
//
// 进程内计数、重启归零，所以判"是否迁完"仍要用 DB 计数：
// `SELECT count(*) FROM api_keys WHERE key_hash IS NULL AND deleted_at IS NULL`。
var apiKeyPlaintextLookupFallbacks atomic.Int64

// APIKeyPlaintextLookupFallbackCount 返回本进程累计的明文回落次数。
func APIKeyPlaintextLookupFallbackCount() int64 { return apiKeyPlaintextLookupFallbacks.Load() }

// resetAPIKeyPlaintextLookupFallbackCount 仅供测试。
func resetAPIKeyPlaintextLookupFallbackCount() { apiKeyPlaintextLookupFallbacks.Store(0) }

// constantTimeEqualAPIKey 做常量时间比对，避免按字节提前返回泄漏前缀信息。
func constantTimeEqualAPIKey(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// apiKeyCiphertextMissingLookups 统计"鉴权时这一行还没有 key_encrypted"的次数。
//
// 与 apiKeyPlaintextLookupFallbacks 的区别：那个统计"没指纹、只能按明文列查"，
// 这个统计"有指纹但没有密文，复核只能看明文列"。两者都是回填窗口的产物，
// 但第二阶段修不了这个 —— 它只在回填跑完之前出现。
var apiKeyCiphertextMissingLookups atomic.Int64

// APIKeyCiphertextMissingLookupCount 返回本进程累计次数。
func APIKeyCiphertextMissingLookupCount() int64 { return apiKeyCiphertextMissingLookups.Load() }

// resetAPIKeyCiphertextMissingLookupCount 仅供测试。
func resetAPIKeyCiphertextMissingLookupCount() { apiKeyCiphertextMissingLookups.Store(0) }

// apiKeyAuthoritativePlaintext 给出这一行"权威的"明文 key：鉴权复核要用它，
// 对外返回的 service.APIKey.Key（掩码/日志要用）也来自它。
//
// 为什么必须优先取密文：drop 明文列之后 `key_encrypted` 是**唯一**副本。
// 若复核仍读明文列，drop 当天会撞 `column "key" does not exist`；若代码改成
// "读不到就当空"（更糟），那么每一行都会因"明文不等"被判查不到 —— 全站 401。
// 所以最终形态必须在 drop **之前**就落到位：密文优先，明文列只在密文缺失时兜底。
//
// 三种输入的处置：
//   - 有密文：解密后作为权威值（解密失败=硬错误，绝不当成"没密文"再回落）；
//   - 无密文但有明文列：回填窗口内，用明文列并计数+打日志，让空洞可见；
//   - 两者都没有：返回空串，调用方据此判"查不到"。
func (r *apiKeyRepository) apiKeyAuthoritativePlaintext(m *dbent.APIKey) (string, error) {
	if m == nil {
		return "", nil
	}
	if m.KeyEncrypted != nil && *m.KeyEncrypted != "" {
		if r == nil || r.cryptor == nil {
			return "", fmt.Errorf("api key %d has key_encrypted but no encryptor is configured: refusing to trust the plaintext column", m.ID)
		}
		plaintext, err := r.cryptor.Decrypt(*m.KeyEncrypted)
		if err != nil {
			// 解密失败是数据/密钥问题，不能退化成"用明文列凑一个答案"。
			return "", fmt.Errorf("decrypt api key %d: %w", m.ID, err)
		}
		return plaintext, nil
	}
	if m.Key != "" {
		apiKeyCiphertextMissingLookups.Add(1)
		logger.LegacyPrintf("repository.apikey",
			"[CredentialAtRest] api key %d has no key_encrypted yet; falling back to the plaintext column", m.ID)
	}
	return m.Key, nil
}
