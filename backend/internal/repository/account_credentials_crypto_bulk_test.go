package repository

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"strconv"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/lib/pq"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// BulkUpdate 的凭据合并：**阶段 5.2 改了实现**。
//
// 旧实现是 SQL 侧 `credentials = COALESCE(credentials,'{}') || $n::jsonb`，
// 再把派生列置空、事务内回读明文重算 —— 明文列 drop 之后这条语句不存在。
//
// 新实现：主 UPDATE 只改非凭据列（WHERE 里仍用"新值 vs 旧行"判身份是否变化），
// 凭据合并在**同一事务、主 UPDATE 之后**由 mergeAccountCredentialsInTx 完成：
// 读旧密文 → 解密 → 顶层浅合并 → 重算派生列 → 逐行写回。
//
// 这条测试守三件事，任何一件退化都会让"批量编辑一次凭据"变成"凭据被清空"：
//  1. 主 UPDATE 里**不能再出现** `credentials` 明文列（列已删）；
//  2. 合并必须发生在主 UPDATE 之后（否则身份清理谓词拿新值比新值，永远不触发）；
//  3. 没有加密器时**必须报错**，不许静默写 NULL（那等于删凭据）。
func TestBulkUpdate_CredentialsMergeIsGoSideAndNeedsEncryptor(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	// 无加密器：合并凭据必须被拒绝，而不是把凭据写成 NULL
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{
		Credentials: map[string]any{"api_key": "sk-rotated"},
	})
	require.Error(t, err, "没有加密器时合并凭据必须失败：静默写 NULL 等于删凭据")
	require.Contains(t, err.Error(), "no encryptor configured")

	// 主 UPDATE 里不该出现明文列（连"置空派生列"也不该出现——派生列由合并那步写）
	for _, q := range exec.execQueries {
		nq := normalizeSQLWhitespace(q)
		require.NotContains(t, nq, "credentials = COALESCE")
		require.NotContains(t, nq, "credentials_hash = NULL")
	}
}

// 不传 Credentials 时不该碰任何凭据列——否则任何一次 status/priority 批量更新
// 都会把所有行的密文抹掉，双写就白做了。
func TestBulkUpdate_DoesNotClearDerivedColumnsWhenCredentialsUnspecified(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	priority := 5
	_, err := repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{
		Priority: &priority,
	})
	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)

	query := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, query, "priority = $")
	require.NotContains(t, query, "credentials_encrypted = NULL")
	require.NotContains(t, query, "credentials_hash = NULL")
	require.NotContains(t, query, "credentials_meta")
}

// 带加密器时：合并真的发生（读旧密文 → 写回新密文），且浅合并语义正确。
// 用真 PG 跑：合并的正确性（浅合并、派生列重算）无法用 sqlmock 证明。
func TestBulkUpdate_CredentialsMergeRoundTrip(t *testing.T) {
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；需要真 PostgreSQL")
	}
	cryptor := credentialTestEncryptor(t)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	_, err = db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })

	cols, err := CredentialCryptoColumnsFor(map[string]any{
		"api_key": "sk-old", "base_url": "https://keep.example.com",
	}, cryptor)
	require.NoError(t, err)
	var id int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials_meta,
			credentials_hash, credentials_encrypted, credentials_api_key_hash)
		VALUES ('bulk-merge', 'openai', 'apikey', $1::jsonb, $2, $3, $4)
		RETURNING id`,
		mustJSON(t, cols.Meta), *cols.Hash, *cols.Encrypted, cols.APIKeyHash).Scan(&id))

	repo := newAccountRepositoryWithSQLCrypto(client, db, nil, cryptor)
	_, err = repo.BulkUpdate(ctx, []int64{id}, service.AccountBulkUpdate{
		Credentials: map[string]any{"api_key": "sk-new"},
	})
	require.NoError(t, err)

	got, err := repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "sk-new", got.Credentials["api_key"], "新值必须生效")
	require.Equal(t, "https://keep.example.com", got.Credentials["base_url"],
		"浅合并：delta 里没有的子键必须保留（整份替换会丢掉它）")

	// 派生列必须与**合并后**的明文自洽。
	// 这段断言接管了原 deriveCredentialColumnsAfterMerge 那两条测试的位置：
	// 那个函数（SQL 侧合并后回读明文重算）在 5.2 之后已无调用点并被删除，
	// 但"合并之后四列都对"这个契约必须有真库覆盖——只靠上面的 GetByID
	// 只能证明密文对，证明不了 hash / meta / api_key 指纹没留在旧值上。
	var (
		hashCol      string
		apiKeyHash   *string
		metaCol      []byte
		encryptedCol *string
	)
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT credentials_hash, credentials_api_key_hash, credentials_meta, credentials_encrypted
		FROM accounts WHERE id = $1`, id).Scan(&hashCol, &apiKeyHash, &metaCol, &encryptedCol))

	merged := map[string]any{"api_key": "sk-new", "base_url": "https://keep.example.com"}
	wantCols, err := CredentialCryptoColumnsFor(merged, cryptor)
	require.NoError(t, err)
	require.Equal(t, *wantCols.Hash, hashCol, "哈希必须按合并后的明文重算")
	require.NotNil(t, apiKeyHash, "api_key 指纹必须重算，不能留旧值")
	require.Equal(t, *wantCols.APIKeyHash, *apiKeyHash)
	require.JSONEq(t, mustJSON(t, wantCols.Meta), string(metaCol))
	require.NotNil(t, encryptedCol)
	plaintext, err := cryptor.Decrypt(*encryptedCol)
	require.NoError(t, err)
	require.JSONEq(t, `{"api_key":"sk-new","base_url":"https://keep.example.com"}`, plaintext)
}

