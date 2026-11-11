package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

type apiKeyRepository struct {
	client *dbent.Client
	sql    sqlExecutor
	// cryptor 负责 key_encrypted 的加解密。nil 是合法值 = 只写 key_hash、密文列留 NULL。
	// 与 accounts 同一模式（design D1a）：不改既有构造函数签名，避免 17 个调用点跟着动。
	cryptor service.SecretEncryptor
}

func NewAPIKeyRepository(client *dbent.Client, sqlDB *sql.DB) service.APIKeyRepository {
	return newAPIKeyRepositoryWithSQL(client, sqlDB)
}

func newAPIKeyRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor) *apiKeyRepository {
	return newAPIKeyRepositoryWithSQLCrypto(client, sqlq, nil)
}

// NewAPIKeyRepositoryCrypto 是带加密器的生产构造入口（wire 用）。
// 不传加密器时 key_encrypted 保持 NULL，只有 key_hash 会写——那是可接受的中间态
// （阶段三 3.5b 才依赖密文），但生产不应该是这个状态。
func NewAPIKeyRepositoryCrypto(client *dbent.Client, sqlDB *sql.DB, cryptor service.SecretEncryptor) service.APIKeyRepository {
	return newAPIKeyRepositoryWithSQLCrypto(client, sqlDB, cryptor)
}

func newAPIKeyRepositoryWithSQLCrypto(client *dbent.Client, sqlq sqlExecutor, cryptor service.SecretEncryptor) *apiKeyRepository {
	return &apiKeyRepository{client: client, sql: sqlq, cryptor: cryptor}
}

func (r *apiKeyRepository) activeQuery() *dbent.APIKeyQuery {
	// 默认过滤已软删除记录，避免删除后仍被查询到。
	return r.client.APIKey.Query().Where(apikey.DeletedAtIsNil())
}

func (r *apiKeyRepository) Create(ctx context.Context, key *service.APIKey) error {
	builder := r.client.APIKey.Create().
		SetUserID(key.UserID).
		SetKey(key.Key).
		SetName(key.Name).
		SetStatus(key.Status).
		SetNillableGroupID(key.GroupID).
		SetNillableLastUsedAt(key.LastUsedAt).
		SetQuota(key.Quota).
		SetQuotaUsed(key.QuotaUsed).
		SetNillableExpiresAt(key.ExpiresAt).
		SetRateLimit5h(key.RateLimit5h).
		SetRateLimit1d(key.RateLimit1d).
		SetRateLimit7d(key.RateLimit7d)

	if len(key.IPWhitelist) > 0 {
		builder.SetIPWhitelist(key.IPWhitelist)
	}
	if len(key.IPBlacklist) > 0 {
		builder.SetIPBlacklist(key.IPBlacklist)
	}

	// 阶段三 3.5a：新发的 key 必须同时落指纹（以及配了加密器时的密文），
	// 否则按 key_hash 查找会在"新建的 key"上立刻失效（回填只补存量）。
	keyHash, keyEncrypted, cryptoErr := apiKeyDerivedColumns(key.Key, r.cryptor)
	if cryptoErr != nil {
		return cryptoErr
	}
	if keyHash != "" {
		builder.SetKeyHash(keyHash)
	}
	if keyEncrypted != nil {
		builder.SetKeyEncrypted(*keyEncrypted)
	}

	created, err := builder.Save(ctx)
	if err == nil {
		key.ID = created.ID
		key.LastUsedAt = created.LastUsedAt
		key.CreatedAt = created.CreatedAt
		key.UpdatedAt = created.UpdatedAt
	}
	return translatePersistenceError(err, nil, service.ErrAPIKeyExists)
}

func (r *apiKeyRepository) GetByID(ctx context.Context, id int64) (*service.APIKey, error) {
	m, err := r.activeQuery().
		Where(apikey.IDEQ(id)).
		WithUser().
		WithGroup().
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return r.apiKeyEntityToServiceResolvingKey(m), nil
}

// GetKeyAndOwnerID 根据 API Key ID 获取其 key 与所有者（用户）ID。
// 相比 GetByID，此方法性能更优，因为：
//   - 使用 Select() 只查询必要字段，减少数据传输量
//   - 不加载完整的 API Key 实体及其关联数据（User、Group 等）
//   - 适用于删除等只需 key 与用户 ID 的场景
func (r *apiKeyRepository) GetKeyAndOwnerID(ctx context.Context, id int64) (string, int64, error) {
	m, err := r.activeQuery().
		Where(apikey.IDEQ(id)).
		Select(apikey.FieldKey, apikey.FieldKeyEncrypted, apikey.FieldUserID).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return "", 0, service.ErrAPIKeyNotFound
		}
		return "", 0, err
	}
	// 走权威明文：drop 明文列之后 `key` 列不存在，这里能拿到的只有密文。
	plaintext, err := r.apiKeyAuthoritativePlaintext(m)
	if err != nil {
		return "", 0, err
	}
	return plaintext, m.UserID, nil
}

func (r *apiKeyRepository) GetByKey(ctx context.Context, key string) (*service.APIKey, error) {
	apply := func(q *dbent.APIKeyQuery) *dbent.APIKeyQuery {
		return q.
			WithUser(func(q *dbent.UserQuery) {
				q.WithAllowedGroups(func(gq *dbent.GroupQuery) {
					gq.Select(group.FieldID)
				})
			}).
			WithGroup()
	}
	m, err := r.lookupAPIKeyByHashThenPlaintext(ctx, key, apply)
	if err != nil {
		return nil, err
	}
	return apiKeyEntityToService(m), nil
}

