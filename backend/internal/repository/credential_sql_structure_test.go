package repository

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 阶段二的结构性判据（tasks 2.5）。
//
// 为什么需要它：阶段二把 51 处直接读 `credentials` 列的 SQL 换成新列，其中
// **每一处改错都不会报错**——`->>` 作用在密文上是静默返回 NULL，
// 等值比较在 NULL 上恒假。这类改动不会让编译红、也不会让既有测试红，
// 只会让线上"某些账号突然查不到"。所以必须有一条测试把剩余清单钉死。
//
// 本测试断言的是**当前刻意保留**的清单，不是"越少越好"：
//   - 整文档等值比较：0 处（全部改比 credentials_hash）
//   - 子键提取：8 处，且每一处都必须属于下面两类之一
//       (a) api_key 的取值类（SELECT ... AS api_key / GROUP BY ...）—— 指纹替代不了，
//           需要把 api_key 本身取出来，属阶段四前置（tasks 2.2c）
//       (b) refresh_token 的存在性判断 —— 密钥、不在 meta 白名单，也不是等值比较，
//           同样属阶段四前置
//   - 明文列的 SET 写：必须仍存在（阶段四之前仍要写明文，否则回滚路径没了）

// 两种写法都要扫：
//  1. 裸 SQL 里的 `credentials ->> 'x'`
//  2. ent Aggregate 里的 `s.C(dbaccount.FieldCredentials) + "->'x'"` —— 上一版只认第 1 种，
//     于是 custom_admin_key_scope.go 那两处（其中一个抽的是 access_token）完全没被看见。
var credentialColumnRefRe = regexp.MustCompile(`credentials\s*->>?\s*'([a-z_]+)'|FieldCredentials\)\s*\+\s*"[^"]*->>?\s*'([a-z_]+)'`)

func productionRepositorySources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "account_credentials_crypto.go" || name == "account_credentials_backfill.go" {
			// 这两个文件是迁移工具与派生逻辑本身，不是业务查询
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(name))
		require.NoError(t, err)
		out[name] = string(raw)
	}
	return out
}

// TestCredentialSQLStructure_OnlyKnownSubkeySitesRemain 钉死剩余的子键提取清单。
func TestCredentialSQLStructure_OnlyKnownSubkeySitesRemain(t *testing.T) {
	// 站点 A（方案 ①）落地后，生产侧对 credentials 列的子键提取应当**归零**：
	//   - api_key 的取值类 → 已改走指纹分组
	//   - SetGrokOAuthError 那处 → 已改成 Go 侧早返回
	//   - ListOAuthRefreshCandidatePage 那处 → 已读 credentials_meta 的派生事实
	// 所以这里不再有白名单条目；新增任何一处都会让本测试变红。
	// 现在**没有任何白名单条目**：生产侧对明文列的子键提取已归零。
	// 归零不是靠"少扫一点"，而是逐一改掉的：
	//   - api_key 取值类 → 指纹分组
	//   - SetGrokOAuthError → Go 侧早返回
	//   - ListOAuthRefreshCandidatePage → credentials_meta 的派生事实
	//   - scoped-admin Codex 身份匹配 → Go 侧解密后匹配（A4 选项 C，`3c9b947e1`）
	//   - scoped-admin 列表的 model_mapping → 解密后取值（Go 侧）
	// 新增任何一处都会变红，且**必须同时更新 13b/5.2 的台账**。
	allowed := map[string][]string{}

	var unexpected []string
	total := 0
	for name, src := range productionRepositorySources(t) {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			matches := credentialColumnRefRe.FindAllString(trimmed, -1)
			if len(matches) == 0 {
				continue
			}
			total += len(matches)
			ok := false
			for _, allow := range allowed[name] {
				if strings.Contains(trimmed, allow) {
					ok = true
					break
				}
			}
			if !ok {
				unexpected = append(unexpected, name+": "+trimmed)
			}
		}
	}

	sort.Strings(unexpected)
	require.Empty(t, unexpected,
		"出现了清单外的 `credentials -> ...` 子键提取。阶段二之后这类新增几乎一定是漏改：%v", unexpected)
	// 0 就是终点：生产侧不再有任何 SQL 从明文列抽子键。
	// 这个数字曾经是 8 → 1 → 0，每一次下降都对应一处改造；
	// 它一旦回升，说明有人又把子键提取写回了 SQL（`->>` 作用在密文上是静默 NULL，
	// 不会报错，只会让"某些账号突然查不到"）。
	require.Zero(t, total,
		"生产侧的子键提取应为 0 处。上升说明有 SQL 又开始吃明文列——"+
			"drop 之后那类语句会运行期报错，或更糟：静默返回 NULL")
}