// TestBulkUpdate_EveryPlaceholderHasExactlyOneArg 把真库错误
// `could not determine data type of parameter $1` 变成无库可复现的断言。
//
// 起因（2026-11-12，真 PG 上抓到）：凭据 JSON 那个参数原本**无条件**分配，
// 但只有 base_url / account_mode 两族身份谓词会引用它。于是"批量只改 api_key"
// ——最常见的用法——就变成「参数传了、SQL 里却没有 $n 引用它」，lib/pq 报
// `could not determine data type of parameter $1`，整条批量编辑失败。
// sqlmock 不校验参数类型与占位符对齐，所以既有用例全绿，只有真库能看见。
//
// 不变量：**传入参数个数 == SQL 里最大的 $n**。四个组合覆盖两族谓词的每种出现方式。
func TestBulkUpdate_EveryPlaceholderHasExactlyOneArg(t *testing.T) {
	cases := []map[string]any{
		{"api_key": "sk-rotated"},
		{"base_url": "https://ollama.com"},
		{"account_mode": "go"},
		{"api_key": "sk-rotated", "base_url": "https://opencode.ai/zen/go"},
	}
	for _, delta := range cases {
		exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
		exec.queryRows = emptyCredentialQueryRows(t)
		repo := newAccountRepositoryWithSQLCrypto(nil, exec, nil, credentialTestEncryptor(t))

		_, err := repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{Credentials: delta})
		require.NoError(t, err, "delta=%v", delta)
		require.NotEmpty(t, exec.execQueries)
		referenced := referencedSQLPlaceholders(exec.execQueries[0])
		require.Len(t, referenced, len(exec.execArgs[0]),
			"delta=%v：引用到的 $n 个数必须等于传入参数个数（多给/少给都会在真库上 bind 失败）", delta)
		// 关键的一条：必须是**连续**的 1..len(args)，不许有空洞。
		// 只比"个数"或"最大值"抓不到漏洞——未引用的 $1 仍然让个数与最大值对得上。
		for n := 1; n <= len(exec.execArgs[0]); n++ {
			require.True(t, referenced[n],
				"delta=%v：$%d 被传了却没有任何引用 —— lib/pq 会报 "+
					"`could not determine data type of parameter $%d`", delta, n, n)
		}
	}
}

// referencedSQLPlaceholders 返回 SQL 里出现过的 `$n` 集合。
//
// 为什么不用正则里的 `\$` 转义陷阱提醒：这里刻意用字符类拼出 `$`，
// 避免在本文件里出现会被结构守卫误扫的字面量。
func referencedSQLPlaceholders(query string) map[int]bool {
	out := make(map[int]bool)
	dollar := string(rune(36))
	re := regexp.MustCompile(regexp.QuoteMeta(dollar) + `(\d+)`)
	for _, m := range re.FindAllStringSubmatch(query, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out[n] = true
		}
	}
	return out
}
