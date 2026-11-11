package repository

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	dbent "github.com/Wei-Shaw/sub2api/ent"
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
	const wantSites = 4 // 新增/删除回落站点时改这个数，并同步 tasks.md 3.3

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

// 运行层：真的走一次回落（accounts 读路径），断言打出来的日志含标记词。
// 只验源码文本的话，把标记词写进注释也能过。
func TestPlaintextFallbackLogMarker_EmittedAtRuntime(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	// accounts：有明文、无密文、无加密器 → 走回落分支并打日志。
	repo := newAccountRepositoryWithSQL(nil, nil, nil)
	resetCredentialPlaintextFallbackCount()
	got, err := repo.credentialsFromEntity(&dbent.Account{
		ID:          4242,
		Credentials: map[string]any{"api_key": "sk-marker"},
	})
	require.NoError(t, err)
	require.Equal(t, "sk-marker", got["api_key"])
	require.Equal(t, int64(1), CredentialPlaintextFallbackCount(), "前提：确实走了回落分支")

	t.Logf("captured: %q", buf.String())
	require.Containsf(t, buf.String(), "plaintext fallback",
		"回落日志里没有标记词 — 3.3 的 grep 在生产上会恒为 0（buf=%q）", buf.String())
}
