package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCredentialsMetaHasRefreshTokenMigration 校验 250 迁移的形状。
//
// 这是**纯数据**迁移（无 DDL）：把"有非空 refresh_token"写成 credentials_meta 里的
// 派生事实，供阶段四 drop 明文列之后的 OAuth 刷新候选筛选使用。
func TestCredentialsMetaHasRefreshTokenMigration(t *testing.T) {
	content, err := FS.ReadFile("250_credentials_meta_has_refresh_token.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	require.Contains(t, sql,
		"UPDATE accounts SET credentials_meta = credentials_meta || jsonb_build_object( 'has_refresh_token',")
	// 派生表达式必须与代码里的 credentialHasRefreshToken 同语义（BTRIM 后非空）
	require.Contains(t, sql,
		`COALESCE(NULLIF(BTRIM(credentials ->> 'refresh_token'), '') IS NOT NULL, false)`)
	// 幂等：只补还没带该 key 的行
	require.Contains(t, sql, "WHERE NOT (credentials_meta ? 'has_refresh_token')")

	// 本迁移 MUST NOT 引入 DDL，也 MUST NOT 触碰明文列以外的东西
	require.NotContains(t, sql, "ALTER TABLE")
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "DELETE FROM")
}