// lookupAPIKeyByHashThenPlaintext 是鉴权路径的唯一查找实现（阶段三 3.5b）。
//
// 顺序（顺序即语义）：
//  1. 先按 key_hash 定位——这是加密落盘之后**唯一**不依赖明文列的查找方式；
//  2. 命中后 MUST 再拿明文做一次常量时间比对。指纹只是索引，不是凭据：
//     指纹与明文脱节（旧数据算错、写路径漏带、人工改库）时必须判"查不到"，
//     绝不能因为指纹撞上就放行。这是"选可验证更严的一侧"。
//  3. 指纹没命中 → 回落明文查找（该行尚未回填指纹），并计数。
//     回填完成、且所有写路径都带指纹之后，这个分支可以删掉。
func (r *apiKeyRepository) lookupAPIKeyByHashThenPlaintext(
	ctx context.Context,
	key string,
	apply func(*dbent.APIKeyQuery) *dbent.APIKeyQuery,
) (*dbent.APIKey, error) {
	if key == "" {
		return nil, service.ErrAPIKeyNotFound
	}

	hash := sha256Hex([]byte(key))
	byHash, err := apply(r.activeQuery().Where(apikey.KeyHashEQ(hash))).Only(ctx)
	switch {
	case err == nil:
		// 复核要用的明文来自**密文**（drop 明文列之后那是唯一副本）；
		// 这里比对的是"权威明文 vs 入参"，不是"明文列 vs 入参"。
		plaintext, perr := r.apiKeyAuthoritativePlaintext(byHash)
		if perr != nil {
			logger.LegacyPrintf("repository.apikey",
				"[APIKeyLookup] cannot resolve plaintext for id=%d: %v; rejecting", byHash.ID, perr)
			return nil, service.ErrAPIKeyNotFound
		}
		if plaintext == "" || !constantTimeEqualAPIKey(plaintext, key) {
			// 指纹命中但明文不等：拒绝，而不是回落明文给它第二次机会。
			// 回落会让"指纹算错的坏行"变成可认证，那正是要防的方向。
			logger.LegacyPrintf("repository.apikey",
				"[APIKeyLookup] key_hash matched but plaintext differs (id=%d); rejecting", byHash.ID)
			return nil, service.ErrAPIKeyNotFound
		}
		// 把权威明文放回实体：调用方（掩码/日志/缓存键）拿到的必须是明文，
		// drop 之后它只能来自解密。
		byHash.Key = plaintext
		return byHash, nil
	case !dbent.IsNotFound(err):
		return nil, err
	}

	byPlaintext, err := apply(r.activeQuery().Where(apikey.KeyEQ(key))).Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, err
	}
	apiKeyPlaintextLookupFallbacks.Add(1)
	logger.LegacyPrintf("repository.apikey",
		"[APIKeyLookup] plaintext fallback: id=%d has no usable key_hash yet", byPlaintext.ID)
	return byPlaintext, nil
}

func (r *apiKeyRepository) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	apply := func(q *dbent.APIKeyQuery) *dbent.APIKeyQuery {
		return q.Select(
			apikey.FieldID,
			// 复核需要明文：指纹只用于定位，命中后必须再比对一次（见 lookup 的注释）。
			// 明文**优先取自密文**，所以这里必须一起选出 key_encrypted——
			// 漏选会让权威明文退化成明文列，drop 之后就没人可退了。
			apikey.FieldKey,
			apikey.FieldKeyEncrypted,
			apikey.FieldUserID,
			apikey.FieldGroupID,
			apikey.FieldName,
			apikey.FieldStatus,
			apikey.FieldIPWhitelist,
			apikey.FieldIPBlacklist,
			apikey.FieldQuota,
			apikey.FieldQuotaUsed,
			apikey.FieldExpiresAt,
			apikey.FieldRateLimit5h,
			apikey.FieldRateLimit1d,
			apikey.FieldRateLimit7d,
		).
			WithUser(func(q *dbent.UserQuery) {
				q.Select(
					user.FieldID,
					user.FieldEmail,
					user.FieldUsername,
					user.FieldStatus,
					user.FieldRole,
					user.FieldBalance,
					user.FieldConcurrency,
					user.FieldBalanceNotifyEnabled,
					user.FieldRestrictPublicGroups,
					user.FieldBalanceNotifyThresholdType,
					user.FieldBalanceNotifyThreshold,
					user.FieldBalanceNotifyExtraEmails,
					user.FieldTotalRecharged,
					user.FieldSignupSource,
					user.FieldLastLoginAt,
					user.FieldLastActiveAt,
					user.FieldRpmLimit,
				)
				q.WithAllowedGroups(func(gq *dbent.GroupQuery) {
					gq.Select(group.FieldID)
				})
			}).
			WithGroup(func(q *dbent.GroupQuery) {
				q.Select(
					group.FieldID,
					group.FieldName,
					group.FieldPlatform,
					group.FieldIsExclusive,
					group.FieldStatus,
					group.FieldSubscriptionType,
					group.FieldRateMultiplier,
					group.FieldDailyLimitUsd,
					group.FieldWeeklyLimitUsd,
					group.FieldMonthlyLimitUsd,
					group.FieldAllowImageGeneration,
					group.FieldAllowBatchImageGeneration,
					group.FieldImageCountBilling,
					group.FieldImageRateIndependent,
					group.FieldImageRateMultiplier,
					group.FieldImagePrice1k,
					group.FieldImagePrice2k,
					group.FieldImagePrice4k,
					group.FieldVideoRateIndependent,
					group.FieldVideoRateMultiplier,
					group.FieldVideoPrice480p,
					group.FieldVideoPrice720p,
					group.FieldVideoPrice1080p,
					group.FieldVideoModelPrices,
					group.FieldWebSearchPricePerCall,
					group.FieldRerankPricePerRequest,
					group.FieldSearchPricePer1k,
					group.FieldAudioRealtimePricePerMin,
					group.FieldAudioTtsPricePerMillionChars,
					group.FieldAudioSttPricePerHour,
					group.FieldLongContextPricingEnabled,
					group.FieldModelPricing,
					group.FieldClaudeCodeOnly,
					group.FieldFallbackGroupID,
					group.FieldFallbackGroupIDOnInvalidRequest,
					group.FieldModelRoutingEnabled,
					group.FieldModelRouting,
					group.FieldMcpXMLInject,
					group.FieldSupportedModelScopes,
					group.FieldAllowMessagesDispatch,
					group.FieldAllowLive,
					group.FieldForceOpenaiFast,
					group.FieldFreeOpenaiFast,
					group.FieldDefaultMappedModel,
					group.FieldMessagesDispatchModelConfig,
					group.FieldModelAllowlist,
					group.FieldCodexModelsManifestConfig,
					group.FieldRpmLimit,
					group.FieldMaxReasoningEffort,
					group.FieldMaxReasoningEffortOverLimit,
					group.FieldReasoningEffortMappings,
					group.FieldPeakRateEnabled,
					group.FieldPeakStart,
					group.FieldPeakEnd,
					group.FieldPeakRateMultiplier,
					// 分组利润控制：认证快照是调度门 enable 判定的直接来源，
					// 漏选会让门静默失效；新增快照分组字段时必须同步本投影，
					// 集成测试对账兜底。
					group.FieldProfitControlEnabled,
					group.FieldProfitMinMargin,
					group.FieldProfitSafetyBuffer,
				)
			})
	}
	m, err := r.lookupAPIKeyByHashThenPlaintext(ctx, key, apply)
	if err != nil {
		return nil, err
	}
	return apiKeyEntityToService(m), nil
}

