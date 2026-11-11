package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/lib/pq"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 凭据落盘加密的列派生逻辑（阶段一：双写）。
//
// 设计见 openspec/changes/harden-sub2api-credential-storage/design.md D2 / D7。
// 这里只产出"要写进新列的值"，不改任何既有读写行为——明文列照旧写，
// 因此本阶段的每次写入都是可回滚的。

// credentialMetaWhitelist 是允许从凭据里**原样复制**进 credentials_meta 的子键白名单。
//
// **必须保持极窄**：这一列是明文、可被 SQL 直接筛选、也随备份外泄。
// 只允许放"生产 SQL 确实要筛选、且本身不是秘密"的键。当前生产 SQL 只提取
// base_url 与 account_mode（各 8 处 / 5 处，见 design D7 的 35 处子键提取清单）。
// 新增子键前必须先确认 SQL 层确实需要明文筛选——默认是"不进明文列"。
var credentialMetaWhitelist = map[string]struct{}{
	"base_url":     {},
	"account_mode": {},
}

// credentialMetaKeyHasRefreshToken 是写进 credentials_meta 的**派生事实**，
// 不是从凭据里复制的子键，所以它不在 credentialMetaWhitelist 里。
//
// 为什么需要它：加密之后密文不透明，SQL 无法判断 refresh_token（密钥子键，
// 不许进明文列）是否存在，而后台 OAuth 刷新候选筛选只需要"有没有"这个布尔事实。
// 它泄漏的只是"该账号持有 refresh token"，不含密钥本身。
const credentialMetaKeyHasRefreshToken = "has_refresh_token"

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
	out := make(map[string]any, len(credentialMetaWhitelist)+1)
	for key := range credentialMetaWhitelist {
		if value, ok := credentials[key]; ok {
			out[key] = value
		}
	}
	// **始终写入**（含 false）：让 meta 自描述——"这行是否已带此事实"可以用
	// `NOT (credentials_meta ? 'has_refresh_token')` 一条 SQL 查完，回填闸门就靠它。
	// 用 credentialHasRefreshToken 保证与 SQL `->>` + BTRIM 同语义（含非字符串值）。
	out[credentialMetaKeyHasRefreshToken] = credentialHasRefreshToken(credentials)
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

	// 阶段 3.4：**回落分支已删除**。有明文却没有密文 = 硬错误。
	//
	// 为什么不能再"读不到密文就用明文凑"：
	//  1. drop 明文列之后本来就无明文可读，这段代码只是在掩盖"这一行没回填"；
	//  2. 它会把配置/迁移问题伪装成正常服务，症状退化成"凭据看起来是空的"，最难查；
	//  3. 留着它，就永远没有证据说明"可以安全地 drop 明文列"。
	// 观察期（3.3）已在生产上确认回落次数为 0，才做的这一步。
	//
	// 合法终态仍放行：明文字段为空（新建未填凭据的账号）返回 nil 且不报错
	// —— 与原行为一致（原来走的就是 copyJSONMap(nil)），避免让调用方多一种判空形态。
	if len(m.Credentials) > 0 {
		credentialPlaintextFallbacks.Add(1)
		logger.LegacyPrintf("repository.credential",
			"[CredentialAtRest] missing ciphertext: account=%d has plaintext credentials but no credentials_encrypted", m.ID)
		return nil, fmt.Errorf(
			"account %d has plaintext credentials but no credentials_encrypted: "+
				"stage-3.4 removed the plaintext fallback; re-run cmd/credential-backfill", m.ID)
	}
	return copyJSONMap(m.Credentials), nil
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

// credentialHasRefreshToken 判断凭据里是否有非空 refresh_token。
//
// **必须与 SQL 侧 `NULLIF(BTRIM(credentials->>'refresh_token'), '') IS NULL` 的反面逐字等价**，
// 因为这个判定现在由 Go 承担（见 SetGrokOAuthErrorIfCredentialsUnchanged 的等价改写）。
// 对齐的细节：
//   - 键缺失 / 值为 JSON null  -> `->>` 得 SQL NULL -> 视为"没有"
//   - 字符串                   -> BTRIM 去首尾空白后非空才算"有"
//   - 其它类型（数字/布尔/对象/数组）-> `->>` 得到非空的文本表示 -> 视为"有"
//     例如 refresh_token: 123 在 SQL 下得到 '123'，BTRIM 后非空；这里也必须判 true，
//     否则 Go 与 SQL 会给出相反结论（那种不一致表现为"有些账号能刷新、有些不能"）。
func credentialHasRefreshToken(credentials map[string]any) bool {
	raw, ok := credentials["refresh_token"]
	if !ok || raw == nil {
		return false
	}
	if text, isString := raw.(string); isString {
		return strings.TrimSpace(text) != ""
	}
	return true
}

// credentialAPIKeyHashArg 返回可直接当 SQL 参数用的 api_key 指纹：
// 有 api_key → hex 字符串；没有 → **无类型 nil**（SQL NULL）。
//
// 为什么必须是 NULL 而不是空串：这些位置用的是 `IS [NOT] DISTINCT FROM`，
// 空串与 NULL 是两回事。旧写法比较 jsonb 子键，缺失时是 SQL NULL，
// 所以新写法也必须传 NULL 才能保持「两边都缺 ⇒ 不变化」的语义。
//
// 也不要把 (*string)(nil) 直接塞进 any——那是个非 nil 的接口包着空指针，
// 驱动会报错；必须转成真正的无类型 nil。
func credentialAPIKeyHashArg(credentials map[string]any) any {
	hash := credentialAPIKeyHash(credentials)
	if hash == nil {
		return nil
	}
	return *hash
}

