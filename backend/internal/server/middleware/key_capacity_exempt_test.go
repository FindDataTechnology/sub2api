//go:build unit

package middleware

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 任务车道豁免秤：只豁免提交/查询路径本身，执行路径照常占席位。
//
// 为什么用「精确路径 + 边界前缀」而不是 strings.Contains：
// 后者会让 /v1/ciyuan/tasks-evil 这类同前缀路径也绕过容量闸 —— 一个
// 手滑就能把整个席位契约作废。
func TestKeyCapacityExemptPath(t *testing.T) {
	require.True(t, KeyCapacityExemptPath("/v1/ciyuan/tasks"))
	require.True(t, KeyCapacityExemptPath("/v1/ciyuan/tasks/123"))

	require.False(t, KeyCapacityExemptPath("/v1/chat/completions"), "真正的上游调用必须占席位")
	require.False(t, KeyCapacityExemptPath("/v1/ciyuan/tasks-evil"), "前缀相同但不是子路径，不得豁免")
	require.False(t, KeyCapacityExemptPath("/v1/ciyuan/capabilities"), "能力快照不豁免（与是否占席位无关，保持最小豁免面）")
	require.False(t, KeyCapacityExemptPath(""))
	require.False(t, KeyCapacityExemptPath("/v1/ciyuan/tasks/../../etc/passwd/x"), "路径穿越形态不豁免")
}