func (r *apiKeyRepository) Update(ctx context.Context, key *service.APIKey, fields service.APIKeyUpdateFields) error {
	// 空掩码代表调用方不改任何列，直接返回，避免产生一次无意义的整行写。
	if fields.IsEmpty() {
		return nil
	}

	// 使用原子操作：将软删除检查与更新合并到同一语句，避免竞态条件。
	// 之前的实现先检查 Exist 再 UpdateOneID，若在两步之间发生软删除，
	// 则会更新已删除的记录。
	// 这里选择 Update().Where()，确保只有未软删除记录能被更新。
	// 同时显式设置 updated_at，避免二次查询带来的并发可见性问题。
	client := clientFromContext(ctx, r.client)
	now := time.Now()
	builder := client.APIKey.Update().
		Where(apikey.IDEQ(key.ID), apikey.DeletedAtIsNil()).
		SetUpdatedAt(now)
	if fields.Name {
		builder.SetName(key.Name)
	}
	if fields.Status {
		builder.SetStatus(key.Status)
	}
	if fields.Quota {
		builder.SetQuota(key.Quota)
	}
	if fields.QuotaUsed {
		builder.SetQuotaUsed(key.QuotaUsed)
	}
	if fields.RateLimits {
		builder.
			SetRateLimit5h(key.RateLimit5h).
			SetRateLimit1d(key.RateLimit1d).
			SetRateLimit7d(key.RateLimit7d)
	}
	if fields.RateLimitUsage {
		builder.
			SetUsage5h(key.Usage5h).
			SetUsage1d(key.Usage1d).
			SetUsage7d(key.Usage7d)

		// Rate limit window start times
		if key.Window5hStart != nil {
			builder.SetWindow5hStart(*key.Window5hStart)
		} else {
			builder.ClearWindow5hStart()
		}
		if key.Window1dStart != nil {
			builder.SetWindow1dStart(*key.Window1dStart)
		} else {
			builder.ClearWindow1dStart()
		}
		if key.Window7dStart != nil {
			builder.SetWindow7dStart(*key.Window7dStart)
		} else {
			builder.ClearWindow7dStart()
		}
	}
	if fields.GroupID {
		if key.GroupID != nil {
			builder.SetGroupID(*key.GroupID)
		} else {
			builder.ClearGroupID()
		}
	}

	// Expiration time
	if fields.ExpiresAt {
		if key.ExpiresAt != nil {
			builder.SetExpiresAt(*key.ExpiresAt)
		} else {
			builder.ClearExpiresAt()
		}
	}

	// IP 限制字段
	if fields.IPRules {
		if len(key.IPWhitelist) > 0 {
			builder.SetIPWhitelist(key.IPWhitelist)
		} else {
			builder.ClearIPWhitelist()
		}
		if len(key.IPBlacklist) > 0 {
			builder.SetIPBlacklist(key.IPBlacklist)
		} else {
			builder.ClearIPBlacklist()
		}
	}

	affected, err := builder.Save(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		// 更新影响行数为 0，说明记录不存在或已被软删除。
		return service.ErrAPIKeyNotFound
	}

	// 使用同一时间戳回填，避免并发删除导致二次查询失败。
	key.UpdatedAt = now
	return nil
}

