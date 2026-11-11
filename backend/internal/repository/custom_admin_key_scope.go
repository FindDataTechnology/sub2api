package repository

import (
	"context"
	"fmt"
	"slices"
	"strings"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) ScopedAdminList(ctx context.Context, p pagination.PaginationParams, platform, kind, status, search string) ([]service.Account, int64, error) {
	principal := adminaccess.FromContext(ctx)
	if principal == nil {
		return nil, 0, fmt.Errorf("缺少受限身份")
	}
	alternatives := scopedAdminAccountPredicates(principal.Policy, "accounts.read")
	if len(alternatives) == 0 {
		return []service.Account{}, 0, nil
	}
	q := r.accountListFilteredQuery(platform, kind, status, search, 0, "").Where(dbaccount.Or(alternatives...))
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	// model_mapping 原先用 SQL 聚合 `credentials->'model_mapping'` 直接投影。
	// drop 明文列之后那条聚合会运行期报错，所以改成**解密后取值**：
	// 凭据只在 Go 侧出现，返回体里也只有 model_mapping，其余字段不外带。
	models, err := q.Offset(p.Offset()).Limit(p.Limit()).Order(dbaccount.ByID()).
		Select(dbaccount.FieldID, dbaccount.FieldName, dbaccount.FieldNotes,
			dbaccount.FieldPlatform, dbaccount.FieldType, dbaccount.FieldStatus,
			dbaccount.FieldSchedulable, dbaccount.FieldConcurrency, dbaccount.FieldPriority,
			dbaccount.FieldCredentials, dbaccount.FieldCredentialsEncrypted).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	accounts := make([]service.Account, 0, len(models))
	for _, m := range models {
		groups, e := r.loadAccountGroupIDs(ctx, m.ID)
		if e != nil {
			return nil, 0, e
		}
		mapping := map[string]string{}
		creds, credErr := r.credentialsFromEntity(m)
		if credErr != nil {
			// 这一列只用于展示：解不开就让该行映射为空，别把整页列表打成 500。
			// 但必须留痕，否则"管理端看不到路由配置"会变成查不出原因的现象。
			logger.LegacyPrintf("repository.scopedadmin",
				"[ScopedAdminList] cannot resolve credentials for account=%d: %v; returning empty model_mapping", m.ID, credErr)
		} else if raw, ok := creds["model_mapping"].(map[string]any); ok {
			for source, target := range raw {
				if value, ok := target.(string); ok {
					mapping[source] = value
				}
			}
		}
		var outCredentials map[string]any
		if len(mapping) > 0 {
			outCredentials = map[string]any{"model_mapping": mapping}
		}
		accounts = append(accounts, service.Account{ID: m.ID, Name: m.Name, Notes: m.Notes, Platform: m.Platform, Type: m.Type, Status: m.Status, Schedulable: m.Schedulable, Concurrency: m.Concurrency, Priority: m.Priority, GroupIDs: groups, Credentials: outCredentials})
	}
	return accounts, int64(total), nil
}

