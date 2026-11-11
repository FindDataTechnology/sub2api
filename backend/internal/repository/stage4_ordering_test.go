package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestStage4Ordering_EntSchemaMustDropFieldBeforeColumn 钉住**阶段四的执行顺序**。
//
// 为什么需要这条：ent 生成的 SELECT 会把 schema 里声明的**所有字段**都列出来。
// 所以如果先把数据库列 drop 掉、而 ent schema 还留着 `credentials` 字段，
// 那么**所有** ent 查询都会 `column "credentials" does not exist` —— 不是某条路径坏，
// 是整个账号读写全挂。这个顺序搞反的代价比"漏改一处 SQL"大得多。
//
// 正确顺序：① ent schema 删字段 + `go generate` → ② 删 Go 调用点 → ③ 才 drop DB 列。
func TestStage4Ordering_EntSchemaMustDropFieldBeforeColumn(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, client.Schema.Create(ctx))

	// 造一行，确认 ent 路径本来是通的
	_, err = client.Account.Create().
		SetName("stage4-probe").
		SetPlatform("openai").
		SetType("apikey").
		SetCredentials(map[string]any{"api_key": "sk-x"}).
		Save(ctx)
	require.NoError(t, err)
	_, err = client.Account.Query().All(ctx)
	require.NoError(t, err, "drop 之前 ent 查询必须正常")

	// 模拟"顺序搞反"：ent schema 仍有 credentials 字段，但 DB 列被 drop
	_, err = db.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN credentials`)
	require.NoError(t, err)

	_, err = client.Account.Query().All(ctx)
	require.Error(t, err, "ent schema 还声明着 credentials 时，drop 列会让**所有** ent 查询失败")
	require.Contains(t, err.Error(), "credentials",
		"失败原因必须明确指向这一列，否则排查会误以为是别的问题")
	t.Logf("预期失败信息: %v", err)
}