func (r *apiKeyRepository) Delete(ctx context.Context, id int64) error {
	// 存在唯一键约束 生成tombstone key 用来释放原key，长度远小于 128，满足 schema 限制
	tombstoneKey := fmt.Sprintf("__deleted__%d__%d", id, time.Now().UnixNano())
	// 显式软删除：避免依赖 Hook 行为，确保 deleted_at 一定被设置。
	// tombstone 会改写明文 key，所以派生列 MUST 一起改：
	// 留着旧指纹等于"已删除的 key 仍能被 key_hash 命中"。
	tombstoneHash, tombstoneEncrypted, cryptoErr := apiKeyDerivedColumns(tombstoneKey, r.cryptor)
	if cryptoErr != nil {
		return cryptoErr
	}
	update := r.client.APIKey.Update().
		Where(apikey.IDEQ(id), apikey.DeletedAtIsNil()).
		SetKey(tombstoneKey).
		SetDeletedAt(time.Now())
	if tombstoneHash != "" {
		update = update.SetKeyHash(tombstoneHash)
	}
	if tombstoneEncrypted != nil {
		update = update.SetKeyEncrypted(*tombstoneEncrypted)
	} else {
		// 没有加密器时显式清空，避免留下与明文不一致的旧密文
		update = update.ClearKeyEncrypted()
	}
	affected, err := update.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrAPIKeyNotFound
		}
		return err
	}
	if affected == 0 {
		exists, err := r.client.APIKey.Query().
			Where(apikey.IDEQ(id)).
			Exist(mixins.SkipSoftDelete(ctx))
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		return service.ErrAPIKeyNotFound
	}
	return nil
}

// DeleteWithAudit keeps the legacy method name for rolling-upgrade compatibility.
// It atomically tombstones and soft-deletes the key without retaining credential
// material. Tombstoning releases the unique key value for safe reuse.
func (r *apiKeyRepository) DeleteWithAudit(ctx context.Context, id int64) error {
	tombstoneKey := fmt.Sprintf("__deleted__%d__%d", id, time.Now().UnixNano())

	if existingTx := dbent.TxFromContext(ctx); existingTx != nil {
		return r.deleteWithTombstone(ctx, existingTx.Client(), id, tombstoneKey)
	}

	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}
	exec := r.client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		exec = tx.Client()
	}

	if err := r.deleteWithTombstone(ctx, exec, id, tombstoneKey); err != nil {
		return err
	}

	if tx != nil {
		return tx.Commit()
	}
	return nil
}

func (r *apiKeyRepository) deleteWithTombstone(ctx context.Context, exec *dbent.Client, id int64, tombstoneKey string) error {
	res, err := exec.ExecContext(ctx, `
		UPDATE api_keys
		SET key = $1, deleted_at = NOW(), updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL`, tombstoneKey, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// 并发/重复删除:记录已存在(已软删)则幂等返回 nil(defer 回滚空事务),否则 NotFound。
		exists, existErr := r.client.APIKey.Query().
			Where(apikey.IDEQ(id)).
			Exist(mixins.SkipSoftDelete(ctx))
		if existErr != nil {
			return existErr
		}
		if exists {
			return nil
		}
		return service.ErrAPIKeyNotFound
	}
	return nil
}

func (r *apiKeyRepository) apiKeyListByUserIDQuery(userID int64, filters service.APIKeyListFilters) *dbent.APIKeyQuery {
	q := r.activeQuery().Where(apikey.UserIDEQ(userID))

	if filters.Search != "" {
		q = q.Where(apikey.Or(
			apikey.NameContainsFold(filters.Search),
			apikey.KeyContainsFold(filters.Search),
		))
	}
	if filters.Status != "" {
		q = q.Where(apikey.StatusEQ(filters.Status))
	}
	if filters.GroupID != nil {
		if *filters.GroupID == 0 {
			q = q.Where(apikey.GroupIDIsNil())
		} else {
			q = q.Where(apikey.GroupIDEQ(*filters.GroupID))
		}
	}

	return q
}

func (r *apiKeyRepository) ListByUserID(ctx context.Context, userID int64, params pagination.PaginationParams, filters service.APIKeyListFilters) ([]service.APIKey, *pagination.PaginationResult, error) {
	q := r.apiKeyListByUserIDQuery(userID, filters)

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	keysQuery := q.
		WithGroup().
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range apiKeyListOrder(params) {
		keysQuery = keysQuery.Order(order)
	}

	keys, err := keysQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *r.apiKeyEntityToServiceResolvingKey(keys[i]))
	}
	if err := r.attachLastUsedIPs(ctx, outKeys); err != nil {
		return nil, nil, err
	}

	return outKeys, paginationResultFromTotal(int64(total), params), nil
}

func (r *apiKeyRepository) ListAllByUserID(ctx context.Context, userID int64, filters service.APIKeyListFilters) ([]service.APIKey, error) {
	keys, err := r.apiKeyListByUserIDQuery(userID, filters).
		WithGroup().
		Order(dbent.Asc(apikey.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *r.apiKeyEntityToServiceResolvingKey(keys[i]))
	}
	if err := r.attachLastUsedIPs(ctx, outKeys); err != nil {
		return nil, err
	}
	return outKeys, nil
}

func (r *apiKeyRepository) attachLastUsedIPs(ctx context.Context, keys []service.APIKey) error {
	if len(keys) == 0 || r.sql == nil {
		return nil
	}

	apiKeyIDs := make([]int64, 0, len(keys))
	for i := range keys {
		apiKeyIDs = append(apiKeyIDs, keys[i].ID)
	}

	lastUsedIPs, err := r.latestUsageLogIPs(ctx, apiKeyIDs)
	if err != nil {
		return err
	}
	for i := range keys {
		if ip, ok := lastUsedIPs[keys[i].ID]; ok {
			keys[i].LastUsedIP = &ip
		}
	}
	return nil
}

