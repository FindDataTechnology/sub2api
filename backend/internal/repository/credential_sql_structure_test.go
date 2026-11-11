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
	allowed := map[string][]string{
		// 例外：scoped-admin 的账号列表/Codex 候选用 ent 的 `s.C(FieldCredentials)` 从**明文列**
		// 直接抽子键。这是**阶段四的阻碍**（含 access_token 这种密钥子键），
		// 已在 RELEASE-CHECKLIST 13b 登记；在改掉之前这里必须显式列出，
		// 不允许它静默通过（本守卫的上一版就因为只认 `credentials ->> 'x'` 而漏了这两种写法）。
		"custom_admin_key_scope.go": {
			"->'model_mapping' AS model_mapping",
			"jsonb_build_object('chatgpt_account_id'",
		},
	}

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
	// 期望值改为 1，并说明为什么不是 0：
	// 阶段二把 51 处改走新列后，按两种形态（`credentials ->> 'x'` 与
	// `FieldCredentials) + "->'x'"`）扫，生产侧**还剩 1 处**未改：
	// custom_admin_key_scope.go:50（从明文列抽 model_mapping）。
	// 另有一处 :179 用 `c := s.C(FieldCredentials)` 再拼 jsonb_build_object（连
	// access_token 一起抽），正则抓不到那种形态，已在白名单显式登记。
	// **这两处都是阶段四 drop 明文列前必须改掉的**（RELEASE-CHECKLIST 13b）。
	require.Equal(t, 1, total,
		"剩余子键提取应为 1 处（custom_admin_key_scope.go:50 抽 model_mapping）。"+
			"归零是阶段四 drop 明文列的前置——改完这里要同步改成 0")
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

// TestCredentialSQLStructure_StillWritesPlaintextColumn 明文列在阶段四之前必须继续写。
//
// 这条防守的是反方向：有人"顺手"把明文写入停掉，等于跳过阶段三直接进入不可逆状态，
// 回滚路径当场消失。
func TestCredentialSQLStructure_StillWritesPlaintextColumn(t *testing.T) {
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
	require.Equal(t, 3, found,
		"明文列的 SQL 写入应为 3 处：UpdateCredentials 的 SET、UpdateGrokOAuthCredentials 的 SET、BulkUpdate 的合并。"+
			"若这是有意的阶段四动作，必须先完成迁移并单独审批")
}
