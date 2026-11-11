package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 存量回填：把 accounts / api_keys 里"只有明文"的行补上派生列。
//
// 为什么这个工具必须在**阶段二之前**跑完（见 design 与 tasks 2.0c）：
// 阶段二会把 SQL 谓词从 `credentials IS DISTINCT FROM $1::jsonb` 换成
// `credentials_hash IS DISTINCT FROM $1`。而 `NULL IS DISTINCT FROM x` 恒为真，
// 所以只要有一行的 hash 还是 NULL，它就会被当成"整份凭据已变化"——
// 触发上游探测/用量快照的清理分支，把没坏的受管状态清掉。
// 也就是说：不是"回填最好先做"，而是**不先做就会误清数据**。
//
// 派生逻辑与运行时共用 CredentialCryptoColumnsFor，避免两边各算一套哈希。

// CredentialBackfillOptions 控制回填范围与安全性。
type CredentialBackfillOptions struct {
	// BatchSize 每批处理的行数（keyset 分页，按 id 递增）。<=0 用默认值。
	BatchSize int
	// Limit 本次最多处理多少行（0 = 不限）。用于在生产上先小批量试跑。
	Limit int
	// DryRun 只统计不写库。生产首次执行强烈建议先跑一遍。
	DryRun bool
	// Logger 可选，用于逐批进度输出。
	Logger func(format string, args ...any)
}

// CredentialBackfillResult 汇总一次回填的结果。
type CredentialBackfillResult struct {
	AccountsScanned int
	AccountsUpdated int
	AccountsFailed  int
	APIKeysScanned  int
	APIKeysUpdated  int
	// APIKeysSkipped 是空 key 这类无法产生指纹的行。**不是失败**：
	// 真实库里可能有历史遗留的空 key，把它算失败会让回填工具报错退出，
	// 从而卡住本该通过的发布闸门。
	APIKeysSkipped int
	APIKeysFailed  int
}

const defaultCredentialBackfillBatchSize = 200

// BackfillCredentialColumns 补齐 accounts 与 api_keys 的派生列。
//
// 幂等：只选"还缺东西"的行，重复执行不会重复写。
// 不删不改明文列：明文列是回滚路径，只有阶段四才会 drop。
func BackfillCredentialColumns(
	ctx context.Context,
	db *sql.DB,
	cryptor service.SecretEncryptor,
	opts CredentialBackfillOptions,
) (CredentialBackfillResult, error) {
	var result CredentialBackfillResult
	if db == nil {
		return result, fmt.Errorf("backfill: nil db")
	}
	if cryptor == nil {
		// 没有加密器时可以补 meta 与指纹，但补不了密文 —— 那样阶段二之后
		// 读路径永远回落明文，等于白做。这里直接拒绝，避免跑出一半状态。
		return result, fmt.Errorf("backfill: encryptor is required (otherwise credentials_encrypted stays NULL)")
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultCredentialBackfillBatchSize
	}
	logf := opts.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if err := backfillAccountCredentialColumns(ctx, db, cryptor, batchSize, opts, logf, &result); err != nil {
		return result, err
	}
	if err := backfillAPIKeyColumns(ctx, db, cryptor, batchSize, opts, logf, &result); err != nil {
		return result, err
	}
	return result, nil
}

// credentialNeedsBackfillSQL 是"这一行还缺派生列"的唯一判据。
//
// 注意 meta 不参与判据：白名单子键本来就可能为空，`credentials_meta = '{}'`
// 是合法终态，拿它当"没迁完"会让回填永远跑不完。
const credentialNeedsBackfillSQL = `(credentials_encrypted IS NULL OR credentials_hash IS NULL)`

func backfillAccountCredentialColumns(
	ctx context.Context,
	db *sql.DB,
	cryptor service.SecretEncryptor,
	batchSize int,
	opts CredentialBackfillOptions,
	logf func(string, ...any),
	result *CredentialBackfillResult,
) error {
	remaining := opts.Limit
	lastID := int64(0)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		limit := batchSize
		if remaining > 0 && remaining < limit {
			limit = remaining
		}
		rows, err := db.QueryContext(ctx, `
			SELECT id, credentials
			FROM accounts
			WHERE id > $1 AND `+credentialNeedsBackfillSQL+`
			ORDER BY id
			LIMIT $2`, lastID, limit)
		if err != nil {
			return fmt.Errorf("backfill accounts: select: %w", err)
		}
		type row struct {
			id          int64
			credentials []byte
		}
		batch := make([]row, 0, limit)
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.credentials); err != nil {
				_ = rows.Close()
				return fmt.Errorf("backfill accounts: scan: %w", err)
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("backfill accounts: rows: %w", err)
		}
		_ = rows.Close()

		if len(batch) == 0 {
			return nil
		}
		lastID = batch[len(batch)-1].id

		for _, r := range batch {
			result.AccountsScanned++
			creds := map[string]any{}
			if len(r.credentials) > 0 {
				if err := json.Unmarshal(r.credentials, &creds); err != nil {
					// 单行坏数据不该中断整批：记失败、继续。
					result.AccountsFailed++
					logf("[CredentialBackfill] account %d: unmarshal credentials: %v (skipped)", r.id, err)
					continue
				}
			}
			columns, err := CredentialCryptoColumnsFor(creds, cryptor)
			if err != nil {
				result.AccountsFailed++
				logf("[CredentialBackfill] account %d: derive columns: %v (skipped)", r.id, err)
				continue
			}
			metaJSON, err := json.Marshal(columns.Meta)
			if err != nil {
				result.AccountsFailed++
				logf("[CredentialBackfill] account %d: marshal meta: %v (skipped)", r.id, err)
				continue
			}
			if opts.DryRun {
				result.AccountsUpdated++
				continue
			}
			res, err := db.ExecContext(ctx, `
				UPDATE accounts
				SET credentials_meta = $2::jsonb,
					credentials_encrypted = $3,
					credentials_hash = $4,
					credentials_api_key_hash = $5
				WHERE id = $1 AND `+credentialNeedsBackfillSQL,
				r.id, string(metaJSON), columns.Encrypted, columns.Hash, columns.APIKeyHash,
			)
			if err != nil {
				result.AccountsFailed++
				logf("[CredentialBackfill] account %d: update: %v", r.id, err)
				continue
			}
			// 只有真的写进去才算 updated。回填是在**活库**上跑的：语句里的谓词
			// 就是防覆盖的闸（应用刚写过派生列 → 谓词为假 → 0 行），此时计 updated
			// 会让"updated=5000"变成一个没人能核对的数。
			if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
				continue
			}
			result.AccountsUpdated++
		}

		if remaining > 0 {
			remaining -= len(batch)
			if remaining <= 0 {
				return nil
			}
		}
		if len(batch) < limit {
			return nil
		}
	}
}