func (r *apiKeyRepository) latestUsageLogIPs(ctx context.Context, apiKeyIDs []int64) (result map[int64]string, err error) {
	if len(apiKeyIDs) == 0 || r.sql == nil {
		return map[int64]string{}, nil
	}

	query, args := latestUsageLogIPsQuery(apiKeyIDs, r.client.Driver().Dialect())
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	out := make(map[int64]string, len(apiKeyIDs))
	for rows.Next() {
		var apiKeyID int64
		var ipAddress string
		if err := rows.Scan(&apiKeyID, &ipAddress); err != nil {
			return nil, err
		}
		out[apiKeyID] = ipAddress
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func latestUsageLogIPsQuery(apiKeyIDs []int64, dialectName string) (string, []any) {
	if dialectName == dialect.Postgres {
		// Keep each key lookup bounded to one ordered index probe instead of ranking its full history.
		return `
		SELECT requested.api_key_id, latest.ip_address
		FROM unnest($1::bigint[]) AS requested(api_key_id)
		CROSS JOIN LATERAL (
			SELECT ul.ip_address
			FROM usage_logs AS ul
			WHERE ul.api_key_id = requested.api_key_id
				AND ul.ip_address IS NOT NULL
				AND ul.ip_address <> ''
			ORDER BY ul.created_at DESC, ul.id DESC
			LIMIT 1
		) AS latest`, []any{pq.Array(apiKeyIDs)}
	}

	placeholders := make([]string, len(apiKeyIDs))
	args := make([]any, len(apiKeyIDs))
	for i, id := range apiKeyIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	return fmt.Sprintf(`
		SELECT api_key_id, ip_address
		FROM (
			SELECT api_key_id, ip_address,
				ROW_NUMBER() OVER (PARTITION BY api_key_id ORDER BY created_at DESC, id DESC) AS rn
			FROM usage_logs
			WHERE api_key_id IN (%s)
				AND ip_address IS NOT NULL
				AND ip_address <> ''
		) ranked
		WHERE rn = 1`, strings.Join(placeholders, ", ")), args
}

func (r *apiKeyRepository) VerifyOwnership(ctx context.Context, userID int64, apiKeyIDs []int64) ([]int64, error) {
	if len(apiKeyIDs) == 0 {
		return []int64{}, nil
	}

	ids, err := r.client.APIKey.Query().
		Where(apikey.UserIDEQ(userID), apikey.IDIn(apiKeyIDs...), apikey.DeletedAtIsNil()).
		IDs(ctx)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *apiKeyRepository) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	count, err := r.activeQuery().Where(apikey.UserIDEQ(userID)).Count(ctx)
	return int64(count), err
}

func (r *apiKeyRepository) ExistsByKey(ctx context.Context, key string) (bool, error) {
	count, err := r.activeQuery().Where(apikey.KeyEQ(key)).Count(ctx)
	return count > 0, err
}

func (r *apiKeyRepository) ListByGroupID(ctx context.Context, groupID int64, params pagination.PaginationParams) ([]service.APIKey, *pagination.PaginationResult, error) {
	q := r.activeQuery().Where(apikey.GroupIDEQ(groupID))

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	keysQuery := q.
		WithUser().
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range apiKeyListOrder(params) {
		keysQuery = keysQuery.Order(order)
	}

	keys, err := keysQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *r.apiKeyEntityToServiceResolvingKey(keys[i]))
	}

	return outKeys, paginationResultFromTotal(int64(total), params), nil
}

func apiKeyListOrder(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortBy := strings.ToLower(strings.TrimSpace(params.SortBy))
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderDesc)

	if sortBy == "group" {
		// Sort before pagination, keeping ungrouped keys last in either direction.
		opts := []entsql.OrderTermOption{entsql.OrderNullsLast()}
		tieOrder := dbent.Asc(apikey.FieldID)
		if sortOrder == pagination.SortOrderDesc {
			opts = append(opts, entsql.OrderDesc())
			tieOrder = dbent.Desc(apikey.FieldID)
		}
		return []func(*entsql.Selector){apikey.ByGroupField(group.FieldName, opts...), tieOrder}
	}

	var field string
	switch sortBy {
	case "name":
		field = apikey.FieldName
	case "status":
		field = apikey.FieldStatus
	case "expires_at":
		field = apikey.FieldExpiresAt
	case "last_used_at":
		field = apikey.FieldLastUsedAt
	case "created_at":
		field = apikey.FieldCreatedAt
	case "id":
		field = apikey.FieldID
	default:
		field = apikey.FieldID
	}

	if sortOrder == pagination.SortOrderAsc {
		orders := []func(*entsql.Selector){dbent.Asc(field)}
		if field != apikey.FieldID {
			orders = append(orders, dbent.Asc(apikey.FieldID))
		}
		return orders
	}
	orders := []func(*entsql.Selector){dbent.Desc(field)}
	if field != apikey.FieldID {
		orders = append(orders, dbent.Desc(apikey.FieldID))
	}
	return orders
}

// SearchAPIKeys searches API keys by user ID and/or keyword (name)
func (r *apiKeyRepository) SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]service.APIKey, error) {
	q := r.activeQuery()
	if userID > 0 {
		q = q.Where(apikey.UserIDEQ(userID))
	}

	if keyword != "" {
		q = q.Where(apikey.NameContainsFold(keyword))
	}

	keys, err := q.Limit(limit).Order(dbent.Desc(apikey.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *r.apiKeyEntityToServiceResolvingKey(keys[i]))
	}
	return outKeys, nil
}

// ClearGroupIDByGroupID 将指定分组的所有 API Key 的 group_id 设为 nil
func (r *apiKeyRepository) ClearGroupIDByGroupID(ctx context.Context, groupID int64) (int64, error) {
	n, err := r.client.APIKey.Update().
		Where(apikey.GroupIDEQ(groupID), apikey.DeletedAtIsNil()).
		ClearGroupID().
		Save(ctx)
	return int64(n), err
}