// ScopedAdminCommitted 提交后按实际操作删除或更新缓存，避免对已删除账户读取快照。
func (r *accountRepository) ScopedAdminCommitted(ctx context.Context, id int64, deleted bool) {
	if deleted {
		r.deleteSchedulerAccountSnapshot(ctx, id)
		return
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
}

// scopedAdminBindGroups 只增删变化的关联，保留现有关联的优先级和创建时间。
// 调用者已在受限事务内验权并合并不可操作的分组，不得回退到全量重建。
func (r *accountRepository) scopedAdminBindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	if dbent.TxFromContext(ctx) == nil {
		return fmt.Errorf("受限分组写入需要数据库事务")
	}
	existing, err := r.loadAccountGroupIDs(ctx, accountID)
	if err != nil {
		return err
	}
	added, removed := []int64{}, []int64{}
	for _, id := range groupIDs {
		if !slices.Contains(existing, id) && !slices.Contains(added, id) {
			added = append(added, id)
		}
	}
	for _, id := range existing {
		if !slices.Contains(groupIDs, id) {
			removed = append(removed, id)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	if err := lockLiveGroups(ctx, r.client, added); err != nil {
		return err
	}
	if len(removed) > 0 {
		if _, err := r.client.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(accountID), dbaccountgroup.GroupIDIn(removed...)).Exec(ctx); err != nil {
			return err
		}
	}
	for _, id := range added {
		if _, err := r.client.AccountGroup.Create().SetAccountID(accountID).SetGroupID(id).SetPriority(slices.Index(groupIDs, id) + 1).Save(ctx); err != nil {
			return err
		}
	}
	return enqueueSchedulerOutbox(ctx, r.sql, service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, buildSchedulerGroupPayload(mergeGroupIDs(existing, groupIDs)))
}

// scopedAdminAccountPredicates 保证读取和导入候选查询使用相同的范围语义。
func scopedAdminAccountPredicates(policy adminaccess.Policy, permission string) []dbpredicate.Account {
	alternatives := []dbpredicate.Account{}
	for _, rule := range policy.RulesFor(permission) {
		if len(rule.AccountTypes) == 0 {
			continue
		}
		terms := []dbpredicate.Account{dbaccount.TypeIn(rule.AccountTypes...)}
		if len(rule.Platforms) > 0 {
			terms = append(terms, dbaccount.PlatformIn(rule.Platforms...))
		}
		if rule.AllGroups && !rule.IncludeUngrouped {
			terms = append(terms, dbaccount.HasAccountGroups())
		}
		if !rule.AllGroups {
			groups := []dbpredicate.Account{}
			if len(rule.GroupIDs) > 0 {
				groups = append(groups, dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDIn(rule.GroupIDs...)))
			}
			if rule.IncludeUngrouped {
				groups = append(groups, dbaccount.Not(dbaccount.HasAccountGroups()))
			}
			if len(groups) == 0 {
				continue
			}
			terms = append(terms, dbaccount.Or(groups...))
		}
		alternatives = append(alternatives, dbaccount.And(terms...))
	}
	return alternatives
}

// ScopedAdminCodexCandidates 按导入身份逐级匹配，最多返回一个候选的身份字段。
// 不向应用加载其他账户的凭证；真正合并凭证仍在写事务锁内进行。
//
// 匹配在 **Go 侧**完成（design.md A4 选项 C）：原实现把身份谓词拼成 SQL
// （`"accounts"."credentials"->'key'`），drop 明文列之后整族失效。现在只把
// **权限范围**（scope）+ platform/type 留在 SQL 里，身份匹配改在解密后的凭据上做。
// 顺序语义不变：先命中者胜，同一谓词内取 id 最大者（与原来的 ORDER BY id DESC LIMIT 1 等价）。
//
// 等价性由 TestScopedCodexIdentityPredicates_SQLAndGoAgree 守着（每条谓词 × 每行）；
// 反向实验证明 trim / 大小写 / 数字归一化 / 顺序任一处漂移都会让它变红。
func (r *accountRepository) ScopedAdminCodexCandidates(ctx context.Context, identity service.ScopedCodexIdentity) ([]service.Account, error) {
	principal := adminaccess.FromContext(ctx)
	if principal == nil {
		return nil, fmt.Errorf("缺少受限身份")
	}
	scope := scopedAdminAccountPredicates(principal.Policy, "accounts.credentials")
	if len(scope) == 0 {
		return []service.Account{}, nil
	}
	if len(scopedCodexIdentityMatchers(identity)) == 0 {
		// 身份字段一个都没给：原实现此时不生成任何谓词，直接空结果。
		return []service.Account{}, nil
	}

	// 一次取候选再在 Go 里筛。代价是这条路径要解密全部候选行 —— 它是人工触发的
	// 低频导入，不是请求热路径（A4 选项 C 的取舍）。
	// **不加 LIMIT**：加了就把"按 id 降序第一个命中"变成"前 N 行里第一个命中"，语义变了。
	rows, err := r.accountListFilteredQuery(service.PlatformOpenAI, service.AccountTypeOAuth, "", "", 0, "").
		Where(dbaccount.Or(scope...)).
		Order(dbaccount.ByID(entsql.OrderDesc())).
		Select(dbaccount.FieldID, dbaccount.FieldCredentials, dbaccount.FieldCredentialsEncrypted).
		All(ctx)
	if err != nil {
		return nil, err
	}

	resolved := make(map[int64]map[string]any, len(rows))
	picked := scopedCodexIdentityPick(rows, func(m *dbent.Account) map[string]any {
		creds, err := r.credentialsFromEntity(m)
		if err != nil {
			// 解密失败就跳过这一行，但必须留痕：静默跳过会让"导入时找不到已有账号"
			// 变成一个查不出原因的现象。注意返回 nil 不会误命中任何谓词
			// ——每条谓词都在与一个非空值比较。
			logger.LegacyPrintf("repository.scopedadmin",
				"[ScopedCodexIdentity] cannot resolve credentials for account=%d: %v; skipping candidate", m.ID, err)
			return nil
		}
		resolved[m.ID] = creds
		return creds
	}, identity)
	if picked == nil {
		return []service.Account{}, nil
	}

	// 只回传这四个身份字段（与原 SQL 的 jsonb_build_object 投影一致），
	// 不把整份凭据交给调用方。
	creds := resolved[picked.ID]
	return []service.Account{{
		ID: picked.ID,
		Credentials: map[string]any{
			"chatgpt_account_id": creds["chatgpt_account_id"],
			"chatgpt_user_id":    creds["chatgpt_user_id"],
			"email":              creds["email"],
			"access_token":       creds["access_token"],
		},
	}}, nil
}

