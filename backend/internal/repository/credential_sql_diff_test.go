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
	extra                    jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials              jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials_meta         jsonb NOT NULL DEFAULT '{}'::jsonb,
	credentials_encrypted    text,
	credentials_hash         varchar(64),
	credentials_api_key_hash varchar(64)
);
`

// 旧谓词：**冻结的改动前快照**，刻意不引用生产常量。
//
// 原因：差分测试要拿"改动前的写法"当基准。如果这里引用生产常量，那么一改生产，
// 基准就跟着一起动，测试退化成"新对新"——永远绿、什么都证明不了。
// 只有**列引用**被冻结成字面量（那正是本迁移要动的东西）；
// 平台白名单与 base_url 正则仍引用生产常量（它们不是本迁移的对象，
// 让基准跟着它们走是对的）。
const ollamaCloudUsageEligibleLegacySQL = `
	platform IN (` + ollamaCloudUsagePlatformsSQL + `)
	AND type = 'apikey'
	AND ` + ollamaCloudBaseURLMatchSQLPrefix + `credentials ->> 'base_url'` + ollamaCloudBaseURLMatchSQLSuffix + `
	AND jsonb_typeof(credentials -> 'api_key') = 'string'
`

// 新谓词：引用生产常量，即"现在实际生效的写法"。
func ollamaEligibleOldSQL() string { return ollamaCloudUsageEligibleLegacySQL }
func ollamaEligibleCurrentSQL() string { return ollamaCloudUsageEligibleSQL }

// 新谓词：与旧谓词逐条对位，只把取值来源换掉。
// OpenCode 的冻结快照，理由同上。
const opencodeGoUsageEligibleLegacySQL = `
	(
		(platform = 'opencode_go'
			AND COALESCE(btrim(credentials ->> 'account_mode') <> 'zen', true))
		OR (platform IN (` + opencodeGoUsageMountPlatformsSQL + `)
			AND ` + opencodeGoBaseURLMatchSQLPrefix + `credentials ->> 'base_url'` + opencodeGoBaseURLMatchSQLSuffix + `)
	)
	AND type = 'apikey'
	AND jsonb_typeof(credentials -> 'api_key') = 'string'
`

func opencodeEligibleOldSQL() string     { return opencodeGoUsageEligibleLegacySQL }
func opencodeEligibleCurrentSQL() string { return opencodeGoUsageEligibleSQL }

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

	// 整 schema 重置：同包的 api_keys 测试会用 ent migrate 建完整 schema 并留下外键，
	// 逐表 `DROP TABLE accounts`（无 CASCADE）会被依赖对象挡住。
	_, err = db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public; ` + credentialDiffTableDDL)
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
	newIDs := selectIDs(t, db, ollamaEligibleCurrentSQL())

	// 期望值用 fixture 手工推导，而不是把 oldIDs 当期望——否则实现全错也会"相等"。
	require.Equal(t, []int64{1, 5, 9, 19, 20}, oldIDs, "旧谓词的语义基线")
	// 2.2 之后：空 api_key 的 9 不再合格（旧写法 jsonb_typeof('')='string' 算合格）。
	// 这个收窄是刻意的：空 api_key 的账号在用量查询里永远查不到数据。
	require.Equal(t, []int64{1, 5, 19, 20}, newIDs, "2.2 之后：空 api_key 的 9 被排除")
}

func TestCredentialSQLDiff_OpenCodeEligibility(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	oldIDs := selectIDs(t, db, opencodeEligibleOldSQL())
	newIDs := selectIDs(t, db, opencodeEligibleCurrentSQL())

	require.Equal(t, []int64{11, 13, 14, 16, 18}, oldIDs, "旧谓词的语义基线")
	require.Equal(t, []int64{11, 13, 14, 16}, newIDs, "2.2 之后：空 api_key 的 18 被排除")
}