// UpdateGroupIDByUserAndGroup 将用户下绑定 oldGroupID 的所有 Key 迁移到 newGroupID
func (r *apiKeyRepository) UpdateGroupIDByUserAndGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error) {
	client := clientFromContext(ctx, r.client)
	n, err := client.APIKey.Update().
		Where(apikey.UserIDEQ(userID), apikey.GroupIDEQ(oldGroupID), apikey.DeletedAtIsNil()).
		SetGroupID(newGroupID).
		Save(ctx)
	return int64(n), err
}

// CountByGroupID 获取分组的 API Key 数量
func (r *apiKeyRepository) CountByGroupID(ctx context.Context, groupID int64) (int64, error) {
	count, err := r.activeQuery().Where(apikey.GroupIDEQ(groupID)).Count(ctx)
	return int64(count), err
}

func (r *apiKeyRepository) ListKeysByUserID(ctx context.Context, userID int64) ([]string, error) {
	keys, err := r.activeQuery().
		Where(apikey.UserIDEQ(userID)).
		Select(apikey.FieldKey).
		Strings(ctx)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (r *apiKeyRepository) ListKeysByGroupID(ctx context.Context, groupID int64) ([]string, error) {
	keys, err := r.activeQuery().
		Where(apikey.GroupIDEQ(groupID)).
		Select(apikey.FieldKey).
		Strings(ctx)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// IncrementQuotaUsed 使用 Ent 原子递增 quota_used 字段并返回新值
func (r *apiKeyRepository) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) (float64, error) {
	updated, err := r.client.APIKey.UpdateOneID(id).
		Where(apikey.DeletedAtIsNil()).
		AddQuotaUsed(amount).
		Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return 0, service.ErrAPIKeyNotFound
		}
		return 0, err
	}
	return updated.QuotaUsed, nil
}

