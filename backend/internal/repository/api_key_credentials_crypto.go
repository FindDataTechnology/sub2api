package repository

import (
	"fmt"

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
