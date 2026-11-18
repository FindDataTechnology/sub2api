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

// scoped-admin Codex 身份匹配的**语义金标**（design.md A4 的前置仪器）。
//
// 为什么先冻结再改：这一族谓词（`"accounts"."credentials"->'key'`，见
// custom_admin_key_scope.go 的 field()）要在 drop 明文列之前重写成"meta / 指纹 / Go 侧过滤"
// 之一。改错不是报错，而是**命中到别的账号**——把 A 的凭证合并进 B，且事后无从发现。
//
// 所以这里把当前实现的行为逐条钉死：给定形状矩阵，每条谓词按序命中哪一行。
// 任何重写都必须让本表逐元素相同（**包括顺序**，因为调用方是"先命中者胜"）。
//
// 表中的期望值是**从改造前的实现录下来的**，不是推导出来的：它的作用就是回归基线。
// 前置条件：`SUB2API_CREDENTIAL_DIFF_DSN` 指向一个可清空的一次性库。
func TestScopedCodexIdentityPredicates_FrozenShapeMatrix(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；身份匹配金标需要真 PostgreSQL")
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
	// 同 fixture 文件：这条金标比的是改造前的 SQL 谓词，必须有明文列才跑得起来。
	restoreLegacyPlaintextColumns(t, db)

	// 形状矩阵：覆盖字符串 / 数字 / 前后空白 / 缺字段 / 空串 / 大小写。
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
		// 若没有大小写不敏感，用全小写去查就不会命中。
		{"upper-email", map[string]any{"email": "Six@Example.COM", "access_token": "tok-8"}},
	}
	ids := make(map[string]int64, len(matrix))
	for _, row := range matrix {
		m, err := client.Account.Create().
			SetName("frozen-" + row.name).
			SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeOAuth).
			Save(ctx)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `UPDATE accounts SET credentials = $2::jsonb WHERE id = $1`,
			m.ID, mustJSON(t, row.creds))
		require.NoError(t, err)
		ids[row.name] = m.ID
	}

	// 每条谓词单独查一次，复刻调用方的"按序逐条、先命中者胜、ByID desc、Limit 1"。
	matchOf := func(identity service.ScopedCodexIdentity) []int64 {
		var out []int64
		for _, predicate := range scopedCodexIdentityPredicates(identity) {
			m, err := client.Account.Query().
				Where(predicate).
				Order(dbent.Desc("id")).
				First(ctx)
			switch {
			case err == nil:
				out = append(out, m.ID)
			case dbent.IsNotFound(err):
				out = append(out, 0)
			default:
				require.NoError(t, err, "谓词执行报错")
			}
		}
		return out
	}

	// 期望值＝改造前实现的实际输出（回归基线）。索引语义：0=未命中。
	//
	// 读这张表时要留意的两件事（它们都是我预测错、被实现纠正后录下来的）：
	//  1. 这是**有序降级**：`email` 那条谓词只有在 account/user **都为空**时才会被生成；
	//     `user` 那条又要求 account 为空。所以"只给 email"而库里的行有 account id 时，
	//     是**不命中**的——不是大小写或 trim 的问题。
	//  2. 数字形态的 id 会按 SQL 展开成文本（`12345` → `"12345"`），所以按字符串
	//     `"12345"` 能命中它；但 `a = ''` 这类"要求字段为空"的谓词**不会**命中数字行。
	cases := []struct {
		name     string
		identity service.ScopedCodexIdentity
		want     []string // 用行名而不是 ID，换机器/换序都稳定
	}{
		// pred0 = (a=? AND u=?)；pred1 = (a=? AND (u='' OR u=?))
		{"按 account+user 精确匹配", service.ScopedCodexIdentity{AccountID: "acc-1", UserID: "user-1"}, []string{"plain", "plain"}},
		// 只有 user：谓词是 a='' AND u=? —— numeric-id 的 account_id 非空 ⇒ 不命中
		{"只有 user（要求 account 为空）", service.ScopedCodexIdentity{UserID: "user-2"}, []string{"-"}},
		// 只有 email：谓词是 a='' AND u='' AND lower(e)=? —— 只有 id 全空的行才可能命中
		{"只有 email，命中缺 id 的行（大小写不敏感）", service.ScopedCodexIdentity{Email: "FOUR@example.com"}, []string{"email-only"}},
		// 大小写不敏感的**真**判据：库里是 "Six@Example.COM"，用全小写查必须命中。
		// （上一条用的是全小写的库值，去掉 lower() 也照样命中，分辨不出东西。）
		{"只有 email，库值含大写也必须命中", service.ScopedCodexIdentity{Email: "six@example.com"}, []string{"upper-email"}},
		{"只有 email，但候选行带 account id ⇒ 不命中", service.ScopedCodexIdentity{Email: "THREE@example.com"}, []string{"-"}},
		{"access_token 精确匹配（含空白 trim）", service.ScopedCodexIdentity{AccessToken: "tok-3"}, []string{"padded"}},
		{"数字 account_id 按展开后的文本比较", service.ScopedCodexIdentity{AccountID: "12345"}, []string{"numeric-id"}},
		// account 给了 ID、user 不匹配时，靠 `u = ''` 这一支仍能命中缺 user 的行
		{"account 命中 + u='' 兜底", service.ScopedCodexIdentity{AccountID: "acc-7", UserID: "zzz"}, []string{"-", "id-no-user"}},
		// account_id 传空串不等于"未提供"：谓词照样按 a='' 比较，故不命中带 id 的行
		{"空串 account_id 不等于未提供", service.ScopedCodexIdentity{AccountID: "", UserID: "user-1"}, []string{"-"}},
	}

	nameByID := map[int64]string{}
	for name, id := range ids {
		nameByID[id] = name
	}

	for _, c := range cases {
		got := matchOf(c.identity)
		gotNames := make([]string, len(got))
		for i, id := range got {
			if id == 0 {
				gotNames[i] = "-"
				continue
			}
			gotNames[i] = nameByID[id]
		}
		require.Equal(t, c.want, gotNames,
			"身份匹配语义漂移：%s。这份金标是从改造前实现录下来的，"+
				"任何重写（meta / 指纹 / Go 侧过滤）都必须逐元素相同——包括顺序", c.name)
		// 便于人工核对：把实际顺序打出来
		t.Logf("%-42s → %v", c.name, gotNames)
	}
}
