package repository

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 把 `scripts/credential-migration-status.sh` 这个**运维真正会敲的东西**钉进回归。
//
// 为什么必须单独一条：判据的 SQL 在仓库里存在两份 —— 一份在脚本里（运维用），
// 一份在 Go 测试里（当断言用）。测试里那份绿，只证明"我抄的那份"对；
// 脚本改了、或哪一项的谓词漏了、或 drop 之后它开始假红，测试全都不会响。
// 2026-11-11 手工在真实 300 表 schema 上跑过三档（脏 exit 1 / 回填后 exit 0 /
// drop 后降级 exit 0），这里把它变成可重复执行的断言。
//
// 三档之间只差一个事实，所以"档与档的退出码不同"本身就是判据在分辨东西的证据；
// 空 key 那一档额外证明当年把回填工具判错的那个例外在真约束下也成立。
func TestStatusScript_OnMigratedSchema_ThreeStates(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	const script = "../../scripts/credential-migration-status.sh"
	if _, err := os.Stat(script); err != nil {
		t.Skipf("找不到状态脚本（%s）：%v", script, err)
	}
	if _, err := exec.LookPath("psql"); err != nil && os.Getenv("PG_BIN") == "" {
		t.Skip("psql 不在 PATH 且未设 PG_BIN；状态脚本需要真 psql")
	}

	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db), "真实迁移链（含 249 派生列 / 250 meta 事实）")
	restoreLegacyPlaintextColumns(t, db) // 状态脚本的 pre 分支要求这两列存在

	run := func() (string, int) {
		// script 与 dsn 都是本测试自己构造的，不是外部输入。
		cmd := exec.Command(script, dsn) //nolint:gosec // 参数来自测试内部
		cmd.Env = os.Environ()           // PG_BIN（如设）由脚本自己处理
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0
		}
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); !ok {
			t.Fatalf("状态脚本无法执行：%v\n%s", err, out)
		}
		return string(out), exitErr.ExitCode()
	}

	// ── 档 1：造"迁移前形态"的待回填行 → 脚本必须 exit 1 并逐项点名 ──
	var userID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash) VALUES ('status@example.test', 'x') RETURNING id`).Scan(&userID))
	_, err = db.ExecContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials, credentials_meta)
		VALUES ('pending-acc', 'openai', 'apikey', '{"api_key":"sk-status-zzzzzzzzzzzz"}', '{}'::jsonb)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO api_keys (user_id, key, name, status)
		VALUES ($1, 'sk-status-key-zzzz', 'pending', 'active')`, userID)
	require.NoError(t, err)

	out, code := run()
	t.Logf("档 1（脏）exit=%d\n%s", code, out)
	require.Equal(t, 1, code, "有待回填行时脚本必须 exit 1（否则发布门形同虚设）")
	require.Contains(t, out, "结论：**不可往下走**")
	for _, want := range []string{"↳ 跑 cmd/credential-backfill -apply", "↳ 迁移 250 未跑过或只跑了一半",
		"↳ 同上（回填工具会一起处理 api_keys）", "↳ 有未软删的 key 没有指纹"} {
		require.Contains(t, out, want, "四项各自要打印自己的补救说明，缺一条就是某项静默没查")
	}
	require.Contains(t, out, "F 明文列                        : 仍在")

	// ── 档 2：回填之后 → 四项归零、exit 0 ──
	result, err := BackfillCredentialColumns(ctx, db, cryptor, CredentialBackfillOptions{})
	require.NoError(t, err)
	require.Zero(t, result.AccountsFailed)
	require.Zero(t, result.APIKeysFailed)
	out, code = run()
	t.Logf("档 2（回填后）exit=%d\n%s", code, out)
	require.Equal(t, 0, code, "四项归零后脚本必须 exit 0，否则运维会被永久挡住")
	require.Contains(t, out, "结论：判据全为 0 —— 可以往下走下一步")

	// ── 档 2b：插一条**合法空 key** → 仍 exit 0（那个把回填工具判错过的例外）──
	_, err = db.ExecContext(ctx, `
		INSERT INTO api_keys (user_id, key, name, status) VALUES ($1, '', 'empty-legal', 'active')`, userID)
	require.NoError(t, err)
	out, code = run()
	t.Logf("档 2b（含合法空 key）exit=%d\n%s", code, out)
	require.Equal(t, 0, code, "空 key 是合法终态（`key <> ''` 例外）；判成 FAIL 会让闸门永远过不去")
	require.Contains(t, out, "结论：判据全为 0 —— 可以往下走下一步")

	// ── 档 3：drop 明文列之后 → 不崩、不假红，并说明进入阶段四 ──
	_, err = db.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN credentials; ALTER TABLE api_keys DROP COLUMN key`)
	require.NoError(t, err)
	out, code = run()
	t.Logf("档 3（drop 后）exit=%d\n%s", code, out)
	require.Equal(t, 0, code, "drop 之后脚本仍要能跑：它是发布后体检的唯一入口")
	require.Contains(t, out, "跳过（key 列已 drop，无法区分空 key 与漏回填）",
		"B 项必须主动说明自己为何降级，而不是悄悄换口径")
	require.Contains(t, out, "跳过（credentials 列已不存在）")
	require.Contains(t, out, "F 明文列                        : 已 drop（阶段四完成）")
	require.NotContains(t, out, "FAIL")
}

// asExitError 是 errors.As 的薄包装，避免为一条断言引入 import 分组。
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
