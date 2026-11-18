package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCredentialAtRestColumnsMigration 校验 249 迁移的形状。
//
// 这个迁移是凭据落盘加密的**阶段一**（加列 + 双写 + 边界解密的前提），
// 见 openspec/changes/harden-sub2api-credential-storage/。
//
// 断言刻意写成"列名 + 约束 + 索引谓词"而不是只数行数——只断言文件非空
// 的话，把某个 ADD COLUMN 删掉测试照样绿。
func TestCredentialAtRestColumnsMigration(t *testing.T) {
	content, err := FS.ReadFile("249_credential_at_rest_columns.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")

	// accounts：四个新列必须都在，且都可空/带默认（否则既有 49 行无法回滚）
	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_meta JSONB NOT NULL DEFAULT '{}'::jsonb")
	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_encrypted TEXT")
	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_hash VARCHAR(64)")
	require.Contains(t, sql, "ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_api_key_hash VARCHAR(64)")

	// 指纹列必须有索引：它是用量车道替代 `credentials ->> 'api_key'` 的唯一入口。
	// 用部分索引（WHERE ... IS NOT NULL），避免迁移完成前全 NULL 行占位。
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_accounts_credentials_api_key_hash ON accounts (credentials_api_key_hash) WHERE credentials_api_key_hash IS NOT NULL")

	// api_keys：key_hash 唯一（Postgres 唯一索引允许多个 NULL，迁移完成前不冲突）
	require.Contains(t, sql, "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_hash VARCHAR(64)")
	require.Contains(t, sql, "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_encrypted TEXT")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys (key_hash) WHERE key_hash IS NOT NULL")

	// 本阶段 MUST NOT 触碰明文列——它是回滚的凭据本体。
	// 一旦这条断言变红，说明有人把 drop 明文列提前了，回滚路径就没了。
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "ALTER TABLE accounts ALTER COLUMN credentials")
	require.NotContains(t, sql, "ALTER TABLE api_keys ALTER COLUMN key")
}

// TestCredentialAtRestMigrationDoesNotTouchPlaintextColumns 单独一条，避免上面
// 那条被将来的"加列时顺手 reformat"误伤而连带失效。
func TestCredentialAtRestMigrationDoesNotTouchPlaintextColumns(t *testing.T) {
	content, err := FS.ReadFile("249_credential_at_rest_columns.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "UPDATE accounts SET credentials")
	require.NotContains(t, sql, "UPDATE api_keys SET key")
}
