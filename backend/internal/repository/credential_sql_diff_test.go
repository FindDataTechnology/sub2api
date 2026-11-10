package repository

import (
	"database/sql"
	"os"
	"sort"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 阶段二 SQL 改写的**差分仪器**。
//
// 背景：阶段二要把 51 处直接读 `credentials` 列的 SQL 表达式改指
// `credentials_meta` / `credentials_hash` / `credentials_api_key_hash`。
// 现有仓储测试只断言 SQL **文本**（`require.Contains(query, "credentials ->> 'base_url'")`），
// 改完再同步改断言 = 自证：它只能证明"代码里出现了我写的字符串"，
// 证明不了"新旧谓词选中同一批行"。
//
// 本文件用真 PostgreSQL 做差分：同一批 fixture 行，分别用**生产里的同一批 helper**
// 构造旧谓词与新谓词，比较两边选中的 id 集合。
//
// 为什么这很重要：ollama / opencode 的身份清理是**值条件删除**——谓词选错一小撮行，
// 就会静默清掉别的账号的受管快照，或漏清本应清的行。这类错误不会报错，
// 只会表现为"用量数据莫名其妙不对"。
//
// 运行方式（需要真 PG，未设置 DSN 时整组跳过，因此不会污染普通 CI）：
//
//	createdb sub2api_credential_diff
//	SUB2API_CREDENTIAL_DIFF_DSN='postgres:///sub2api_credential_diff?sslmode=disable' \
//	  go test ./internal/repository/ -run 'CredentialSQLDiff' -v
//
// 已知且**刻意接受**的差异（见 TestCredentialSQLDiff_DocumentsEmptyAPIKeyNarrowing）：
// 旧谓词 `jsonb_typeof(credentials -> 'api_key') = 'string'` 把**空串**也算合格；
// 新谓词按指纹是否为 NULL 判断，空串的 api_key 不计指纹 → 不合格。
// 空 api_key 的账号本来也查不到任何用量数据，收窄是对的，但必须显式记录。

const credentialDiffTableDDL = `
CREATE TABLE accounts (
	id                       bigserial PRIMARY KEY,
	name                     text NOT NULL DEFAULT '',
	platform                 text NOT NULL,
	type                     text NOT NULL,
	credentials              jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials_meta         jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials_encrypted    text,
	credentials_hash         varchar(64),
	credentials_api_key_hash varchar(64)
);
`

// 旧谓词（当前生产用的，直接引用生产常量）
func ollamaEligibleOldSQL() string { return ollamaCloudUsageEligibleSQL }

// 新谓词：与旧谓词逐条对位，只把取值来源换掉。
const opencodeGoUsageEligibleMetaSQL = `
	(
		(platform = 'opencode_go'
			AND COALESCE(btrim(credentials_meta ->> 'account_mode') <> 'zen', true))
		OR (platform IN (` + opencodeGoUsageMountPlatformsSQL + `)
			AND ` + opencodeGoBaseURLMatchSQLPrefix + `credentials_meta ->> 'base_url'` + opencodeGoBaseURLMatchSQLSuffix + `)
	)
	AND type = 'apikey'
	AND credentials_api_key_hash IS NOT NULL
`

const ollamaCloudUsageEligibleMetaSQL = `
	platform IN (` + ollamaCloudUsagePlatformsSQL + `)
	AND type = 'apikey'
	AND ` + ollamaCloudBaseURLMatchSQLPrefix + `credentials_meta ->> 'base_url'` + ollamaCloudBaseURLMatchSQLSuffix + `
	AND credentials_api_key_hash IS NOT NULL
`

func opencodeEligibleOldSQL() string { return opencodeGoUsageEligibleSQL }

type credentialDiffFixture struct {
	id         int64
	platform   string
	accountTyp string
	creds      map[string]any
	// emptyAPIKey 标记"api_key 是空串"的行：新旧谓词在这类行上刻意不同。
	emptyAPIKey bool
}

func credentialDiffFixtures() []credentialDiffFixture {
	return []credentialDiffFixture{
		// ── ollama 侧 ────────────────────────────────────────────────
		{id: 1, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com"}},
		{id: 2, platform: "openai", accountTyp: "apikey", creds: map[string]any{"base_url": "https://ollama.com"}}, // 无 api_key
		{id: 3, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-c", "base_url": "https://example.com"}},
		// 域名大小写不敏感（正则逐字母列出两种大小写），但路径 /v1 只接受**小写**：
		// id=4 用大写 V1 必须不合格，id=20 用 /v1 才合格。这一条我最初手推错了，
		// 是差分仪器抓出来的——所以期望值必须由 fixture 语义复核，不能凭印象。
		{id: 4, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-d", "base_url": "https://OLLAMA.com/V1"}},
		{id: 20, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-t", "base_url": "https://OLLAMA.com/v1"}},
		{id: 5, platform: "anthropic", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-e", "base_url": "  https://www.ollama.com:443/v1  "}},
		{id: 6, platform: "ollama", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-f", "base_url": "https://ollama.com"}}, // 平台不在白名单
		{id: 7, platform: "openai", accountTyp: "oauth", creds: map[string]any{"api_key": "sk-g", "base_url": "https://ollama.com"}},  // 类型不对
		{id: 8, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-h"}},                                   // 无 base_url
		{id: 9, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "", "base_url": "https://ollama.com"}, emptyAPIKey: true},
		{id: 10, platform: "minimax", accountTyp: "apikey", creds: map[string]any{"api_key": 42, "base_url": "https://ollama.com"}}, // api_key 非字符串

		// ── opencode 侧 ──────────────────────────────────────────────
		{id: 11, platform: "opencode_go", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-k"}},                                  // account_mode 缺失 → 视为 go
		{id: 12, platform: "opencode_go", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-l", "account_mode": "zen"}},          // zen → 不合格
		{id: 13, platform: "opencode_go", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-m", "account_mode": "go"}},           // go → 合格
		{id: 14, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-n", "base_url": "https://opencode.ai/zen/go"}},
		{id: 15, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-o", "base_url": "https://opencode.ai/zen"}}, // zen（非 Go）→ 不合格
		{id: 16, platform: "deepseek", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-p", "base_url": "https://OpenCode.ai:443/zen/go/v1"}},
		{id: 17, platform: "openai", accountTyp: "oauth", creds: map[string]any{"api_key": "sk-q", "base_url": "https://opencode.ai/zen/go"}},
		{id: 18, platform: "opencode_go", accountTyp: "apikey", creds: map[string]any{"api_key": "", "account_mode": "go"}, emptyAPIKey: true},
		{id: 19, platform: "openai", accountTyp: "apikey", creds: map[string]any{"api_key": "sk-s", "base_url": "https://ollama.com"}}, // 同时满足 ollama
	}
}

func credentialDiffDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SUB2API_CREDENTIAL_DIFF_DSN")
	if dsn == "" {
		t.Skip("SUB2API_CREDENTIAL_DIFF_DSN 未设置；差分测试需要真 PostgreSQL")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`DROP TABLE IF EXISTS accounts; ` + credentialDiffTableDDL)
	require.NoError(t, err)
	return db
}

// seedCredentialDiffRows 用**生产写路径的同一套派生逻辑**填充新列。
// 这一步很关键：如果派生逻辑本身错了（比如白名单漏了 base_url），
// 新谓词就会选不中行，差分立刻暴露，而不是等线上。
func seedCredentialDiffRows(t *testing.T, db *sql.DB) []credentialDiffFixture {
	t.Helper()
	repo := newAccountRepositoryWithSQL(nil, nil, nil) // cryptor=nil，只算 meta 与指纹
	fixtures := credentialDiffFixtures()

	for _, f := range fixtures {
		columns, err := repo.credentialCryptoColumnsFor(f.creds)
		require.NoError(t, err)
		require.NotNil(t, columns.Hash)
		_, err = db.Exec(`
			INSERT INTO accounts (id, platform, type, credentials, credentials_meta, credentials_hash, credentials_api_key_hash)
			VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7)`,
			f.id, f.platform, f.accountTyp, mustJSON(t, f.creds), mustJSON(t, columns.Meta),
			*columns.Hash, columns.APIKeyHash)
		require.NoError(t, err, "seed id=%d", f.id)
	}
	return fixtures
}

func mustJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	raw, err := canonicalCredentialJSON(v)
	require.NoError(t, err)
	return string(raw)
}

func selectIDs(t *testing.T, db *sql.DB, predicate string, args ...any) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM accounts WHERE `+predicate+` ORDER BY id`, args...)
	require.NoError(t, err, "query failed for predicate:\n%s", predicate)
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// TestCredentialSQLDiff_OllamaEligibility 旧谓词（credentials 列）与新谓词
// （credentials_meta + 指纹）必须选中同一批行。
func TestCredentialSQLDiff_OllamaEligibility(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	oldIDs := selectIDs(t, db, ollamaEligibleOldSQL())
	newIDs := selectIDs(t, db, ollamaCloudUsageEligibleMetaSQL)

	// 期望值用 fixture 手工推导，而不是把 oldIDs 当期望——否则实现全错也会"相等"。
	require.Equal(t, []int64{1, 5, 9, 19, 20}, oldIDs, "旧谓词的语义基线")
	require.Equal(t, []int64{1, 5, 19, 20}, newIDs, "新谓词：只少了空 api_key 的 9")
}

func TestCredentialSQLDiff_OpenCodeEligibility(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	oldIDs := selectIDs(t, db, opencodeEligibleOldSQL())
	newIDs := selectIDs(t, db, opencodeGoUsageEligibleMetaSQL)

	require.Equal(t, []int64{11, 13, 14, 16, 18}, oldIDs, "旧谓词的语义基线")
	require.Equal(t, []int64{11, 13, 14, 16}, newIDs, "新谓词：只少了空 api_key 的 18")
}

// TestCredentialSQLDiff_DocumentsEmptyAPIKeyNarrowing 把上面两处差异**显式钉住**。
//
// 新谓词比旧谓词严格一处：`api_key` 为空串的行。旧写法
// `jsonb_typeof(credentials -> 'api_key') = 'string'` 对空串返回 true，
// 新写法按指纹是否为 NULL 判断，空串不产生指纹。
//
// 判定：这个收窄是**对的**。空 api_key 的账号在用量查询里永远查不到数据，
// 把它算作"合格身份"只会让它参与组查询却返回空。但它是行为变化，
// 所以必须有测试把它写下来，而不是让它在上线后靠"用量好像少了"被发现。
func TestCredentialSQLDiff_DocumentsEmptyAPIKeyNarrowing(t *testing.T) {
	db := credentialDiffDB(t)
	fixtures := seedCredentialDiffRows(t, db)

	var expectedOllamaDiff, expectedOpenCodeDiff []int64
	for _, f := range fixtures {
		if !f.emptyAPIKey {
			continue
		}
		if len(selectIDs(t, db, ollamaEligibleOldSQL())) == 0 {
			t.Fatal("fixture 未生效")
		}
		oldOllama := containsID(selectIDs(t, db, ollamaEligibleOldSQL()), f.id)
		newOllama := containsID(selectIDs(t, db, ollamaCloudUsageEligibleMetaSQL), f.id)
		oldOpenCode := containsID(selectIDs(t, db, opencodeEligibleOldSQL()), f.id)
		newOpenCode := containsID(selectIDs(t, db, opencodeGoUsageEligibleMetaSQL), f.id)

		if oldOllama && !newOllama {
			expectedOllamaDiff = append(expectedOllamaDiff, f.id)
		}
		if oldOpenCode && !newOpenCode {
			expectedOpenCodeDiff = append(expectedOpenCodeDiff, f.id)
		}
		// 反向：新谓词 MUST NOT 比旧谓词多选中任何行
		require.False(t, newOllama && !oldOllama, "id=%d：新谓词比旧谓词多选了行", f.id)
		require.False(t, newOpenCode && !oldOpenCode, "id=%d：新谓词比旧谓词多选了行", f.id)
	}
	require.Equal(t, []int64{9}, expectedOllamaDiff)
	require.Equal(t, []int64{18}, expectedOpenCodeDiff)
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestCredentialSQLDiff_APIKeyLookup 按 api_key 找账号：旧写法比 JSONB 子键，
// 新写法比指纹。指纹用生产函数算，保证与实际写入一致。
func TestCredentialSQLDiff_APIKeyLookup(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	const apiKey = "sk-a"
	oldIDs := selectIDs(t, db, `credentials ->> 'api_key' = $1`, apiKey)

	hash := sha256Hex([]byte(apiKey))
	newIDs := selectIDs(t, db, `credentials_api_key_hash = $1`, hash)

	require.Equal(t, []int64{1}, oldIDs)
	require.Equal(t, []int64{1}, newIDs)

	// 不存在的 key 两边都必须空——这是"错 key 必须查不到"的负例
	require.Empty(t, selectIDs(t, db, `credentials ->> 'api_key' = $1`, "sk-不存在"))
	require.Empty(t, selectIDs(t, db, `credentials_api_key_hash = $1`, sha256Hex([]byte("sk-不存在"))))
}

// TestCredentialSQLDiff_WholeDocumentEquality 整文档乐观并发比较：
// 旧写法比 JSONB 文档本身，新写法比 sha256(规范化明文)。
//
// 关键点：同一份内容、**键序不同**，两边都必须判"相等"——否则并发保护会误判
// 为"别人改过"，导致本该生效的 token 轮换被丢弃。
func TestCredentialSQLDiff_WholeDocumentEquality(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	// id=1 的文档是 {"api_key":"sk-a","base_url":"https://ollama.com"}
	// 这里刻意换键序、并让 jsonb 文本化后与写入时不同
	reordered := map[string]any{"base_url": "https://ollama.com", "api_key": "sk-a"}

	oldIDs := selectIDs(t, db, `credentials = $1::jsonb`, mustJSON(t, reordered))
	require.Equal(t, []int64{1}, oldIDs, "旧写法：jsonb 等值本来就忽略键序")

	newIDs := selectIDs(t, db, `credentials_hash = $1`, sha256Hex(mustCanonical(t, reordered)))
	require.Equal(t, []int64{1}, newIDs, "新写法：规范化 JSON 的哈希也必须忽略键序")

	// 内容真的变了就必须不相等
	changed := map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com/v1"}
	require.Empty(t, selectIDs(t, db, `credentials = $1::jsonb`, mustJSON(t, changed)))
	require.Empty(t, selectIDs(t, db, `credentials_hash = $1`, sha256Hex(mustCanonical(t, changed))))
}

func mustCanonical(t *testing.T, v map[string]any) []byte {
	t.Helper()
	raw, err := canonicalCredentialJSON(v)
	require.NoError(t, err)
	return raw
}