// 与导入器的 account_user / user / email / access / account 优先级保持一致。
// account 回退必须排除明确属于其他用户的账户；仅 access token 和 Agent Identity 各有独立匹配规则。
func scopedCodexIdentityPredicates(identity service.ScopedCodexIdentity) []dbpredicate.Account {
	accountID, userID := strings.TrimSpace(identity.AccountID), strings.TrimSpace(identity.UserID)
	email, token := strings.ToLower(strings.TrimSpace(identity.Email)), strings.TrimSpace(identity.AccessToken)
	// 与 Go strings.TrimSpace 的 Unicode 空白集合一致，兼容旧凭证中的首尾空白。
	const whitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	field := func(key string) string {
		// 字段名均为下方固定常量；非法 JSON 类型按导入器规则视为空值。
		raw := `"accounts"."credentials"->'` + key + `'`
		value := `"accounts"."credentials"->>'` + key + `'`
		// 旧 JSON 数值经 Go 解码为 float64：保留同样的精度，展开指数并去掉尾随小数零。
		number := "trim_scale(((" + value + ")::double precision)::text::numeric)::text"
		return "btrim(CASE jsonb_typeof(" + raw + ") WHEN 'string' THEN " + value + " WHEN 'number' THEN " + number + " ELSE '' END, '" + whitespace + "')"
	}
	a, u, e, access := field("chatgpt_account_id"), field("chatgpt_user_id"), field("email"), field("access_token")
	match := func(expr string, args ...any) dbpredicate.Account {
		parts := strings.Split(expr, "?")
		return func(s *entsql.Selector) {
			s.Where(entsql.P(func(b *entsql.Builder) {
				for i, part := range parts {
					b.WriteString(part)
					if i < len(args) {
						b.Arg(args[i])
					}
				}
			}))
		}
	}
	predicates := []dbpredicate.Account{}
	if !identity.AgentIdentity && !identity.AccessTokenOnly {
		switch {
		case accountID != "" && userID != "":
			predicates = append(predicates, match(a+" = ? AND "+u+" = ?", accountID, userID))
		case userID != "":
			predicates = append(predicates, match(a+" = '' AND "+u+" = ?", userID))
		case accountID == "" && email != "":
			predicates = append(predicates, match(a+" = '' AND "+u+" = '' AND lower("+e+") = ?", email))
		}
	}
	if !identity.AgentIdentity && token != "" {
		predicates = append(predicates, match(access+" = ?", token))
	}
	if accountID != "" && (identity.AgentIdentity || !identity.AccessTokenOnly) {
		if userID == "" {
			predicates = append(predicates, match(a+" = ?", accountID))
		} else {
			predicates = append(predicates, match(a+" = ? AND ("+u+" = '' OR "+u+" = ?)", accountID, userID))
		}
	}
	return predicates
}
