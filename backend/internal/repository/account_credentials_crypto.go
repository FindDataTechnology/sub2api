package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// 凭据落盘加密的列派生逻辑（阶段一：双写）。
//
// 设计见 openspec/changes/harden-sub2api-credential-storage/design.md D2 / D7。
// 这里只产出"要写进新列的值"，不改任何既有读写行为——明文列照旧写，
// 因此本阶段的每次写入都是可回滚的。

// credentialMetaWhitelist 是允许进入 credentials_meta 明文列的子键白名单。
//
// **必须保持极窄**：这一列是明文、可被 SQL 直接筛选、也随备份外泄。
// 只允许放"生产 SQL 确实要筛选、且本身不是秘密"的键。当前生产 SQL 只提取
// base_url 与 account_mode（各 8 处 / 5 处，见 design D7 的 35 处子键提取清单）。
// 新增子键前必须先确认 SQL 层确实需要明文筛选——默认是"不进明文列"。
var credentialMetaWhitelist = map[string]struct{}{
	"base_url":     {},
	"account_mode": {},
}

// credentialCryptoColumns 是一条账号写入需要落到新列上的四个值。
// 指针为 nil 表示该列写 NULL（尚未迁移 / 未配置加密器）。
type credentialCryptoColumns struct {
	Meta       map[string]any // credentials_meta：白名单子键的明文副本，永远写
	Encrypted  *string        // credentials_encrypted：AES-256-GCM 密文，无 cryptor 时为 nil
	Hash       *string        // credentials_hash：sha256(规范化明文)，永远写
	APIKeyHash *string        // credentials_api_key_hash：sha256(api_key)，有 api_key 时写
}

// credentialMetaSubset 从凭据里挑出白名单子键。返回空 map 而不是 nil——
// 列是 NOT NULL DEFAULT '{}'，写 nil 会把整份 meta 清空。
func credentialMetaSubset(credentials map[string]any) map[string]any {
	out := make(map[string]any, len(credentialMetaWhitelist))
	for key := range credentialMetaWhitelist {
		if value, ok := credentials[key]; ok {
			out[key] = value
		}
	}
	return out
}

// canonicalCredentialJSON 产出用于哈希的规范化 JSON。
//
// 依赖 encoding/json 对 map key 的排序保证——同一份内容（键序不同）必须得到
// 相同的 hash，否则乐观并发比较会在"内容没变但键序变了"时误判为已变更。
// 注意：这只保证 map 键有序；数组元素顺序按原样保留，这是对的（数组顺序有语义）。
func canonicalCredentialJSON(credentials map[string]any) ([]byte, error) {
	if credentials == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(credentials)
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// credentialAPIKeyHash 返回 sha256(api_key)，没有 api_key 时返回 nil。
//
// 用途：列变密文后 `credentials ->> 'api_key'` 会静默返回 NULL（不报错），
// ollama / opencode 用量车道按 api_key 找账号必须改走这个指纹。
func credentialAPIKeyHash(credentials map[string]any) *string {
	apiKey, ok := credentials["api_key"].(string)
	if !ok || apiKey == "" {
		return nil
	}
	hash := sha256Hex([]byte(apiKey))
	return &hash
}

// credentialCryptoColumnsFor 计算一条账号写入要落的新列值。
//
// cryptor 为 nil 时只跳过加密列（其余三列照写：meta 和两个指纹都不需要密钥，
// 提前写入能让阶段二的 SQL 改写有数据可依）。
func (r *accountRepository) credentialCryptoColumnsFor(credentials map[string]any) (credentialCryptoColumns, error) {
	canonical, err := canonicalCredentialJSON(credentials)
	if err != nil {
		return credentialCryptoColumns{}, err
	}
	hash := sha256Hex(canonical)

	columns := credentialCryptoColumns{
		Meta:       credentialMetaSubset(credentials),
		Hash:       &hash,
		APIKeyHash: credentialAPIKeyHash(credentials),
	}
	if r == nil || r.cryptor == nil {
		return columns, nil
	}
	encrypted, err := r.cryptor.Encrypt(string(canonical))
	if err != nil {
		return credentialCryptoColumns{}, err
	}
	columns.Encrypted = &encrypted
	return columns, nil
}
