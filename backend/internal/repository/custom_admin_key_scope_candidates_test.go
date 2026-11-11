package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// ScopedAdminCodexCandidates 的**新实现**（design.md A4 选项 C）在真库上的验收。
//
// 等价性另有 TestScopedCodexIdentityPredicates_SQLAndGoAgree 逐条守着；这条补的是
// 等价性测不到的部分：**SQL 之外的行为**——权限 scope 是否仍然生效、
// 返回体是否只带四个身份字段、以及明文列清空后是否仍能匹配（drop 后就是这个状态）。
func TestScopedAdminCodexCandidates_MatchesWithoutPlaintextColumn(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx))

	// 两行：target 会被身份命中；other 属于"另一个用户"，用来确认选中的是 target。
	seed := func(name string, creds map[string]any) int64 {
		cols, err := CredentialCryptoColumnsFor(creds, cryptor)
		require.NoError(t, err)
		create := client.Account.Create().
			SetName(name).
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeOAuth).
			SetCredentials(creds).
			SetCredentialsMeta(cols.Meta).
			SetCredentialsHash(*cols.Hash)
		if cols.Encrypted != nil {
			create = create.SetCredentialsEncrypted(*cols.Encrypted)
		}
		if cols.APIKeyHash != nil {
			create = create.SetCredentialsAPIKeyHash(*cols.APIKeyHash)
		}
		m, err := create.Save(ctx)
		require.NoError(t, err)
		return m.ID
	}
	targetID := seed("codex-target", map[string]any{
		"chatgpt_account_id": "acc-target", "chatgpt_user_id": "user-target",
		"email": "Target@Example.com", "access_token": "tok-target",
		"refresh_token": "refresh-must-not-leak",
	})
	seed("codex-other", map[string]any{
		"chatgpt_account_id": "acc-other", "chatgpt_user_id": "user-other",
		"email": "other@example.com", "access_token": "tok-other",
	})

	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, cryptor)
	// 权限：只给账号类权限。AllGroups 必须配 IncludeUngrouped ——
	// `AllGroups && !IncludeUngrouped` 会追加 `HasAccountGroups()`，
	// 而这批夹具没有分组，scope 就会一条都匹配不上（我第一版就这么空跑了）。
	// 这里要验的是身份匹配路径，不是分组范围，所以放开未分组。
	principalCtx := adminaccess.WithPrincipal(ctx, &adminaccess.Principal{
		KeyID: "fixture",
		Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
			Permissions:      []string{"accounts.credentials"},
			AllGroups:        true,
			IncludeUngrouped: true,
			AccountTypes:     []string{"oauth"},
			Platforms:        []string{service.PlatformOpenAI},
		}}},
	})

	identity := service.ScopedCodexIdentity{AccountID: "acc-target", UserID: "user-target"}

	found, err := repo.ScopedAdminCodexCandidates(principalCtx, identity)
	require.NoError(t, err)
	require.Len(t, found, 1, "应命中一个候选")
	require.Equal(t, targetID, found[0].ID)
	require.Equal(t, "acc-target", found[0].Credentials["chatgpt_account_id"])
	require.Equal(t, "user-target", found[0].Credentials["chatgpt_user_id"])
	require.Equal(t, "Target@Example.com", found[0].Credentials["email"])
	require.Equal(t, "tok-target", found[0].Credentials["access_token"])
	require.Len(t, found[0].Credentials, 4,
		"只回传四个身份字段：refresh_token 之类的其余凭据不能跟着出来")
	require.NotContains(t, found[0].Credentials, "refresh_token")

	// ── drop 模拟：明文列清空，只剩密文 ──
	_, err = db.ExecContext(ctx, `UPDATE accounts SET credentials = '{}'::jsonb`)
	require.NoError(t, err)

	found, err = repo.ScopedAdminCodexCandidates(principalCtx, identity)
	require.NoError(t, err, "明文列没了也必须能匹配 —— drop 之后就是这个状态")
	require.Len(t, found, 1)
	require.Equal(t, targetID, found[0].ID, "匹配必须来自密文解密，而不是明文列")
	require.Equal(t, "tok-target", found[0].Credentials["access_token"])

	// 不匹配的身份 → 空（且不是报错）
	found, err = repo.ScopedAdminCodexCandidates(principalCtx, service.ScopedCodexIdentity{AccountID: "acc-none"})
	require.NoError(t, err)
	require.Empty(t, found)

	// 空身份 → 空（原实现在这种情况下一条谓词都不生成）
	found, err = repo.ScopedAdminCodexCandidates(principalCtx, service.ScopedCodexIdentity{})
	require.NoError(t, err)
	require.Empty(t, found)

	// 没有 principal → 报错（授权边界不能被这次改动绕过）
	_, err = repo.ScopedAdminCodexCandidates(ctx, identity)
	require.Error(t, err, "缺少受限身份时必须拒绝")
}

// ScopedAdminList 的 model_mapping 原先用 SQL 聚合 `credentials->'model_mapping'` 取，
// drop 明文列之后会运行期报错。改成解密后取值，这条测试把两件事钉在一起：
//
//	① 正常时能拿到映射（正例；没有它，下面的断言全"通过"也无意义）
//	② **明文列清空后仍能拿到**（drop 后就是这个状态）
//	③ 返回体里只有 model_mapping，别的凭据不外带
func TestScopedAdminList_ModelMappingWithoutPlaintextColumn(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx))

	creds := map[string]any{
		"api_key":       "sk-must-not-leak",
		"model_mapping": map[string]any{"claude-3": "gpt-4o", "dropped": 42},
	}
	cols, err := CredentialCryptoColumnsFor(creds, cryptor)
	require.NoError(t, err)
	created, err := client.Account.Create().
		SetName("list-mapping").
		SetPlatform(service.PlatformOpenAI).
		SetType(service.AccountTypeAPIKey).
		SetCredentials(creds).
		SetCredentialsMeta(cols.Meta).
		SetCredentialsHash(*cols.Hash).
		SetCredentialsEncrypted(*cols.Encrypted).
		Save(ctx)
	require.NoError(t, err)

	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, cryptor)
	principalCtx := adminaccess.WithPrincipal(ctx, &adminaccess.Principal{
		KeyID: "fixture",
		Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
			Permissions:      []string{"accounts.read"},
			AllGroups:        true,
			IncludeUngrouped: true,
			AccountTypes:     []string{service.AccountTypeAPIKey},
			Platforms:        []string{service.PlatformOpenAI},
		}}},
	})

	list, total, err := repo.ScopedAdminList(principalCtx, pagination.PaginationParams{Page: 1, PageSize: 10}, "", "", "", "")
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, list, 1)
	require.Equal(t, created.ID, list[0].ID)
	require.Equal(t, map[string]any{"model_mapping": map[string]string{"claude-3": "gpt-4o"}},
		list[0].Credentials, "只保留字符串值，且只回传 model_mapping（api_key 不外带）")

	// ── drop 模拟：明文列清空 ──
	_, err = db.ExecContext(ctx, `UPDATE accounts SET credentials = '{}'::jsonb`)
	require.NoError(t, err)

	list, total, err = repo.ScopedAdminList(principalCtx, pagination.PaginationParams{Page: 1, PageSize: 10}, "", "", "", "")
	require.NoError(t, err, "明文列没了也必须能列出 —— drop 之后就是这个状态")
	require.Equal(t, int64(1), total)
	require.Equal(t, map[string]any{"model_mapping": map[string]string{"claude-3": "gpt-4o"}},
		list[0].Credentials, "映射必须来自密文解密")
}