// TestCredentialSQLStructure_NoWholeDocumentComparisonsLeft 整文档等值比较必须清零。
//
// 这类比较集中在乐观并发（CAS）与身份守卫上。只要有一处还比 jsonb 明文，
// 阶段四 drop 明文列之后该 CAS 就会永远不命中（或永远命中），且不报错。
func TestCredentialSQLStructure_NoWholeDocumentComparisonsLeft(t *testing.T) {
	// 只找**比较**，不找赋值：赋值长这样 `SET credentials = $1::jsonb` / `"credentials = ..."`。
	// 用负向断言排除 `SET `/`"`/`, ` 前缀。
	re := regexp.MustCompile(`(?:a\.)?credentials\s+(?:IS\s+(?:NOT\s+)?DISTINCT\s+FROM|=)\s+\$\d+`)
	// 赋值有四种形态：`SET credentials = $1::jsonb,`（同行）、
	// `credentials = $1::jsonb,`（SET 的续行，行首就是字段名）、
	// `"credentials = COALESCE(credentials, ...)"`（拼接进 SET 子句）、
	// 以及 `credentials = ...` 前面带逗号的写法。
	assignmentPrefix := regexp.MustCompile(`^(?:SET\s+)?credentials\s*=\s*(?:\$|COALESCE\()|["]credentials\s*=\s*(?:\$|COALESCE\()|,\s*credentials\s*=\s*(?:\$|COALESCE\()`)

	var offenders []string
	for name, src := range productionRepositorySources(t) {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if assignmentPrefix.MatchString(trimmed) {
				continue
			}
			if re.MatchString(trimmed) {
				offenders = append(offenders, name+": "+trimmed)
			}
		}
	}
	sort.Strings(offenders)
	require.Empty(t, offenders,
		"还有整文档等值比较没改比 credentials_hash：%v", offenders)
}

// TestCredentialSQLStructure_NoPlaintextColumnWrites 明文列的 SQL 写入必须为 **0**。
//
// 历史：阶段四（5.2）之前这条守的是反方向 —— "别顺手把明文写入停掉，否则回滚路径消失"，
// 期望值是 3 处（UpdateCredentials 的 SET、UpdateGrokOAuthCredentials 的 SET、
// BulkUpdate 的合并）。5.2 把两列物理删除之后，这 3 处都已经改成写派生列，
// 于是同一条守卫翻面：**再出现任何一处写明文列，就是代码回退到了已删除的列上**，
// 上线即 `column "credentials" does not exist`。
//
// 判据与实现解耦：只扫生产源码里的赋值形态（`SET credentials = $n` / `credentials = COALESCE(...)`），
// 跳过注释行 —— 这条守卫自己就踩过"注释里引用了被禁字面量"的坑。
func TestCredentialSQLStructure_NoPlaintextColumnWrites(t *testing.T) {
	re := regexp.MustCompile(`^(?:SET\s+)?credentials\s*=\s*\$|["]credentials\s*=\s*COALESCE\(credentials`)

	found := 0
	for _, src := range productionRepositorySources(t) {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if re.MatchString(trimmed) {
				found++
			}
		}
	}
	require.Zero(t, found,
		"仍有 %d 处 SQL 在写明文列 credentials —— 阶段 5.2 已把它 drop 掉，"+
			"这些语句上线即 `column \"credentials\" does not exist`", found)
}

// TestCredentialSQLStructure_QuotedIdentifierSubkeyRefsArePinned
//
// 第三形态：**带引号的标识符** —— `"accounts"."credentials"->'key'`。
// 前两条守卫只认 `credentials ->'字面量'` 与 `s.C(FieldCredentials) + "->'x'"`，
// 而 scoped-admin 的 Codex 身份匹配是用
//
//	raw := `"accounts"."credentials"->'` + key + `'`
//
// 拼出来的：`->'` 后面跟的是**变量**，上面两个正则都匹配不到。
// 结果是一整族谓词（account_user / user / email / access_token）从来没被守卫看见，
// 而它们每一条在 drop `credentials` 之后都会运行期报错 —— 属于 13b 之外的**新发现**。
//
// 本测试只做一件事：把这种写法的出现次数钉死（当前 2 处，都在
// scopedCodexIdentityPredicates 的 field() 里，一个 raw 一个 value）。
//
// ⚠️ `3c9b947e1` 之后，`scopedCodexIdentityPredicates` **已不在生产路径上**：
// `ScopedAdminCodexCandidates` 改成 Go 侧匹配（design.md A4 选项 C），这族 SQL
// 只作为**等价性仪器**的对照实现保留（TestScopedCodexIdentityPredicates_SQLAndGoAgree
// 用它逐条比对）。所以这里的 2 处不是阻塞，而是"oracle 还在"。
// 若有人把它重新接回生产读取路径，必须同时把 13b 的那一行重新标成阻塞。
// 让这一类不能继续静默增长。
func TestCredentialSQLStructure_QuotedIdentifierSubkeyRefsArePinned(t *testing.T) {
	// 引号紧跟 -> 才算这一族（避免把 `credentials ->> 'x'` 之类算进来）
	re := regexp.MustCompile(`credentials"\s*->>?`)

	const wantTotal = 2
	const wantFile = "custom_admin_key_scope.go"

	var hits []string
	total := 0
	for name, src := range productionRepositorySources(t) {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			// 数**出现次数**而不是行数：同一行里再拼一次也是新增一处，
			// 按行计数会漏掉（反向实验时实测：同加倍一处仍绿）。
			matches := re.FindAllString(trimmed, -1)
			if len(matches) == 0 {
				continue
			}
			total += len(matches)
			hits = append(hits, name+": "+trimmed)
		}
	}

	require.Equal(t, wantTotal, total,
		"带引号标识符的 credentials 子键引用应为 %d 处（%s 的 field()）。"+
			"这一族是 drop 明文列的阻碍（含 access_token）：新增一处说明又有 SQL 直接吃了明文列",
		wantTotal, wantFile)
	for _, h := range hits {
		require.Contains(t, h, wantFile,
			"出现了清单外的带引号子键引用（%s）；这一类必须显式登记并纳入阶段四改造清单", h)
	}
}
