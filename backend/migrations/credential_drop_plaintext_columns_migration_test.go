package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCredentialDropPlaintextColumnsMigration 校验 251 迁移的形状。
//
// 这是阶段 5.2 的唯一一次不可逆操作，所以断言分成三组，缺一组都会让人放松警惕：
//  1. **两列都必须被 drop**（只 drop 一列 = 另一半还在漏明文，5.3 判据会红）；
//  2. **派生列一律不许动**（drop 顺手改派生列 = 静默丢凭据）；
//  3. **幂等**（IF EXISTS）：迁移链条可重跑，drop 之后重复执行不报错。
func TestCredentialDropPlaintextColumnsMigration(t *testing.T) {
	content, err := FS.ReadFile("251_credential_drop_plaintext_columns.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql, "ALTER TABLE accounts DROP COLUMN IF EXISTS credentials;")
	require.Contains(t, sql, "ALTER TABLE api_keys DROP COLUMN IF EXISTS key;")

	// 派生列必须全部保留：它们是 drop 之后唯一的凭据载体。
	for _, derived := range []string{
		"credentials_encrypted", "credentials_meta", "credentials_hash",
		"credentials_api_key_hash", "key_encrypted", "key_hash",
	} {
		require.NotContains(t, sql, "DROP COLUMN IF EXISTS "+derived,
			"派生列 %s 不能被 drop —— 它是 drop 之后唯一的凭据载体", derived)
	}

	// 只允许这两处 DROP COLUMN：多出来的会改到别的结构（这类"顺手"最难在 review 里看见）。
	require.Equal(t, 2, strings.Count(sql, "DROP COLUMN"),
		"只允许 drop 这两列")

	// 死表删除不属于本迁移：三张 _acct* 只存在于生产，且删除前必须在 release
	// 窗口里逐表 pg_dump 留档（/opt/backups/sub2api/retired-tables/）。
	require.NotContains(t, sql, "DROP TABLE")
}
