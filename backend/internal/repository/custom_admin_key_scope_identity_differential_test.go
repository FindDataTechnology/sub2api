package repository

import (
	"context"
	"testing"

	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestScopedCodexIdentityPredicates_SQLAndGoAgree 是 A4 选项 C 的**放行闸门**。
//
// 原实现用 SQL 谓词匹配（custom_admin_key_scope.go 的 field()），drop 明文列后失效；
// 新实现改成 Go 侧解密后过滤（custom_admin_key_scope_identity.go）。
// 这个替换的风险不是"报错"，而是**命中到别的账号**——把 A 的凭证合并进 B。
// 所以这里对每一组（行 × 身份 × 谓词序号）逐条比对两侧结论，并要求顺序也一致。
//
// 断言粒度比"最终选中哪一行"更细：只比最终结果的话，两条谓词互换顺序仍可能撞出同一行，
// 而顺序本身是语义（先命中者胜）。
func TestScopedCodexIdentityPredicates_SQLAndGoAgree(t *testing.T) {
	client, ids, cryptor := seedScopedIdentityMatrix(t)
	ctx := context.Background()

	// 覆盖每一支分支：account+user / user-only / email-only / token / account-only /
	// 两种开关（AgentIdentity、AccessTokenOnly）/ 空身份 / 空串 account。
	//
	// `want` 是这一组身份**应当生成几条谓词**。没有它，两侧"都归零"会静默通过——
	// 那正是最容易被当成"等价"的假绿（一个空列表与另一个空列表当然相等）。
	identities := []struct {
		name     string
		identity service.ScopedCodexIdentity
		want     int
	}{
		{"account+user", service.ScopedCodexIdentity{AccountID: "acc-1", UserID: "user-1"}, 2},
		{"account+user(不匹配)", service.ScopedCodexIdentity{AccountID: "acc-1", UserID: "nope"}, 2},
		{"user-only", service.ScopedCodexIdentity{UserID: "user-2"}, 1},
		{"user-only(命中 padded)", service.ScopedCodexIdentity{UserID: "user-3"}, 1},
		{"email-only", service.ScopedCodexIdentity{Email: "six@example.com"}, 1},
		{"email-only(大小写混合输入)", service.ScopedCodexIdentity{Email: "FOUR@Example.COM"}, 1},
		{"email-only(命中空 id 行)", service.ScopedCodexIdentity{Email: "five@example.com"}, 1},
		{"token", service.ScopedCodexIdentity{AccessToken: "tok-3"}, 1},
		{"token(未命中)", service.ScopedCodexIdentity{AccessToken: "tok-none"}, 1},
		{"account-only", service.ScopedCodexIdentity{AccountID: "12345"}, 1},
		{"account+user(u='' 兜底)", service.ScopedCodexIdentity{AccountID: "acc-7", UserID: "zzz"}, 2},
		{"account+token", service.ScopedCodexIdentity{AccountID: "acc-1", AccessToken: "tok-1"}, 2},
		// 顺序判别：block1 按 account+user 命中 no-email，block2 按 token 命中 id-no-user。
		// 两条谓词指向**不同行**，所以只有"顺序正确"才能选中 no-email —— 这是验证
		// "先命中者胜"这条语义的唯一样本，缺了它顺序错了也可能撞出同一行。
		// 顺序判别（真）：
		//   block2(token) 命中 no-email(id 最小), block3(account) 命中 id-no-user(id 更大)
		// 这里 userID 必须**为空**：否则 block1 也会生成，而 block1 的命中必然是 block3 的
		// 超集（a=? AND u=? 蕴含 a=? AND (u='' OR u=?)），两条指向同一行，
		// 顺序怎么换都选中同一行 —— 那样的用例分辨不出顺序（我第一版就写成那样了）。
		{"顺序判别: token 优先于 account", service.ScopedCodexIdentity{AccountID: "acc-7", AccessToken: "tok-6"}, 2},
		// 佐证上面的"超集"关系：有 userID 时 block1/block3 指向同一行，顺序不可观测。
		{"account+user+token(不判别顺序)", service.ScopedCodexIdentity{AccountID: "acc-6", UserID: "user-6", AccessToken: "tok-7"}, 3},
		{"agent-identity", service.ScopedCodexIdentity{AccountID: "acc-6", UserID: "user-6", AgentIdentity: true}, 1},
		{"agent-identity+token", service.ScopedCodexIdentity{AccountID: "acc-6", AccessToken: "tok-6", AgentIdentity: true}, 1},
		{"access-token-only", service.ScopedCodexIdentity{AccountID: "acc-1", AccessToken: "tok-1", AccessTokenOnly: true}, 1},
		// 两个开关都开、且**没有 token** ⇒ 三条谓词全部失效（block1 被两个开关挡住、
		// block2 要 token、block3 被 AccessTokenOnly 挡住）⇒ 一条都不生成，选不中任何行。
		// 这一档专门守"空身份不能因为某条分支漏判而变成万能匹配"。
		{"access-token-only+user(无 token)", service.ScopedCodexIdentity{AccountID: "acc-1", UserID: "user-1", AccessTokenOnly: true}, 0},
		// 两个开关都开、但有 token ⇒ 只有 block2 那条。
		{"access-token-only+token", service.ScopedCodexIdentity{AccountID: "acc-1", AccessToken: "tok-1", AccessTokenOnly: true}, 1},
		{"空身份", service.ScopedCodexIdentity{}, 0},
		{"空串 account", service.ScopedCodexIdentity{AccountID: "", UserID: "user-1"}, 1},
	}

	// 逐行把凭据读回来（与生产路径一致：先取实体再解密/回落）
	rows, err := client.Account.Query().Order(dbent.Desc("id")).All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, len(ids))

	// 与生产一致：夹具带密文，这一路必须真解密才能读回凭据（阶段 3.4 之后没有回落）。
	repo := newAccountRepositoryWithSQLCrypto(nil, client, nil, cryptor)
	credentialsOf := func(m *dbent.Account) map[string]any {
		creds, err := repo.credentialsFromEntity(m)
		require.NoError(t, err)
		return creds
	}

	for _, c := range identities {
		sqlPredicates := scopedCodexIdentityPredicates(c.identity)
		require.Len(t, sqlPredicates, c.want,
			"身份=%s：SQL 侧谓词条数与预期不符（实现的分支条件变了？）——"+
				"条数不对时下面的逐条比对没有意义", c.name)
		// 两侧的谓词条数必须一样：条数不同说明有一侧漏了某个分支
		for _, row := range rows {
			goVerdicts := scopedCodexIdentityMatchVerdicts(credentialsOf(row), c.identity)
			require.Len(t, goVerdicts, len(sqlPredicates),
				"%s / %s：两侧谓词条数不同（Go=%d SQL=%d）——有一侧漏了分支",
				c.name, row.Name, len(goVerdicts), len(sqlPredicates))

			for i, predicate := range sqlPredicates {
				sqlHit, err := client.Account.Query().
					Where(dbaccount.IDEQ(row.ID), predicate).
					Exist(ctx)
				require.NoError(t, err, "%s / %s：第 %d 条谓词执行报错", c.name, row.Name, i)
				require.Equal(t, sqlHit, goVerdicts[i],
					"语义漂移：身份=%s 行=%s 谓词#%d。SQL=%v Go=%v。"+
						"这条不一致意味着 Go 侧归一化（trim/数字展开/大小写/非字符串→空串）与 SQL 不等价，"+
						"直接切读会命中错账号", c.name, row.Name, i, sqlHit, goVerdicts[i])
			}
		}

		// 端到端：选中哪一行也必须一致（这层防的是"逐条都对、但选择规则不同"）
		var sqlPicked int64
		for _, predicate := range sqlPredicates {
			m, err := client.Account.Query().
				Where(predicate).
				Order(dbent.Desc("id")).
				First(ctx)
			if err == nil {
				sqlPicked = m.ID
				break
			}
			require.True(t, dbent.IsNotFound(err), "%s：选择规则查询报错: %v", c.name, err)
		}
		var goPicked int64
		if picked := scopedCodexIdentityPick(rows, credentialsOf, c.identity); picked != nil {
			goPicked = picked.ID
		}
		require.Equal(t, sqlPicked, goPicked,
			"选择结果不一致：身份=%s SQL 选中 %d、Go 选中 %d", c.name, sqlPicked, goPicked)
	}
}
