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
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
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

// credentialPlaintextFallbacks **阶段 5.2 之后恒为 0**：明文列已物理删除，
// 不再存在"回落读明文列"这件事。
//
// 为什么保留这个符号而不删：3.3 的仪表读数与既有测试仍引用它，删掉会让
// "从 3.3 到 5.2 的观测口径"断成两套；恒 0 本身就是最干净的信号
// （它证明回落在代码里已经不存在，而不是"恰好没触发"）。
//
// 历史上的两点说明（保留以免误用）：
//  1. 它是**进程内**计数，重启即归零，只适合告警/观测；判断存量是否迁完
//     MUST 用数据库计数（`credentials_encrypted IS NULL` 的行数）。
//  2. 它只统计"确实有明文可回落"的行 —— 空账号从不计入。
var credentialPlaintextFallbacks atomic.Int64

// CredentialPlaintextFallbackCount 返回本进程累计回落次数（阶段 5.2 之后恒为 0）。
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

	// 阶段 5.2：明文列 `credentials` 已**物理删除**，所以这里既没有回落、
	// 也没有"有明文无密文"这个状态可判了。没有密文 = 这一行没有凭据。
	//
	// 为什么不再报硬错误：drop 之后"空凭据"与"密文丢了"在数据上不可区分
	// （两者都只是 credentials_encrypted IS NULL）。对它报错会把一个
	// **合法的空账号**（建了还没填凭据）变成调度建桶失败 → 全站 503，
	// 而那种事故本 change 已经发生过一次。所以这里返回 nil 并把计数打出去，
	// 让"密文缺失"在仪表上可见，而不是让一行坏数据拖垮整个进程。
	//
	// 不变量改由**写侧**保证：所有写路径都同事务写 credentials_encrypted
	// （`CredentialCryptoColumnsFor`），5.3 的泄漏判据 + 5.2 前的回填已把存量补齐。
	// 不计数、不打日志：drop 之后"空账号"（建了还没填凭据）与"密文丢了"在数据上
	// 不可区分，而调度会周期性全量读所有账号 —— 在这里计数会把计数器刷成常数噪音，
	// 反而掩盖真正的异常。密文完整性由写侧不变量 + 备份 + 5.3 泄漏判据保证。
	return nil, nil
}

// credentialAPIKeyHashHex 返回 api_key 的指纹（hex），空串返回空串。
//
// 为什么空串回空串而不是 nil：这些调用点把结果直接当 SQL 参数用，
// `credential = ”` 与列里的 NULL 不相等——这与"空 api_key 不构成用量身份"
// 的判断一致（旧写法 `->> 'api_key' = ”` 会匹配空串行，新写法匹配不到，
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
// **必须与 SQL 侧 `NULLIF(BTRIM(credentials->>'refresh_token'), ”) IS NULL` 的反面逐字等价**，
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

// mergeAccountCredentialsInTx 在同一事务内把 delta **合并**进账号凭据并重算派生列。
//
// 为什么不能再用 SQL 侧合并（阶段 5.2）：旧实现是
// `credentials = COALESCE(credentials,'{}'::jsonb) || $n::jsonb`，明文列 drop 之后
// 这条语句不存在；而"用密文做 JSONB 合并"在密码学上也不可能。
// 所以合并改到 Go：读旧密文 → 解密 → 字典合并（与 jsonb `||` 同为**顶层浅合并**）→
// 重新加密并写回四列。
//
// 必须在事务内、且**在主 UPDATE 之后**调用：
//   - 事务内：否则读到的是别的连接可能已改过的值，合并会丢掉并发写；
//   - 主 UPDATE 之后：那条 UPDATE 的 WHERE 里用 `credentials_api_key_hash IS DISTINCT FROM $n`
//     判"api_key 是否变了"，必须先按旧行求值（见 account_repo.go 的注释）。
func mergeAccountCredentialsInTx(
	ctx context.Context,
	exec credentialColumnUpdater,
	cryptor service.SecretEncryptor,
	ids []int64,
	delta map[string]any,
) error {
	if len(ids) == 0 || len(delta) == 0 {
		return nil
	}
	if cryptor == nil {
		// 没有加密器就写不了密文；静默写 NULL 等于**把凭据删掉**。
		return fmt.Errorf("merge credentials: no encryptor configured (refusing to drop credentials)")
	}

	rows, err := exec.QueryContext(ctx, `
		SELECT id, credentials_encrypted
		FROM accounts
		WHERE id = ANY($1) AND deleted_at IS NULL
		FOR UPDATE`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("merge credentials: select: %w", err)
	}
	type mergedRow struct {
		id   int64
		cols CredentialCryptoColumns
	}
	batch := make([]mergedRow, 0, len(ids))
	for rows.Next() {
		var id int64
		var encrypted *string
		if err := rows.Scan(&id, &encrypted); err != nil {
			_ = rows.Close()
			return fmt.Errorf("merge credentials: scan: %w", err)
		}
		base := map[string]any{}
		if encrypted != nil && *encrypted != "" {
			plaintext, derr := cryptor.Decrypt(*encrypted)
			if derr != nil {
				_ = rows.Close()
				return fmt.Errorf("merge credentials: account %d: decrypt: %w", id, derr)
			}
			if uerr := json.Unmarshal([]byte(plaintext), &base); uerr != nil {
				_ = rows.Close()
				return fmt.Errorf("merge credentials: account %d: unmarshal: %w", id, uerr)
			}
		}
		// jsonb `||` 是顶层浅合并：同名键取右侧，其余保留。
		for k, v := range delta {
			base[k] = v
		}
		cols, cerr := CredentialCryptoColumnsFor(base, cryptor)
		if cerr != nil {
			_ = rows.Close()
			return fmt.Errorf("merge credentials: account %d: derive: %w", id, cerr)
		}
		batch = append(batch, mergedRow{id: id, cols: cols})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("merge credentials: rows: %w", err)
	}
	_ = rows.Close()

	for _, m := range batch {
		metaJSON, merr := json.Marshal(m.cols.Meta)
		if merr != nil {
			return fmt.Errorf("merge credentials: account %d: marshal meta: %w", m.id, merr)
		}
		var encArg, hashArg, apiKeyHashArg any
		if m.cols.Encrypted != nil {
			encArg = *m.cols.Encrypted
		}
		if m.cols.Hash != nil {
			hashArg = *m.cols.Hash
		}
		if m.cols.APIKeyHash != nil {
			apiKeyHashArg = *m.cols.APIKeyHash
		}
		if _, err := exec.ExecContext(ctx, `
			UPDATE accounts
			SET credentials_meta = $2::jsonb,
			    credentials_encrypted = $3,
			    credentials_hash = $4,
			    credentials_api_key_hash = $5
			WHERE id = $1`, m.id, string(metaJSON), encArg, hashArg, apiKeyHashArg); err != nil {
			return fmt.Errorf("merge credentials: account %d: write: %w", m.id, err)
		}
	}
	return nil
}
