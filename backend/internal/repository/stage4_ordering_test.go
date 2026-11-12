package repository

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 阶段四（5.2 drop 明文列）的**状态守卫**。
//
// 历史：这条测试原本演示"顺序搞反会怎样"——ent schema 还声明着 `credentials`
// 字段、而 DB 列已被 drop 时，**所有** ent 查询都会 `column "credentials" does not exist`
// （不是某条路径坏，是账号读写全挂）。那个演示需要 schema 里仍有字段，而现在
// 字段已经删掉了，演示的前提不存在 —— 于是改成断言**当前就该成立的两个事实**：
//
//	① schema 源码里不再声明这两个明文列（否则 `go generate` 会把它们带回 SELECT）；
//	② 按当前 schema 建出来的表**物理上没有这两列**，且 ent 查询照常工作
//	   （即 drop 之后的真实形态）。
//
// 只要有人把字段加回去，① 立刻红；只要 ent 重新引用不存在的列，② 立刻红。
func TestStage4_SchemaNoLongerDeclaresPlaintextColumns(t *testing.T) {
	for _, f := range []struct{ file, forbidden string }{
		{"../../ent/schema/account.go", `field.JSON("credentials"`},
		{"../../ent/schema/api_key.go", `field.String("key")`},
	} {
		raw, err := os.ReadFile(f.file)
		require.NoError(t, err)
		// 只看**代码行**：注释里引用被删掉的字段是正常的（本 change 到处都这么写），
		// 拿整份文件去搜会被自己的说明文字误伤 —— 这条守卫第一版就是这么红的。
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			require.NotContains(t, trimmed, f.forbidden,
				"%s:%d 又声明了明文列 %s —— 阶段 5.2 已把它删掉（迁移 251）。"+
					"加回来会让 ent 的 SELECT 重新引用一列不存在的列，全站账号查询直接挂",
				f.file, i+1, f.forbidden)
		}
	}
}

// ② 建出来的表必须**没有**这两列，且 ent 查询照常工作。
func TestStage4_SchemaCreatedTablesHaveNoPlaintextColumns(t *testing.T) {
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

	for _, c := range []struct{ table, column string }{
		{"accounts", "credentials"},
		{"api_keys", "key"},
	} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = $1 AND column_name = $2`, c.table, c.column).Scan(&n))
		require.Zero(t, n, "%s.%s 不该再存在（阶段 5.2 已 drop）", c.table, c.column)
	}

	// 派生列必须在（否则凭据无处可存）。
	for _, c := range []struct{ table, column string }{
		{"accounts", "credentials_encrypted"},
		{"accounts", "credentials_meta"},
		{"accounts", "credentials_hash"},
		{"accounts", "credentials_api_key_hash"},
		{"api_keys", "key_encrypted"},
		{"api_keys", "key_hash"},
	} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_name = $1 AND column_name = $2`, c.table, c.column).Scan(&n))
		require.Equal(t, 1, n, "%s.%s 必须存在", c.table, c.column)
	}

	// 造一行并读回：证明 ent 的 SELECT 与明文列无关。
	_, err = client.Account.Create().
		SetName("stage4-probe").
		SetPlatform("openai").
		SetType("apikey").
		SetCredentialsMeta(map[string]any{}).
		Save(ctx)
	require.NoError(t, err)
	rows, err := client.Account.Query().All(ctx)
	require.NoError(t, err, "drop 之后 ent 查询必须正常（SELECT 不再引用明文列）")
	require.Len(t, rows, 1)
}