// credentialHashHex 返回整份凭据规范化 JSON 的 sha256（hex），永不返回空。
//
// 与 credentials_hash 列的写入用**同一个** canonicalCredentialJSON，
// 所以 SQL 里 `credentials_hash = $n` 与旧的 `credentials = $n::jsonb` 等价
// ——前提是那一行已经回填过（回填是阶段二的硬前置，见 tasks 2.0d）。
func credentialHashHex(credentials map[string]any) string {
	canonical, err := canonicalCredentialJSON(credentials)
	if err != nil {
		// canonicalCredentialJSON 只对不可序列化的值失败（chan/func 等），
		// 凭据里不会出现；真出现也不能静默当空文档——返回空串让 SQL 比较必然不匹配，
		// 比悄悄命中一行错误数据安全。
		return ""
	}
	return sha256Hex(canonical)
}

// credentialHashOfRawJSON 从**数据库读出的明文 JSON** 算整文档哈希。
//
// 用途：Grok 的 CAS 语句把"读到的快照"原样作为比较基准。快照是 jsonb 的文本形态，
// 必须先解析成 map 再走 canonicalCredentialJSON，才能与写路径写入 credentials_hash
// 的那份哈希同源。直接哈希原始字节是不行的——JSON 键序/空白差异会让哈希对不上。
//
// 解析失败返回空串：空串与任何真实哈希都不相等，CAS 必然不命中，属安全失败方向。
func credentialHashOfRawJSON(raw string) string {
	if raw == "" {
		return credentialHashHex(nil)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		logger.LegacyPrintf("repository.credential",
			"[CredentialAtRest] cannot canonicalize credentials snapshot for hashing: %v", err)
		return ""
	}
	return credentialHashHex(decoded)
}

// credentialColumnUpdater 是"能执行 SQL 的东西"的最小接口（*sql.DB / *dbent.Client / tx 都满足）。
// 抽出来是为了让派生列回填既能用在仓储写路径，也能用在事务里。
type credentialColumnUpdater interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// deriveCredentialColumnsAfterMerge 在**同一个事务内**重新推导指定账号的派生列。
//
// 为什么需要它：BulkUpdate 的凭据是 SQL 侧合并（`credentials || $n`），Go 拿不到
// "合并后的完整文档"，因此无法在写之前算出 meta/密文/两个指纹。之前的做法是把
// 这几列置 NULL 让读路径回落明文——安全但会让密文丢失，且阶段四 drop 明文列之后
// 就不再安全。
//
// 现在改为：合并语句执行完之后，在同一事务里回读明文（能读到刚合并的结果），
// 在 Go 端算派生列，再一次性写回。合并语义一字不动，派生列从"被清空"变成"正确"。
//
// 必须在事务内调用：否则回读到的是别的连接可能已经改过的数据。
func deriveCredentialColumnsAfterMerge(ctx context.Context, exec credentialColumnUpdater, cryptor service.SecretEncryptor, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := exec.QueryContext(ctx, `
		SELECT id, credentials
		FROM accounts
		WHERE id = ANY($1) AND deleted_at IS NULL`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("derive credential columns: select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type derived struct {
		id        int64
		meta      string
		encrypted *string
		hash      string
		apiKey    *string
	}
	batch := make([]derived, 0, len(ids))
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("derive credential columns: scan: %w", err)
		}
		decoded := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("derive credential columns: account %d: %w", id, err)
			}
		}
		columns, err := CredentialCryptoColumnsFor(decoded, cryptor)
		if err != nil {
			return fmt.Errorf("derive credential columns: account %d: %w", id, err)
		}
		metaJSON, err := json.Marshal(columns.Meta)
		if err != nil {
			return fmt.Errorf("derive credential columns: account %d: %w", id, err)
		}
		batch = append(batch, derived{
			id:        id,
			meta:      string(metaJSON),
			encrypted: columns.Encrypted,
			hash:      *columns.Hash,
			apiKey:    columns.APIKeyHash,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("derive credential columns: rows: %w", err)
	}
	if len(batch) == 0 {
		return nil
	}

	placeholder := 1
	values := make([]string, 0, len(batch))
	args := make([]any, 0, len(batch)*4)
	for _, d := range batch {
		values = append(values, fmt.Sprintf("($%d::bigint, $%d::jsonb, $%d::text, $%d::text, $%d::text)", placeholder, placeholder+1, placeholder+2, placeholder+3, placeholder+4))
		args = append(args, d.id, d.meta, d.encrypted, d.hash, d.apiKey)
		placeholder += 5
	}
	_, err = exec.ExecContext(ctx, `
		UPDATE accounts AS a
		SET credentials_meta = v.meta,
			credentials_encrypted = v.encrypted,
			credentials_hash = v.hash,
			credentials_api_key_hash = v.api_key_hash
		FROM (VALUES `+strings.Join(values, ", ")+`) AS v(id, meta, encrypted, hash, api_key_hash)
		WHERE a.id = v.id`, args...)
	if err != nil {
		return fmt.Errorf("derive credential columns: update: %w", err)
	}
	return nil
}