// TestCredentialSQLDiff_DocumentsEmptyAPIKeyNarrowing 钉住"新谓词只会比旧谓词**更严**"这条不变式。
//
// 两个方向都要成立：
//   - 新谓词 MUST NOT 比旧谓词多选任何行（否则会清理到本来不该碰的账号）；
//   - 允许的差异**只能**来自 `api_key` 为空串的行。旧写法
//     `jsonb_typeof(credentials -> 'api_key') = 'string'` 对空串返回 true，
//     新写法按指纹是否为 NULL 判断，空串不产生指纹。
//
// 写法刻意不硬编码差异集合的当前大小：2.3（换 meta 列）不改变选择结果，
// 2.2（换指纹列）才把空 api_key 行排除。这条测试在两个阶段都必须绿，
// 只是差异集合从空变成 {9}/{18}。
func TestCredentialSQLDiff_DocumentsEmptyAPIKeyNarrowing(t *testing.T) {
	db := credentialDiffDB(t)
	fixtures := seedCredentialDiffRows(t, db)

	oldOllama := selectIDs(t, db, ollamaEligibleOldSQL())
	newOllama := selectIDs(t, db, ollamaEligibleCurrentSQL())
	oldOpenCode := selectIDs(t, db, opencodeEligibleOldSQL())
	newOpenCode := selectIDs(t, db, opencodeEligibleCurrentSQL())

	emptyAPIKeyIDs := map[int64]bool{}
	for _, f := range fixtures {
		if f.emptyAPIKey {
			emptyAPIKeyIDs[f.id] = true
		}
	}

	checkOnlyNarrows := func(label string, oldIDs, newIDs []int64) {
		for _, id := range newIDs {
			require.True(t, containsID(oldIDs, id), "%s：新谓词多选了 id=%d，绝不允许", label, id)
		}
		for _, id := range oldIDs {
			if containsID(newIDs, id) {
				continue
			}
			require.True(t, emptyAPIKeyIDs[id], "%s：id=%d 被排除，但它不是空 api_key 行——这不是本迁移允许的差异", label, id)
		}
	}
	checkOnlyNarrows("ollama", oldOllama, newOllama)
	checkOnlyNarrows("opencode", oldOpenCode, newOpenCode)
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

// ── UpdateCredentials / BulkUpdate 的 CASE 分支所用的叶子谓词 ──────────────────
//
// 为什么只测叶子而不测整段 CASE：
// 阶段二对这些 CASE 的改动**只替换叶子**（把列引用换成 meta / 指纹），
// AND / OR / CASE 求值顺序一字不动。组合逻辑不变时，叶子等价 ⇒ 整体等价。
// 而整段 CASE 因为内联在 raw string 与常量拼接混排的 SQL 里，抽成常量会破坏
// 引号结构（我试过，直接把文件搞到编译不过），硬抽的风险大于收益。
//
// CASE 求值顺序（OpenCode 必须先于 Ollama）属于与列切换正交的既有约束，
// 已有独立测试与源码注释守着，这里不重复。

// seedCredentialDiffRowsWithExtra 与 seedCredentialDiffRows 相同，但额外写入 extra，
// 供"哪些受管键会被清掉"的观察用。
//nolint:unused // 同上：跨 build tag
func seedCredentialDiffRowsWithExtra(t *testing.T, db *sql.DB, extras map[int64]string) {
	t.Helper()
	repo := newAccountRepositoryWithSQL(nil, nil, nil)
	for _, f := range credentialDiffFixtures() {
		columns, err := repo.credentialCryptoColumnsFor(f.creds)
		require.NoError(t, err)
		extra := "{}"
		if e, ok := extras[f.id]; ok {
			extra = e
		}
		_, err = db.Exec(`
			INSERT INTO accounts (id, platform, type, credentials, credentials_meta, credentials_hash, credentials_api_key_hash, extra)
			VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8::jsonb)`,
			f.id, f.platform, f.accountTyp, mustJSON(t, f.creds), mustJSON(t, columns.Meta),
			*columns.Hash, columns.APIKeyHash, extra)
		require.NoError(t, err, "seed id=%d", f.id)
	}
}

// TestCredentialDiffLeaf_WholeDocumentChangedIsDistinct
//
// CASE 的顶层守卫：整份凭据有没有变。
// 旧：`credentials IS DISTINCT FROM $1::jsonb`
// 新：`credentials_hash IS DISTINCT FROM $2`
func TestCredentialDiffLeaf_WholeDocumentChangedIsDistinct(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	cases := []struct {
		name     string
		rowID    int64
		incoming map[string]any
		want     bool
	}{
		{"内容与键序都相同", 1, map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com"}, false},
		{"只有键序不同", 1, map[string]any{"base_url": "https://ollama.com", "api_key": "sk-a"}, false},
		{"内容不同", 1, map[string]any{"api_key": "sk-a2", "base_url": "https://ollama.com"}, true},
		{"多了一个键", 1, map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com", "extra": 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSON(t, tc.incoming)
			hash := sha256Hex(mustCanonical(t, tc.incoming))

			var oldChanged, newChanged bool
			require.NoError(t, db.QueryRow(
				`SELECT credentials IS DISTINCT FROM $1::jsonb FROM accounts WHERE id = $2`,
				payload, tc.rowID).Scan(&oldChanged))
			require.NoError(t, db.QueryRow(
				`SELECT credentials_hash IS DISTINCT FROM $1 FROM accounts WHERE id = $2`,
				hash, tc.rowID).Scan(&newChanged))

			require.Equal(t, tc.want, oldChanged, "旧谓词")
			require.Equal(t, tc.want, newChanged, "新谓词")
			require.Equal(t, oldChanged, newChanged, "新旧必须一致")
		})
	}
}

// TestCredentialDiffLeaf_APIKeyChangedIsDistinct
//
// CASE 里判断"api_key 变了没有"：决定要不要清受管用量快照。
// 旧：`credentials -> 'api_key' IS DISTINCT FROM $1::jsonb -> 'api_key'`（jsonb 层）
// 新：`credentials_api_key_hash IS DISTINCT FROM $2`
//
// 注意语义差别与它为什么不影响结果：旧写法比较 jsonb（缺失为 SQL NULL），
// 新写法比较指纹（缺失为 NULL）；`NULL IS DISTINCT FROM NULL` 在两边都是 false，
// 所以"两边都缺"仍然一致。
func TestCredentialDiffLeaf_APIKeyChangedIsDistinct(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	cases := []struct {
		name       string
		rowID      int64
		incoming   map[string]any
		wantChange bool
	}{
		{"相同 key", 1, map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com"}, false},
		{"换了 key", 1, map[string]any{"api_key": "sk-a2", "base_url": "https://ollama.com"}, true},
		{"两边都没有 key", 2, map[string]any{"base_url": "https://ollama.com"}, false},
		{"旧有新无", 1, map[string]any{"base_url": "https://ollama.com"}, true},
		{"旧无新有", 2, map[string]any{"api_key": "sk-new", "base_url": "https://ollama.com"}, true},
		{"空串对空串", 9, map[string]any{"api_key": "", "base_url": "https://ollama.com"}, false},
		{"空串换真 key", 9, map[string]any{"api_key": "sk-real", "base_url": "https://ollama.com"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSON(t, tc.incoming)
			incomingHash := credentialAPIKeyHash(tc.incoming)
			var newArg any
			if incomingHash != nil {
				newArg = *incomingHash
			}

			var oldChanged, newChanged bool
			require.NoError(t, db.QueryRow(
				`SELECT credentials -> 'api_key' IS DISTINCT FROM $1::jsonb -> 'api_key' FROM accounts WHERE id = $2`,
				payload, tc.rowID).Scan(&oldChanged))
			require.NoError(t, db.QueryRow(
				`SELECT credentials_api_key_hash IS DISTINCT FROM $1 FROM accounts WHERE id = $2`,
				newArg, tc.rowID).Scan(&newChanged))

			require.Equal(t, tc.wantChange, oldChanged, "旧谓词")
			require.Equal(t, tc.wantChange, newChanged, "新谓词")
			require.Equal(t, oldChanged, newChanged, "新旧必须一致")
		})
	}
}

// TestCredentialDiffLeaf_OllamaBaseURLGroupChanged
//
// Ollama 清理分支的组身份谓词：
// 旧 `NOT (ollamaMatch(credentials) AND ollamaMatch($1::jsonb))`
// 新 `NOT (ollamaMatch(credentials_meta) AND ollamaMatch($2::jsonb))`
// 只要两边都不再匹配 ollama.com 基址（或旧行本来就不匹配），组身份就算变了。
func TestCredentialDiffLeaf_OllamaBaseURLGroupChanged(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	oldExpr := `NOT (` +
		ollamaCloudBaseURLMatchesSQL("credentials ->> 'base_url'") +
		` AND ` + ollamaCloudBaseURLMatchesSQL("$1::jsonb ->> 'base_url'") + `)`
	newExpr := `NOT (` +
		ollamaCloudBaseURLMatchesSQL("credentials_meta ->> 'base_url'") +
		` AND ` + ollamaCloudBaseURLMatchesSQL("$2::jsonb ->> 'base_url'") + `)`

	cases := []struct {
		name     string
		rowID    int64
		incoming map[string]any
	}{
		{"两边都是 ollama 基址", 1, map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com"}},
		{"旧是、新不是", 1, map[string]any{"api_key": "sk-a", "base_url": "https://example.com"}},
		{"旧不是、新是", 3, map[string]any{"api_key": "sk-c", "base_url": "https://ollama.com"}},
		{"两边都不是", 3, map[string]any{"api_key": "sk-c", "base_url": "https://example.com"}},
		{"新侧缺 base_url", 1, map[string]any{"api_key": "sk-a"}},
		{"旧侧缺 base_url", 8, map[string]any{"api_key": "sk-h", "base_url": "https://ollama.com"}},
		{"大小写与空白", 1, map[string]any{"api_key": "sk-a", "base_url": "  https://WWW.OLLAMA.com:443/v1  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSON(t, tc.incoming)
			incomingMeta := mustJSON(t, credentialMetaSubset(tc.incoming))

			// 这个叶子是 **NULL 传播**的：任一侧 base_url 缺失，NOT(... AND NULL)
			// 结果是 NULL 而不是 false。生产靠 WHERE 把 NULL 当假来工作，
			// 所以这里必须比对两件事：结果是否为真、是否为 NULL。
			// 只比 COALESCE(...,false) 会掩盖"NULL 变成 false"这类漂移。
			var oldIsTrue, oldIsNull, newIsTrue, newIsNull bool
			// 旧表达式里 $1 = 入参凭据（与生产一致），所以 id 只能是 $2；
			// 新表达式里 $2 = 入参 meta，id 用 $1。
			require.NoError(t, db.QueryRow(
				`SELECT (`+oldExpr+`) IS TRUE, (`+oldExpr+`) IS NULL FROM accounts WHERE id = $2`,
				payload, tc.rowID).Scan(&oldIsTrue, &oldIsNull))
			require.NoError(t, db.QueryRow(
				`SELECT (`+newExpr+`) IS TRUE, (`+newExpr+`) IS NULL FROM accounts WHERE id = $1`,
				tc.rowID, incomingMeta).Scan(&newIsTrue, &newIsNull))

			require.Equal(t, oldIsTrue, newIsTrue, "真值必须一致")
			require.Equal(t, oldIsNull, newIsNull, "NULL 性必须一致（NULL 与 false 在 WHERE 里等价，但语义不同）")
		})
	}
}

// TestCredentialDiffLeaf_OpenCodeAccountModeIsZen
//
// OpenCode 清理分支里判断"目标身份还是不是 Go 订阅"：
// 旧 `COALESCE(btrim($1::jsonb ->> 'account_mode') <> 'zen', true)`
// 新 `COALESCE(btrim($2::jsonb ->> 'account_mode') <> 'zen', true)`
//
// COALESCE 把"缺字段"归一为 true（默认 Go 订阅）——这层默认逻辑不能因为
// 换了列而改变，否则 opencode_go 账号会在 account_mode 缺失时被判成 Zen。
func TestCredentialDiffLeaf_OpenCodeAccountModeIsZen(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	oldExpr := `COALESCE(btrim($1::jsonb ->> 'account_mode') <> 'zen', true)`
	// 右侧没有 FROM 子句，参数必须从 $1 起编（写 $2 会让 $1 类型不可判定）。
	newExpr := `COALESCE(btrim($1::jsonb ->> 'account_mode') <> 'zen', true)`

	variants := []map[string]any{
		{"api_key": "sk-x"},
		{"api_key": "sk-x", "account_mode": "zen"},
		{"api_key": "sk-x", "account_mode": "go"},
		{"api_key": "sk-x", "account_mode": "  zen  "},
		{"api_key": "sk-x", "account_mode": "ZEN"},
		{"api_key": "sk-x", "account_mode": nil},
	}
	for i, incoming := range variants {
		payload := mustJSON(t, incoming)
		incomingMeta := mustJSON(t, credentialMetaSubset(incoming))
		var oldV, newV bool
		require.NoError(t, db.QueryRow(`SELECT `+oldExpr, payload).Scan(&oldV))
		require.NoError(t, db.QueryRow(`SELECT `+newExpr, incomingMeta).Scan(&newV))
		require.Equal(t, oldV, newV, "variant %d 新旧必须一致", i)
	}
}

// TestCredentialDiffLeaf_NullHashMakesEverythingLookChanged 是**为什么回填必须
// 早于阶段二**的实证。
//
// 未回填的行 hash 为 NULL，而 `NULL IS DISTINCT FROM x` 恒为真：
// 于是"整份凭据没变"也会被判成"变了"→ 命中清理分支 → 受管快照被误清。
// 这条测试不修任何东西，它只是把这个陷阱变成可执行的证据。
func TestCredentialDiffLeaf_NullHashMakesEverythingLookChanged(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	// 人为把 id=1 的 hash 清掉，模拟"未回填"
	_, err := db.Exec(`UPDATE accounts SET credentials_hash = NULL WHERE id = 1`)
	require.NoError(t, err)

	sameDoc := map[string]any{"api_key": "sk-a", "base_url": "https://ollama.com"}
	payload := mustJSON(t, sameDoc)
	hash := sha256Hex(mustCanonical(t, sameDoc))

	var oldChanged, newChanged bool
	require.NoError(t, db.QueryRow(
		`SELECT credentials IS DISTINCT FROM $1::jsonb FROM accounts WHERE id = 1`, payload).Scan(&oldChanged))
	require.NoError(t, db.QueryRow(
		`SELECT credentials_hash IS DISTINCT FROM $1 FROM accounts WHERE id = 1`, hash).Scan(&newChanged))

	require.False(t, oldChanged, "旧谓词：文档其实没变")
	require.True(t, newChanged, "新谓词在 hash 为 NULL 时必然判成'已变更'——这就是必须先回填的原因")
}

// TestCredentialDiffLeaf_RefreshTokenPresenceViaMeta（站点 A 方案 ①）
//
// 旧谓词读明文的 refresh_token，新谓词读 credentials_meta 里的派生事实。
// 这条在真 PG 上比对"两边选中的 id 集合"，是"语义没变"的证据；
// "改了"的证据是 TestListOAuthRefreshCandidatePage_UsesMetaDerivedFlag（文本断言）。
func TestCredentialDiffLeaf_RefreshTokenPresenceViaMeta(t *testing.T) {
	db := credentialDiffDB(t)
	seedCredentialDiffRows(t, db)

	// 共享 fixture 里没有带 refresh_token 的行——不补一行的话"两边都为空"也会
	// 让 Equal 通过，等于什么都没验。这里插两条专用行（有 token / token 为空串）。
	repo := newAccountRepositoryWithSQL(nil, nil, nil)
	for id, creds := range map[int64]map[string]any{
		901: {"refresh_token": "rt-1", "access_token": "at-1"},
		902: {"refresh_token": "", "access_token": "at-2"},
	} {
		columns, err := repo.credentialCryptoColumnsFor(creds)
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO accounts (id, platform, type, credentials, credentials_meta, credentials_hash, credentials_api_key_hash)
			VALUES ($1, 'grok', 'oauth', $2::jsonb, $3::jsonb, $4, $5)`,
			id, mustJSON(t, creds), mustJSON(t, columns.Meta), *columns.Hash, columns.APIKeyHash)
		require.NoError(t, err, "seed id=%d", id)
	}

	oldIDs := selectIDs(t, db,
		`credentials ? 'refresh_token' AND btrim(credentials->>'refresh_token') <> ''`)
	newIDs := selectIDs(t, db,
		`credentials_meta @> '{"has_refresh_token": true}'`)

	require.Equal(t, []int64{901}, oldIDs, "旧谓词：只有 refresh_token 非空的那条")
	require.Equal(t, oldIDs, newIDs, "新旧谓词必须选同一批行")
}
