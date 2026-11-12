package repository

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 阶段三 3.3 的生产判据是**对日志窗口 grep `plaintext fallback`**，窗口里为 0 才说明
// "这一周期没有人回落读明文"。这条判据有一个致命前提：每个回落站点打的日志里
// **真的有这几个字**。事实是当初没有 —— 四个站点写成三种措辞：
//
//	[CredentialAtRest] plaintext fallback: account=%d …        ← 命中
//	[CredentialAtRest] api key %d has no key_encrypted yet; …   ← grep 不到
//	[APIKeyLookup] plaintext fallback: id=%d …                  ← 命中
//	[APIKeyLookup] ExistsByKey fell back to the plaintext …     ← grep 不到
//
// 于是"没有回落"与"没抓到"在日志上是同形的 —— 这正是本 change 反复踩的那一类
// （守卫只认一种写法 / 清单只登记一半 / 测试只验抄件）。措辞已统一，本文件钉住：
//
//	① 源码层：每个回落日志字面量都必须含标记词，且站点数被写死（新增站点会变红，
//	   逼作者把它纳入 3.3 的判据）；
//	② 运行层：真的触发一次回落，断言打出来的那行含标记词（证明标记词不是只写在注释里）。
func TestPlaintextFallbackLogMarker_EverySiteUsesTheMarker(t *testing.T) {
	const marker = "plaintext fallback"
	// 阶段 3.4 摘掉全部回落分支（accounts 边界 + api key 鉴权/ExistsByKey），
	// 所以**预期站点数 = 0**：这些字只应出现在错误文案与注释里，不该再有一条
	// "正在回落到明文列"的日志。
	//
	// 这条断言现在是**反向守卫**：谁重新引入一个回落日志，checked 就不再是 0，
	// 测试立刻红并逼他回答"3.3 的 grep 判据要不要把这个新站点算进去"。
	const wantSites = 0

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var (
		checked int
		offend  []string
	)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		require.NoError(t, err)
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			lower := strings.ToLower(line)
			if !strings.Contains(lower, "fallback") {
				continue
			}
			// 只认跟（或同行于）LegacyPrintf 的那一行为"日志字面量"；
			// 注释与计数器名不算，避免把说明文字当成站点。
			nearPrintf := strings.Contains(line, "LegacyPrintf(")
			if i > 0 {
				nearPrintf = nearPrintf || strings.Contains(lines[i-1], "LegacyPrintf(")
			}
			if !nearPrintf {
				continue
			}
			// 再收紧到**凭据回落**：本仓还有三句无关的 fallback 日志
			//（outbox 重投、proxy 到期兜底链、用量日志单条重试），它们不该被要求带标记词。
			// 判据是"这句日志在讲某个派生列/明文列"。
			if !strings.Contains(lower, "credential") && !strings.Contains(lower, "key_encrypted") &&
				!strings.Contains(lower, "key_hash") && !strings.Contains(lower, "plaintext column") &&
				!strings.Contains(lower, "key column") {
				continue
			}
			checked++
			if !strings.Contains(line, marker) {
				offend = append(offend, f+":"+line)
			}
		}
	}
	require.Empty(t, offend,
		"回落日志必须含 %q，否则 3.3 的 grep 判据会静默失效：\n%s", marker, strings.Join(offend, "\n"))
	require.Equal(t, wantSites, checked,
		"回落日志站点数变了（%d → %d）：确认新站点也打 %q，然后同步 tasks.md 3.3 与本常量",
		wantSites, checked, marker)
}

// 源码层守的是"回落日志"这个词还存不存在：阶段 3.4 摘掉回落、5.2 drop 掉明文列之后，
// 站点数应为 **0**。谁把一个回落日志加回来，上面那条就会红，逼他回答
// "3.3 的 grep 判据要不要纳入这个新站点"。
//
// 与之配套的**行为层**断言已迁到 `account_credentials_boundary_test.go`：
// 那里钉住"没有密文 = 没有凭据（合法终态，不报错、不计数）"，
// 因为 5.2 之后"有明文无密文"这个状态在数据上不再可表达，
// 继续在这里断言"硬错误"会与真实读路径相矛盾。
