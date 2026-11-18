package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 251 把 api_keys.key 下线后，cache 失效触发器仍在引用 OLD.key/NEW.key，
// 导致 api_keys 上任何 UPDATE/DELETE 抛 'record "old" has no field "key"'，
// 结算事务整笔回滚（quota_used / last_used_at 永不累加，DELETE key 500）。
// 本测试守住修复不被回退。
func TestFixAPIKeyCacheInvalidationTriggerMigration(t *testing.T) {
	content, err := FS.ReadFile("253_fix_api_key_cache_invalidation_trigger.sql")
	require.NoError(t, err)

	// 负向断言只看语句本体：文件头部的注释里本来就写着 quota_used / last_used_at
	// 这些词（用来说明"为什么不监听它们"），把注释算进来会误红。
	var stmt strings.Builder
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		_, _ = stmt.WriteString(line)
		_, _ = stmt.WriteString("\n")
	}

	sql := strings.Join(strings.Fields(string(content)), " ")
	body := strings.Join(strings.Fields(stmt.String()), " ")

	// 必须重建同一个函数（触发器本身不用动）
	require.Contains(t, sql, "CREATE OR REPLACE FUNCTION public.enqueue_api_key_auth_cache_invalidation()")
	require.Contains(t, sql, "RETURNS trigger")

	// 不能再碰已经 drop 掉的明文列
	require.NotContains(t, body, "OLD.key ")
	require.NotContains(t, body, "NEW.key ")
	require.NotContains(t, body, "OLD.key,")
	require.NotContains(t, body, "NEW.key,")

	// 监听列切到派生列，且保持原有集合
	require.Contains(t, body, "OLD.key_hash IS DISTINCT FROM NEW.key_hash")
	require.Contains(t, body, "OLD.status IS DISTINCT FROM NEW.status")
	require.Contains(t, body, "OLD.deleted_at IS DISTINCT FROM NEW.deleted_at")
	require.Contains(t, body, "OLD.user_id IS DISTINCT FROM NEW.user_id")
	require.Contains(t, body, "OLD.group_id IS DISTINCT FROM NEW.group_id")
	require.Contains(t, body, "OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist")
	require.Contains(t, body, "OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist")
	require.Contains(t, body, "OLD.expires_at IS DISTINCT FROM NEW.expires_at")

	// 直接按 key_hash 写 outbox：不能再走会二次 sha256 的 enqueue_auth_cache_invalidation()
	require.Contains(t, body, "INSERT INTO auth_cache_invalidation_outbox (cache_key) VALUES (OLD.key_hash)")
	require.Contains(t, body, "INSERT INTO auth_cache_invalidation_outbox (cache_key) VALUES (NEW.key_hash)")
	require.NotContains(t, body, "PERFORM enqueue_auth_cache_invalidation(")

	// 高频计数列不该纳入失效（否则每次计费都产生一条无用投递）
	require.NotContains(t, body, "usage_1d")
	require.NotContains(t, body, "quota_used")
	require.NotContains(t, body, "last_used_at")

	// DELETE 分支同样要能工作
	require.Contains(t, body, "IF TG_OP = 'DELETE' THEN")
}
