package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
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

// CredentialCryptoColumns 是一条账号写入需要落到新列上的四个值。
// 指针为 nil 表示该列写 NULL（尚未迁移 / 未配置加密器）。
//
// 导出是为了让存量回填工具（cmd/credential-backfill）复用**完全同一套**派生逻辑：
// 回填与运行时如果各算各的，哈希就会对不上，而那种不一致只会表现为
// "乐观并发莫名总是判定已变更"。
type CredentialCryptoColumns struct {
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
func (r *accountRepository) credentialCryptoColumnsFor(credentials map[string]any) (CredentialCryptoColumns, error) {
	return CredentialCryptoColumnsFor(credentials, r.cryptorOrNil())
}

// cryptorOrNil 让 nil 仓储也能安全取 cryptor（测试与只读路径会传 nil 仓储）。
func (r *accountRepository) cryptorOrNil() service.SecretEncryptor {
	if r == nil {
		return nil
	}
	return r.cryptor
}

// CredentialCryptoColumnsFor 是派生逻辑的**唯一实现**，供仓储写路径与存量回填共用。
func CredentialCryptoColumnsFor(credentials map[string]any, cryptor service.SecretEncryptor) (CredentialCryptoColumns, error) {
	canonical, err := canonicalCredentialJSON(credentials)
	if err != nil {
		return CredentialCryptoColumns{}, err
	}
	hash := sha256Hex(canonical)

	columns := CredentialCryptoColumns{
		Meta:       credentialMetaSubset(credentials),
		Hash:       &hash,
		APIKeyHash: credentialAPIKeyHash(credentials),
	}
	if cryptor == nil {
		return columns, nil
	}
	encrypted, err := cryptor.Encrypt(string(canonical))
	if err != nil {
		return CredentialCryptoColumns{}, err
	}
	columns.Encrypted = &encrypted
	return columns, nil
}

// credentialPlaintextFallbacks 统计"密文列为空、回落读明文列"的次数。
//
// 两点必须说清，否则这个数字会被误用：
//  1. 它是**进程内**计数，重启即归零。所以它只适合做告警/观测；
//     判断"存量是否迁完"的闸门 MUST 用数据库计数
//     （`SELECT count(*) FROM accounts WHERE credentials_encrypted IS NULL`），
//     那个才是重启不丢的事实。
//  2. 只统计"确实有明文可回落"的行。凭据本来就是空的账号不计入，
//     否则空账号多的实例会永远看起来"没迁完"。
var credentialPlaintextFallbacks atomic.Int64

// CredentialPlaintextFallbackCount 返回本进程累计回落次数（见上面两点说明）。
func CredentialPlaintextFallbackCount() int64 {
	return credentialPlaintextFallbacks.Load()
}

// resetCredentialPlaintextFallbackCount 仅供测试。
func resetCredentialPlaintextFallbackCount() {
	credentialPlaintextFallbacks.Store(0)
}

// credentialsFromEntity 按"优先密文、回落明文"解出凭据。
//
// 这是 design D1 里那个唯一的加解密边界在**读侧**的实现。
func (r *accountRepository) credentialsFromEntity(m *dbent.Account) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}

	if m.CredentialsEncrypted != nil && *m.CredentialsEncrypted != "" {
		if r == nil || r.cryptor == nil {
			// D1a 的负例：有密文却没加密器。绝不能回落明文——那会把
			// "配置错了"伪装成"一切正常"，而且密文本身会被当成凭据发出去。
			return nil, fmt.Errorf(
				"account %d has encrypted credentials but no encryptor is configured: "+
					"refusing to treat ciphertext as plaintext", m.ID)
		}
		plaintext, err := r.cryptor.Decrypt(*m.CredentialsEncrypted)
		if err != nil {
			return nil, fmt.Errorf("decrypt credentials for account %d: %w", m.ID, err)
		}
		decoded := map[string]any{}
		if err := json.Unmarshal([]byte(plaintext), &decoded); err != nil {
			return nil, fmt.Errorf("unmarshal decrypted credentials for account %d: %w", m.ID, err)
		}
		return decoded, nil
	}

	// 回落：迁移窗口内的过渡态。有内容才算一次"未迁移"。
	if len(m.Credentials) > 0 {
		credentialPlaintextFallbacks.Add(1)
		logger.LegacyPrintf("repository.credential",
			"[CredentialAtRest] plaintext fallback: account=%d has no credentials_encrypted yet", m.ID)
	}
	return copyJSONMap(m.Credentials), nil
}

// credentialPlaintextFallbackSQLPredicate 是给 SQL 层用的"该行尚未迁移"判据。
// 保留成函数是为了让 Stage 2 改写 SQL 时只有一个地方定义这个语义。
func credentialPlaintextFallbackSQLPredicate(column string) string {
	return "(" + column + " IS NULL OR " + column + " = '')"
}

// credentialAPIKeyHashHex 返回 api_key 的指纹（hex），空串返回空串。
//
// 为什么空串回空串而不是 nil：这些调用点把结果直接当 SQL 参数用，
// `credential = ''` 与列里的 NULL 不相等——这与"空 api_key 不构成用量身份"
// 的判断一致（旧写法 `->> 'api_key' = ''` 会匹配空串行，新写法匹配不到，
// 这是 2.2 已知并已用差分钉住的收窄）。
func credentialAPIKeyHashHex(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	return sha256Hex([]byte(apiKey))
}

// credentialAPIKeyHashHexes 是批量版本，保持输入顺序。
func credentialAPIKeyHashHexes(apiKeys []string) []string {
	out := make([]string, 0, len(apiKeys))
	for _, k := range apiKeys {
		out = append(out, credentialAPIKeyHashHex(k))
	}
	return out
}
