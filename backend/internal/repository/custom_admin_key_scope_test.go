//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScopedAdminAccountListScopesCountAndPageInSQL(t *testing.T) {
	var countScope string
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		if !strings.Contains(actual, `"type"`) || !strings.Contains(actual, `"account_groups"`) || !strings.Contains(actual, " OR ") {
			return fmt.Errorf("查询缺少完整规则: %s", actual)
		}
		where := actual[strings.Index(actual, " WHERE "):]
		if i := strings.Index(where, " ORDER BY "); i >= 0 {
			where = where[:i]
		}
		if i := strings.Index(where, " LIMIT "); i >= 0 {
			where = where[:i]
		}
		if expected == "count" {
			countScope = where
		} else {
			if where != countScope {
				return fmt.Errorf("分页和统计范围不同")
			}
			// 契约（2026-11-11 更新）：禁止读取**明文** `accounts.credentials` 列
			// —— drop 之后这一列不存在；也禁止 `extra`。
			// 允许 `credentials_encrypted`：它是 drop 之后 model_mapping 的唯一来源，
			// 且不是明文。用带右引号的完整列名做判据，所以 `"credentials_encrypted"`
			// 不会误命中（旧版用子串 `"credentials"`，把这个面允许读的东西也一起禁了）。
			if strings.Contains(actual, `"accounts"."credentials"`) || strings.Contains(actual, `"extra"`) {
				return fmt.Errorf("读取了敏感字段（明文 credentials 或 extra）")
			}
		}
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("page").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	principal := &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{
		{Permissions: []string{"accounts.read"}, GroupIDs: []int64{1}, AccountTypes: []string{"oauth"}},
		{Permissions: []string{"accounts.read"}, GroupIDs: []int64{2}, AccountTypes: []string{"apikey"}},
	}}}
	_, total, err := repo.ScopedAdminList(adminaccess.WithPrincipal(context.Background(), principal), pagination.PaginationParams{Page: 1, PageSize: 20}, "", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatal(total)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// setAccountCredentials 用生产派生逻辑把凭据写进"只有派生列"的 accounts 表。
//
// 阶段 5.2（迁移 251）之后明文列已 drop，测试夹具必须走同一条派生路径：
// 夹具里再写 `credentials = '...'::jsonb` 既跑不过（列不存在），
// 也会让夹具与生产形态不一致 —— 那等于用旧形态验新代码。
func setAccountCredentials(t *testing.T, exec interface {
	Exec(query string, args ...any) (sql.Result, error)
}, id int64, creds map[string]any, cryptor service.SecretEncryptor) {
	t.Helper()
	cols, err := CredentialCryptoColumnsFor(creds, cryptor)
	require.NoError(t, err)
	var encrypted, hash, apiKeyHash any
	if cols.Encrypted != nil {
		encrypted = *cols.Encrypted
	}
	if cols.Hash != nil {
		hash = *cols.Hash
	}
	if cols.APIKeyHash != nil {
		apiKeyHash = *cols.APIKeyHash
	}
	_, err = exec.Exec(`
		UPDATE accounts
		SET credentials_meta = $2::jsonb,
		    credentials_encrypted = $3,
		    credentials_hash = $4,
		    credentials_api_key_hash = $5
		WHERE id = $1`, id, mustJSON(t, cols.Meta), encrypted, hash, apiKeyHash)
	require.NoError(t, err)
}

func TestScopedAdminAccountListPostgresRulesAndPagination(t *testing.T) {
	dsn := os.Getenv("SCOPED_ADMIN_TEST_DSN")
	if dsn == "" {
		t.Skip("需要独立 SCOPED_ADMIN_TEST_DSN")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("scoped_list_test_%d", time.Now().UnixNano())
	_, err = base.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn+" search_path="+schema)
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() {
		_ = client.Close()
		_, _ = base.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = base.Close()
	})
	// 最小数据集刻意交叉分组、类型和平台，防止不同规则被拆开组合。
	_, err = db.Exec(`CREATE TABLE accounts (
		id BIGINT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', notes TEXT, platform TEXT NOT NULL,
		type TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', schedulable BOOLEAN NOT NULL DEFAULT true,
		concurrency INT NOT NULL DEFAULT 1, priority INT NOT NULL DEFAULT 1,
		credentials_meta JSONB NOT NULL DEFAULT '{}', credentials_encrypted TEXT,
		credentials_hash VARCHAR(64), credentials_api_key_hash VARCHAR(64),
		expires_at TIMESTAMPTZ, rate_limit_reset_at TIMESTAMPTZ,
		overload_until TIMESTAMPTZ, temp_unschedulable_until TIMESTAMPTZ,
		deleted_at TIMESTAMPTZ
	);
	CREATE TABLE account_groups(account_id BIGINT, group_id BIGINT, priority INT DEFAULT 50, created_at TIMESTAMPTZ DEFAULT NOW());
	INSERT INTO accounts(id,platform,type) VALUES
	(1,'openai','oauth'), (2,'openai','apikey'), (3,'openai','oauth'),
	(4,'openai','apikey'), (5,'anthropic','oauth'), (6,'openai','oauth');
	INSERT INTO account_groups(account_id,group_id) VALUES (1,1),(1,2),(2,1),(3,2),(4,2),(5,1);`)
	require.NoError(t, err)
	cryptor := credentialTestEncryptor(t)
	setAccountCredentials(t, db, 1, map[string]any{
		"model_mapping": map[string]any{"alias": "target", "invalid": map[string]any{"token": "NESTED_SECRET"}},
		"access_token":  "TOKEN_SECRET",
		"refresh_token": "REFRESH_SECRET",
		"api_key":       "KEY_SECRET",
		"base_url":      "URL_SECRET",
	}, cryptor)
	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, cryptor)
	principal := &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{
		{Permissions: []string{"accounts.read"}, GroupIDs: []int64{1}, AccountTypes: []string{"oauth"}, Platforms: []string{"openai"}},
		{Permissions: []string{"accounts.read"}, GroupIDs: []int64{2}, AccountTypes: []string{"apikey"}, Platforms: []string{"openai"}},
	}}}
	ctx := adminaccess.WithPrincipal(context.Background(), principal)
	for page, wantID := range []int64{1, 4} {
		rows, total, listErr := repo.ScopedAdminList(ctx, pagination.PaginationParams{Page: page + 1, PageSize: 1}, "", "", "", "", 0)
		require.NoError(t, listErr)
		require.EqualValues(t, 2, total)
		require.Len(t, rows, 1)
		require.Equal(t, wantID, rows[0].ID)
		if wantID == 1 {
			require.Equal(t, map[string]any{"model_mapping": map[string]string{"alias": "target"}}, rows[0].Credentials)
		} else {
			require.Empty(t, rows[0].Credentials)
		}
	}
	// 缺失、清空及错误形状都应保留可读取列表，不回传嵌套对象或凭证。
	for _, credentials := range []string{`{}`, `{"model_mapping":null}`, `{"model_mapping":{}}`, `{"model_mapping":[]}`, `{"model_mapping":"invalid"}`, `{"model_mapping":{"bad":{"token":"SECRET"},"number":1},"access_token":"SECRET"}`} {
		var creds map[string]any
		require.NoError(t, json.Unmarshal([]byte(credentials), &creds))
		setAccountCredentials(t, db, 4, creds, cryptor)
		rows, total, listErr := repo.ScopedAdminList(ctx, pagination.PaginationParams{Page: 2, PageSize: 1}, "", "", "", "", 0)
		require.NoError(t, listErr)
		require.EqualValues(t, 2, total)
		require.Len(t, rows, 1)
		require.EqualValues(t, 4, rows[0].ID)
		require.Empty(t, rows[0].Credentials)
	}
	principal.Policy.Rules = []adminaccess.Rule{{Permissions: []string{"accounts.read"}, AllGroups: true, AccountTypes: []string{"oauth"}}}
	rows, total, err := repo.ScopedAdminList(ctx, pagination.DefaultPagination(), "", "", "", "", 0)
	require.NoError(t, err)
	require.EqualValues(t, 3, total, "全部分组不包含无分组账户")
	require.Len(t, rows, 3)
	principal.Policy.Rules[0].IncludeUngrouped = true
	_, total, err = repo.ScopedAdminList(ctx, pagination.DefaultPagination(), "", "", "", "", 0)
	require.NoError(t, err)
	require.EqualValues(t, 4, total)
}