// IncrementQuotaUsedAndGetState atomically increments quota_used, conditionally marks the key
// as quota_exhausted, and returns the latest quota state in one round trip.
func (r *apiKeyRepository) IncrementQuotaUsedAndGetState(ctx context.Context, id int64, amount float64) (*service.APIKeyQuotaUsageState, error) {
	query := `
		UPDATE api_keys
		SET
			quota_used = quota_used + $1,
			status = CASE
				WHEN quota > 0 AND quota_used + $1 >= quota THEN $2
				ELSE status
			END,
			updated_at = NOW()
		WHERE id = $3 AND deleted_at IS NULL
		RETURNING quota_used, quota, key_encrypted, status
	`

	state := &service.APIKeyQuotaUsageState{}
	var encrypted *string
	if scanErr := scanSingleRow(ctx, r.sql, query, []any{amount, service.StatusAPIKeyQuotaExhausted, id}, &state.QuotaUsed, &state.Quota, &encrypted, &state.Status); scanErr != nil {
		if scanErr == sql.ErrNoRows {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, scanErr
	}

	// state.Key 不是展示字段，它直接喂给 InvalidateAuthCacheByKey —— 而缓存键就是
	// sha256(明文)。取不到明文时服务层有个 `state.Key != ""` 的守卫会**静默跳过**失效，
	// 于是刚被置成 quota_exhausted 的 key 还能从缓存里继续放行到 TTL 结束。
	// 所以这里取不到明文必须打日志（可见），不能默默留空。
	plaintext, decryptErr := apiKeyPlaintextFromCiphertext(r.cryptor, id, encrypted)
	if decryptErr != nil {
		logger.LegacyPrintf("repository.apikey",
			"[CredentialAtRest] quota state for id=%d has no usable key_encrypted (%v): auth cache cannot be invalidated", id, decryptErr)
	}
	state.Key = plaintext
	return state, nil
}

func (r *apiKeyRepository) UpdateLastUsed(ctx context.Context, id int64, usedAt time.Time) error {
	affected, err := r.client.APIKey.Update().
		Where(apikey.IDEQ(id), apikey.DeletedAtIsNil()).
		SetLastUsedAt(usedAt).
		SetUpdatedAt(usedAt).
		Save(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAPIKeyNotFound
	}
	return nil
}

// IncrementRateLimitUsage atomically increments all rate limit usage counters and initializes
// window start times via COALESCE if not already set.
func (r *apiKeyRepository) IncrementRateLimitUsage(ctx context.Context, id int64, cost float64) error {
	_, err := r.sql.ExecContext(ctx, `
		UPDATE api_keys SET
			usage_5h = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN $1 ELSE usage_5h + $1 END,
			usage_1d = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN $1 ELSE usage_1d + $1 END,
			usage_7d = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN $1 ELSE usage_7d + $1 END,
			window_5h_start = CASE WHEN window_5h_start IS NULL OR window_5h_start + INTERVAL '5 hours' <= NOW() THEN NOW() ELSE window_5h_start END,
			window_1d_start = CASE WHEN window_1d_start IS NULL OR window_1d_start + INTERVAL '24 hours' <= NOW() THEN date_trunc('day', NOW()) ELSE window_1d_start END,
			window_7d_start = CASE WHEN window_7d_start IS NULL OR window_7d_start + INTERVAL '7 days' <= NOW() THEN date_trunc('day', NOW()) ELSE window_7d_start END,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL`,
		cost, id)
	return err
}

// ResetRateLimitWindows resets expired rate limit windows atomically.
func (r *apiKeyRepository) ResetRateLimitWindows(ctx context.Context, id int64) error {
	_, err := r.sql.ExecContext(ctx, `
		UPDATE api_keys SET
			usage_5h = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN 0 ELSE usage_5h END,
			window_5h_start = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN NOW() ELSE window_5h_start END,
			usage_1d = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN 0 ELSE usage_1d END,
			window_1d_start = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN date_trunc('day', NOW()) ELSE window_1d_start END,
			usage_7d = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN 0 ELSE usage_7d END,
			window_7d_start = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN date_trunc('day', NOW()) ELSE window_7d_start END,
			updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`,
		id)
	return err
}

// GetRateLimitData returns the current rate limit usage and window start times for an API key.
func (r *apiKeyRepository) GetRateLimitData(ctx context.Context, id int64) (result *service.APIKeyRateLimitData, err error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT quota_used, usage_5h, usage_1d, usage_7d, window_5h_start, window_1d_start, window_7d_start
		FROM api_keys
		WHERE id = $1 AND deleted_at IS NULL`,
		id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if !rows.Next() {
		return nil, service.ErrAPIKeyNotFound
	}
	data := &service.APIKeyRateLimitData{}
	if err := rows.Scan(&data.QuotaUsed, &data.Usage5h, &data.Usage1d, &data.Usage7d, &data.Window5hStart, &data.Window1dStart, &data.Window7dStart); err != nil {
		return nil, err
	}
	return data, rows.Err()
}

// apiKeyEntityToService 是纯映射：Key 直接取实体上的值。
//
// ⚠️ 它**不是** drop 安全的：drop 明文列之后 ent 实体上就没有 Key 了（编译期拦下）。
// 现在只剩两处调用，都在 `lookupAPIKeyByHashThenPlaintext` 之后 —— 那里已经把 Key
// 归一成权威明文（密文优先），所以取实体上的值就是对的。
// 任何**新**的读路径请用 apiKeyEntityToServiceResolvingKey / apiKeyEntityToServiceWithKey。
func apiKeyEntityToService(m *dbent.APIKey) *service.APIKey {
	if m == nil {
		return nil
	}
	return apiKeyEntityToServiceWithKey(m, m.Key)
}

// apiKeyEntityToServiceResolvingKey 把 Key 解析成**权威明文（密文优先）**再映射。
//
// 为什么展示路径也要解密：`service.APIKey.Key` 会被掩码/日志/缓存键使用，
// 明文列 drop 之后它只能来自 `key_encrypted`。这里不返回错误——让一行坏数据把
// 整页列表打成 500 没有意义；解不开就置空（掩码显示为空）并打日志，
// 绝不把密文当成明文塞出去。
func (r *apiKeyRepository) apiKeyEntityToServiceResolvingKey(m *dbent.APIKey) *service.APIKey {
	if m == nil {
		return nil
	}
	key, err := r.apiKeyAuthoritativePlaintext(m)
	if err != nil {
		logger.LegacyPrintf("repository.apikey",
			"[APIKeyLookup] cannot resolve plaintext for id=%d: %v; returning empty key so nothing leaks", m.ID, err)
		key = ""
	}
	return apiKeyEntityToServiceWithKey(m, key)
}

func apiKeyEntityToServiceWithKey(m *dbent.APIKey, key string) *service.APIKey {
	if m == nil {
		return nil
	}
	out := &service.APIKey{
		ID:            m.ID,
		UserID:        m.UserID,
		Key:           key,
		Name:          m.Name,
		Status:        m.Status,
		IPWhitelist:   m.IPWhitelist,
		IPBlacklist:   m.IPBlacklist,
		LastUsedAt:    m.LastUsedAt,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
		GroupID:       m.GroupID,
		Quota:         m.Quota,
		QuotaUsed:     m.QuotaUsed,
		ExpiresAt:     m.ExpiresAt,
		RateLimit5h:   m.RateLimit5h,
		RateLimit1d:   m.RateLimit1d,
		RateLimit7d:   m.RateLimit7d,
		Usage5h:       m.Usage5h,
		Usage1d:       m.Usage1d,
		Usage7d:       m.Usage7d,
		Window5hStart: m.Window5hStart,
		Window1dStart: m.Window1dStart,
		Window7dStart: m.Window7dStart,
	}
	if m.Edges.User != nil {
		out.User = userEntityToService(m.Edges.User)
		if allowed := m.Edges.User.Edges.AllowedGroups; len(allowed) > 0 {
			out.User.AllowedGroups = make([]int64, 0, len(allowed))
			for _, g := range allowed {
				if g != nil {
					out.User.AllowedGroups = append(out.User.AllowedGroups, g.ID)
				}
			}
		}
	}
	if m.Edges.Group != nil {
		out.Group = groupEntityToService(m.Edges.Group)
	}
	return out
}

func userEntityToService(u *dbent.User) *service.User {
	if u == nil {
		return nil
	}
	out := &service.User{
		ID:                         u.ID,
		Email:                      u.Email,
		Username:                   u.Username,
		Notes:                      u.Notes,
		PasswordHash:               u.PasswordHash,
		Role:                       u.Role,
		Balance:                    u.Balance,
		FrozenBalance:              u.FrozenBalance,
		Concurrency:                u.Concurrency,
		Status:                     u.Status,
		SignupSource:               u.SignupSource,
		LastLoginAt:                u.LastLoginAt,
		LastActiveAt:               u.LastActiveAt,
		TotpSecretEncrypted:        u.TotpSecretEncrypted,
		TotpEnabled:                u.TotpEnabled,
		TotpEnabledAt:              u.TotpEnabledAt,
		BalanceNotifyEnabled:       u.BalanceNotifyEnabled,
		RestrictPublicGroups:       u.RestrictPublicGroups,
		BalanceNotifyThresholdType: u.BalanceNotifyThresholdType,
		BalanceNotifyThreshold:     u.BalanceNotifyThreshold,
		TotalRecharged:             u.TotalRecharged,
		RPMLimit:                   u.RpmLimit,
		CreatedAt:                  u.CreatedAt,
		UpdatedAt:                  u.UpdatedAt,
		DeletedAt:                  u.DeletedAt,
	}
	// Parse extra emails JSON (supports both old []string and new []NotifyEmailEntry format)
	if u.BalanceNotifyExtraEmails != "" && u.BalanceNotifyExtraEmails != "[]" {
		out.BalanceNotifyExtraEmails = service.ParseNotifyEmails(u.BalanceNotifyExtraEmails)
	}
	return out
}

func groupEntityToService(g *dbent.Group) *service.Group {
	if g == nil {
		return nil
	}
	var modelPricing []service.ChannelModelPricing
	if len(g.ModelPricing) > 0 {
		if err := json.Unmarshal(g.ModelPricing, &modelPricing); err != nil {
			slog.Warn("group model_pricing unmarshal failed; falling back to channel/builtin pricing",
				"group_id", g.ID, "error", err)
			modelPricing = nil
		}
	}
	return &service.Group{
		ID:                              g.ID,
		Name:                            g.Name,
		Description:                     derefString(g.Description),
		I18n:                            g.I18n,
		Platform:                        g.Platform,
		RateMultiplier:                  g.RateMultiplier,
		IsExclusive:                     g.IsExclusive,
		Status:                          g.Status,
		Hydrated:                        true,
		DuplicateOperationID:            derefString(g.DuplicateOperationID),
		SubscriptionType:                g.SubscriptionType,
		DailyLimitUSD:                   g.DailyLimitUsd,
		WeeklyLimitUSD:                  g.WeeklyLimitUsd,
		MonthlyLimitUSD:                 g.MonthlyLimitUsd,
		AllowImageGeneration:            g.AllowImageGeneration,
		AllowBatchImageGeneration:       g.AllowBatchImageGeneration,
		ImageCountBilling:               g.ImageCountBilling,
		ImageRateIndependent:            g.ImageRateIndependent,
		ImageRateMultiplier:             g.ImageRateMultiplier,
		ImagePrice1K:                    g.ImagePrice1k,
		ImagePrice2K:                    g.ImagePrice2k,
		ImagePrice4K:                    g.ImagePrice4k,
		BatchImageDiscountMultiplier:    g.BatchImageDiscountMultiplier,
		BatchImageHoldMultiplier:        g.BatchImageHoldMultiplier,
		VideoRateIndependent:            g.VideoRateIndependent,
		VideoRateMultiplier:             g.VideoRateMultiplier,
		VideoPrice480P:                  g.VideoPrice480p,
		VideoPrice720P:                  g.VideoPrice720p,
		VideoPrice1080P:                 g.VideoPrice1080p,
		VideoModelPrices:                service.NormalizeVideoModelPrices(g.VideoModelPrices),
		WebSearchPricePerCall:           g.WebSearchPricePerCall,
		RerankPricePerRequest:           g.RerankPricePerRequest,
		SearchPricePer1k:                g.SearchPricePer1k,
		AudioRealtimePricePerMin:        g.AudioRealtimePricePerMin,
		AudioTTSPricePerMillionChars:    g.AudioTtsPricePerMillionChars,
		AudioSTTPricePerHour:            g.AudioSttPricePerHour,
		LongContextPricingEnabled:       g.LongContextPricingEnabled,
		ModelPricing:                    modelPricing,
		DefaultValidityDays:             g.DefaultValidityDays,
		ClaudeCodeOnly:                  g.ClaudeCodeOnly,
		FallbackGroupID:                 g.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: g.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    g.ModelRouting,
		ModelRoutingEnabled:             g.ModelRoutingEnabled,
		MCPXMLInject:                    g.McpXMLInject,
		SupportedModelScopes:            g.SupportedModelScopes,
		SortOrder:                       g.SortOrder,
		AllowMessagesDispatch:           g.AllowMessagesDispatch,
		AllowLive:                       g.AllowLive,
		ForceOpenAIFast:                 g.ForceOpenaiFast,
		FreeOpenAIFast:                  g.FreeOpenaiFast,
		RequireOAuthOnly:                g.RequireOauthOnly,
		RequirePrivacySet:               g.RequirePrivacySet,
		DefaultMappedModel:              g.DefaultMappedModel,
		MessagesDispatchModelConfig:     g.MessagesDispatchModelConfig,
		ModelAllowlist:                  service.GroupModelAllowlistFromDomain(g.ModelAllowlist),
		CodexModelsManifestConfig:       g.CodexModelsManifestConfig,
		RPMLimit:                        g.RpmLimit,
		MaxReasoningEffort:              g.MaxReasoningEffort,
		MaxReasoningEffortOverLimit:     g.MaxReasoningEffortOverLimit,
		ReasoningEffortMappings:         g.ReasoningEffortMappings,
		PeakRateEnabled:                 g.PeakRateEnabled,
		PeakStart:                       g.PeakStart,
		PeakEnd:                         g.PeakEnd,
		PeakRateMultiplier:              g.PeakRateMultiplier,
		ProfitControlEnabled:            g.ProfitControlEnabled,
		ProfitMinMargin:                 g.ProfitMinMargin,
		ProfitSafetyBuffer:              g.ProfitSafetyBuffer,
		CreatedAt:                       g.CreatedAt,
		UpdatedAt:                       g.UpdatedAt,
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