// apiKeyNeedsBackfillSQL 排除**空 key**：空串没有可标注的价值（不产生指纹、不产生密文），
// 若把它算作"待回填"，计数永远归不了零 —— CLI 的收尾自检会因此一直 exit 1、
// 并打印"阶段二 MUST NOT 上线"，把一个本来健康的库挡在发布门外面。
const apiKeyNeedsBackfillSQL = `(key <> '' AND (key_encrypted IS NULL OR key_hash IS NULL))`

func backfillAPIKeyColumns(
	ctx context.Context,
	db *sql.DB,
	cryptor service.SecretEncryptor,
	batchSize int,
	opts CredentialBackfillOptions,
	logf func(string, ...any),
	result *CredentialBackfillResult,
) error {
	remaining := opts.Limit
	lastID := int64(0)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		limit := batchSize
		if remaining > 0 && remaining < limit {
			limit = remaining
		}
		rows, err := db.QueryContext(ctx, `
			SELECT id, key
			FROM api_keys
			WHERE id > $1 AND `+apiKeyNeedsBackfillSQL+`
			ORDER BY id
			LIMIT $2`, lastID, limit)
		if err != nil {
			return fmt.Errorf("backfill api_keys: select: %w", err)
		}
		type row struct {
			id  int64
			key string
		}
		batch := make([]row, 0, limit)
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.key); err != nil {
				_ = rows.Close()
				return fmt.Errorf("backfill api_keys: scan: %w", err)
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("backfill api_keys: rows: %w", err)
		}
		_ = rows.Close()

		if len(batch) == 0 {
			return nil
		}
		lastID = batch[len(batch)-1].id

		for _, r := range batch {
			result.APIKeysScanned++
			if r.key == "" {
				// 空 key 不产生指纹：唯一索引允许多个 NULL，也不会误判重复。
				// 记 Skipped 而不是 Failed——空 key 是**形态问题不是故障**，
				// 算失败会让 CLI 退出 1 并卡住发布闸门。
				result.APIKeysSkipped++
				logf("[CredentialBackfill] api_key %d: empty key (skipped)", r.id)
				continue
			}
			hash := sha256Hex([]byte(r.key))
			encrypted, err := cryptor.Encrypt(r.key)
			if err != nil {
				result.APIKeysFailed++
				logf("[CredentialBackfill] api_key %d: encrypt: %v (skipped)", r.id, err)
				continue
			}
			if opts.DryRun {
				result.APIKeysUpdated++
				continue
			}
			res, err := db.ExecContext(ctx, `
				UPDATE api_keys
				SET key_hash = $2, key_encrypted = $3
				WHERE id = $1 AND `+apiKeyNeedsBackfillSQL,
				r.id, hash, encrypted,
			)
			if err != nil {
				result.APIKeysFailed++
				logf("[CredentialBackfill] api_key %d: update: %v", r.id, err)
				continue
			}
			// 同上：谓词为假（并发方已补好）时 0 行，不算 updated。
			if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
				continue
			}
			result.APIKeysUpdated++
		}

		if remaining > 0 {
			remaining -= len(batch)
			if remaining <= 0 {
				return nil
			}
		}
		if len(batch) < limit {
			return nil
		}
	}
}

// CountCredentialBackfillPending 返回仍缺派生列的行数，供命令行工具做收尾自检。
// 与回填用的判据是同一组常量——两边如果各写一套，"自检通过"就说明不了任何事。
func CountCredentialBackfillPending(ctx context.Context, db *sql.DB) (accounts int64, apiKeys int64, err error) {
	if db == nil {
		return 0, 0, fmt.Errorf("count pending: nil db")
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE `+credentialNeedsBackfillSQL).Scan(&accounts); err != nil {
		return 0, 0, fmt.Errorf("count pending accounts: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE `+apiKeyNeedsBackfillSQL).Scan(&apiKeys); err != nil {
		return 0, 0, fmt.Errorf("count pending api_keys: %w", err)
	}
	return accounts, apiKeys, nil
}