func TestScopedAdminCodexCandidatesQueryIsBounded(t *testing.T) {
	// 契约（2026-11-11 更新，随 drop 明文列的必要性而来）：
	// 旧版要求 SQL 的 WHERE 里出现 `credentials`（=按身份子键筛选）且带 ` LIMIT `。
	// drop 明文列之后"身份只存在于明文 JSON"这条前提不成立，于是身份匹配搬到 Go
	// （design D8 / A4 选项 C，另有 TestScopedCodexIdentityPredicates_SQLAndGoAgree
	// 做 20 组身份 × 10 行 × 每条谓词序的等价性）。
	// 新契约是**更严**的两条：
	//   ① 不得读明文列（`"accounts"."credentials"` 整词，密文列允许）；
	//   ② WHERE 必须带权限范围谓词（`account_groups`/platform/type），不能裸扫全表。
	// 注：这里**不要求 LIMIT**。加 LIMIT 会把"按 id 降序第一个命中"变成
	// "前 N 行里第一个命中"，从而把更早的合法账号判成不存在、导入出重复账号 ——
	// 语义代价已在实现处写明，成本则用候选数 WARN 日志观测（>500 行打日志）。
	matcher := sqlmock.QueryMatcherFunc(func(_, actual string) error {
		where := actual[strings.Index(actual, " WHERE "):]
		if strings.Contains(actual, `"accounts"."credentials"`) {
			return fmt.Errorf("导入候选读取了明文凭据列（drop 之后会直接报错）: %s", actual)
		}
		if !strings.Contains(actual, `"accounts"."credentials_encrypted"`) {
			return fmt.Errorf("导入候选必须只读密文列（明文列在 drop 后不存在）: %s", actual)
		}
		if !strings.Contains(where, `"platform"`) || !strings.Contains(where, `"type"`) {
			return fmt.Errorf("导入候选缺少平台/类型谓词（会退化成全表扫描）: %s", actual)
		}
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	defer db.Close()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	repo := newAccountRepositoryWithSQL(client, db, nil)
	ctx := adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"accounts.credentials"}, AccountTypes: []string{"oauth"}, AllGroups: true, IncludeUngrouped: true}}}})
	mock.ExpectQuery("候选身份").WillReturnRows(sqlmock.NewRows([]string{"id", "credentials"}))
	_, err = repo.ScopedAdminCodexCandidates(ctx, service.ScopedCodexIdentity{AccessToken: "inside", AccessTokenOnly: true})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestScopedAdminCodexCandidatesUseCredentialWriteScope(t *testing.T) {
	db := scopedAdminKeyTestDB(t)
	cryptor := credentialTestEncryptor(t)
	_, err := db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY,platform TEXT,type TEXT,
 credentials_meta JSONB NOT NULL DEFAULT '{}',credentials_encrypted TEXT,
 credentials_hash VARCHAR(64),credentials_api_key_hash VARCHAR(64),deleted_at TIMESTAMPTZ);
 CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT);
 INSERT INTO accounts(id,platform,type) VALUES
 (1,'openai','oauth'),(2,'openai','oauth'),(3,'openai','apikey'),(4,'anthropic','oauth'),
 (5,'openai','oauth'),(6,'openai','oauth');
 INSERT INTO account_groups VALUES(1,1),(1,2),(2,2),(3,1),(4,1),(6,1);
 UPDATE accounts SET deleted_at=NOW() WHERE id=6;`)
	require.NoError(t, err)
	// 原实现是一条 `UPDATE accounts SET credentials='{"access_token":"inside"}'`：
	// 全表统一成 inside。5.2 之后只能逐行写派生列（列已 drop）。
	for id := int64(1); id <= 6; id++ {
		setAccountCredentials(t, db, id, map[string]any{"access_token": "inside"}, cryptor)
	}
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, cryptor)
	principal := &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"accounts.credentials"}, AccountTypes: []string{"oauth"}, GroupIDs: []int64{1}}, {Permissions: []string{"accounts.read"}, AccountTypes: []string{"oauth"}, GroupIDs: []int64{2}}}}}
	ctx := adminaccess.WithPrincipal(context.Background(), principal)
	rows, err := repo.ScopedAdminCodexCandidates(ctx, service.ScopedCodexIdentity{AccessToken: "inside", AccessTokenOnly: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 1, rows[0].ID)
	require.Equal(t, "inside", rows[0].Credentials["access_token"])
	principal.Policy.Rules[0].IncludeUngrouped = true
	rows, err = repo.ScopedAdminCodexCandidates(ctx, service.ScopedCodexIdentity{AccessToken: "inside", AccessTokenOnly: true})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 5, rows[0].ID)
	principal.Policy.Rules[0].Permissions = []string{"accounts.read"}
	rows, err = repo.ScopedAdminCodexCandidates(ctx, service.ScopedCodexIdentity{AccessToken: "inside", AccessTokenOnly: true})
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestScopedAdminCodexCandidatesMatchIdentityWithoutLoadingUnrelatedCredentials(t *testing.T) {
	db := scopedAdminKeyTestDB(t)
	cryptor := credentialTestEncryptor(t)
	_, err := db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY,platform TEXT,type TEXT,
 credentials_meta JSONB NOT NULL DEFAULT '{}',credentials_encrypted TEXT,
 credentials_hash VARCHAR(64),credentials_api_key_hash VARCHAR(64),deleted_at TIMESTAMPTZ);
 CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT);`)
	require.NoError(t, err)
	// 夹具逐行写派生列（5.2 之后明文列已 drop）。凭据内容与改动前逐字一致，
	// 只是落盘改走同一条加解密路径 —— 于是这里验的正是"解密后再做身份匹配"。
	for _, row := range []struct {
		id    int64
		creds map[string]any
	}{
		{1, map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "member", "access_token": "old", "refresh_token": "must-not-load", "large_extra": "must-not-load"}},
		{2, map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "other", "access_token": "shared"}},
		{3, map[string]any{"chatgpt_account_id": "workspace", "access_token": "legacy"}},
		{4, map[string]any{"chatgpt_user_id": "member", "access_token": "user-only"}},
		{5, map[string]any{"email": " Test@Example.COM ", "access_token": "email-only"}},
		{6, map[string]any{"chatgpt_account_id": "elsewhere", "chatgpt_user_id": "member", "email": "test@example.com", "access_token": "different-workspace"}},
		{7, map[string]any{"chatgpt_account_id": 123.00, "chatgpt_user_id": int64(9007199254740993), "access_token": "numeric-legacy"}},
	} {
		_, err = db.Exec(`INSERT INTO accounts(id,platform,type) VALUES ($1,'openai','oauth')`, row.id)
		require.NoError(t, err)
		setAccountCredentials(t, db, row.id, row.creds, cryptor)
	}
	// 1001 行"无关凭据"（原实现用一条 INSERT ... generate_series 完成）。
	// 8KB 的 refresh_token 刻意保留：它是"别把无关大对象读进来"那条断言的载体。
	for id := int64(100); id <= 1100; id++ {
		_, err = db.Exec(`INSERT INTO accounts(id,platform,type) VALUES ($1,'openai','oauth')`, id)
		require.NoError(t, err)
		setAccountCredentials(t, db, id, map[string]any{
			"access_token":  fmt.Sprintf("unrelated-%d", id),
			"refresh_token": strings.Repeat("x", 8192),
		}, cryptor)
	}
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, cryptor)
	ctx := adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"accounts.credentials"}, AccountTypes: []string{"oauth"}, AllGroups: true, IncludeUngrouped: true}}}})
	for _, tc := range []struct {
		name     string
		identity service.ScopedCodexIdentity
		want     int64
	}{
		{"精确组合优先于token及account回退", service.ScopedCodexIdentity{AccountID: "workspace", UserID: "member", AccessToken: "shared"}, 1},
		{"仅token不能借用相同workspace", service.ScopedCodexIdentity{AccountID: "workspace", UserID: "member", AccessToken: "missing", AccessTokenOnly: true}, 0},
		{"token命中不施加account用户冲突", service.ScopedCodexIdentity{AccountID: "workspace", UserID: "member", AccessToken: "shared", AccessTokenOnly: true}, 2},
		{"account回退允许缺失用户", service.ScopedCodexIdentity{AccountID: "workspace", UserID: "new-member", AccessToken: "missing"}, 3},
		{"仅user不匹配其他workspace", service.ScopedCodexIdentity{UserID: "member", AccessToken: "missing"}, 4},
		{"email忽略大小写空格但不借用workspace", service.ScopedCodexIdentity{Email: "test@example.com", AccessToken: "missing"}, 5},
		{"AgentIdentity仅account", service.ScopedCodexIdentity{AccountID: "workspace", AccessToken: "different-workspace", AgentIdentity: true}, 3},
		{"AgentIdentity不回退token", service.ScopedCodexIdentity{AccountID: "missing", AccessToken: "shared", AgentIdentity: true}, 0},
		{"空身份不查询全量", service.ScopedCodexIdentity{}, 0},
		{"数值身份遵循原float64归一化", service.ScopedCodexIdentity{AccountID: "123", UserID: "9007199254740992", AccessToken: "rotated-token"}, 7},
		{"不同workspace不能以user合并", service.ScopedCodexIdentity{AccountID: "new-workspace", UserID: "member"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := repo.ScopedAdminCodexCandidates(ctx, tc.identity)
			require.NoError(t, err)
			if tc.want == 0 {
				require.Empty(t, rows)
				return
			}
			require.Len(t, rows, 1)
			require.Equal(t, tc.want, rows[0].ID)
			require.NotContains(t, rows[0].Credentials, "refresh_token")
			require.NotContains(t, rows[0].Credentials, "large_extra")
		})
	}
	// 同一身份有大量旧副本时仍只取 ID 最大者；首尾 Unicode 空白与导入器一致。
	for id := int64(2000); id <= 3000; id++ {
		_, err = db.Exec(`INSERT INTO accounts(id,platform,type) VALUES ($1,'openai','oauth')`, id)
		require.NoError(t, err)
		setAccountCredentials(t, db, id, map[string]any{
			"chatgpt_account_id": "\tworkspace　",
			"chatgpt_user_id":    "member",
			"access_token":       "duplicate",
			"refresh_token":      strings.Repeat("x", 8192),
		}, cryptor)
	}
	rows, err := repo.ScopedAdminCodexCandidates(ctx, service.ScopedCodexIdentity{AccountID: "workspace", UserID: "member"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.EqualValues(t, 3000, rows[0].ID)
	require.Len(t, rows[0].Credentials, 4)
}
