package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// seedScopedIdentityMatrix 造"身份匹配"用的形状矩阵，返回 ent client 与 行名→ID。
//
// 金标测试与等价性测试**共用同一份夹具**：两份夹具会漂移，而漂移之后
// "仪器绿了"就不再意味着"新实现和旧实现一致"。
func seedScopedIdentityMatrix(t *testing.T) (*dbent.Client, map[string]int64) {
	t.Helper()
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；身份匹配夹具需要真 PostgreSQL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx), "用 ent 自己的 migrate 建表")

	// 形状矩阵：字符串 / 数字 / 前后空白 / 缺字段 / 空串 / 大小写。
	matrix := []struct {
		name  string
		creds map[string]any
	}{
		{"plain", map[string]any{"chatgpt_account_id": "acc-1", "chatgpt_user_id": "user-1", "email": "One@Example.com", "access_token": "tok-1"}},
		{"numeric-id", map[string]any{"chatgpt_account_id": 12345, "chatgpt_user_id": "user-2", "email": "two@example.com", "access_token": "tok-2"}},
		{"padded", map[string]any{"chatgpt_account_id": "  acc-3  ", "chatgpt_user_id": " user-3 ", "email": " three@example.com ", "access_token": "  tok-3  "}},
		{"email-only", map[string]any{"email": "four@example.com", "access_token": "tok-4"}},
		{"empty-ids", map[string]any{"chatgpt_account_id": "", "chatgpt_user_id": "", "email": "five@example.com", "access_token": "tok-5"}},
		{"no-email", map[string]any{"chatgpt_account_id": "acc-6", "chatgpt_user_id": "user-6", "access_token": "tok-6"}},
		{"id-no-user", map[string]any{"chatgpt_account_id": "acc-7", "access_token": "tok-7"}},
		// 这一行专门用来分辨 `lower()`：库里存的是**大写** email。
		{"upper-email", map[string]any{"email": "Six@Example.COM", "access_token": "tok-8"}},
		// 非字符串类型：SQL 侧 CASE 落到 ELSE ''，Go 侧必须给同一个结论。
		{"weird-types", map[string]any{"chatgpt_account_id": true, "chatgpt_user_id": map[string]any{"x": 1}, "email": []any{"a@b"}, "access_token": 42}},
	}

	ids := make(map[string]int64, len(matrix))
	for _, row := range matrix {
		m, err := client.Account.Create().
			SetName("matrix-" + row.name).
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeOAuth).
			SetCredentials(row.creds).
			Save(ctx)
		require.NoError(t, err)
		ids[row.name] = m.ID
	}
	return client, ids
}
