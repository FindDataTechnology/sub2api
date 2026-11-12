//go:build unit

package lanes

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 车道前提的启动判据（add-ciyuan-task-lane，2026-11-11 装配事故后补）。
//
// 事故形态：`service.SetLanesTaskStore(...)` 是包级 setter，wire 重新生成时不认识它，
// 被静默覆盖 —— 编译过、单测绿、pod 健康，只有真打 `/v1/ciyuan/tasks` 才看到 503，
// 而日志里"没接线"和"没流量"完全同形。所以这条判据必须可断言、不能只靠人读日志。

func TestLanePrereqStatusSchedulerRequiresStore(t *testing.T) {
	// 缺 store：调度器每个 tick 都会在最前面 return，等于空转 —— 必须报 false。
	line := lanePrereqStatus(false, true, true)
	require.Contains(t, line, "store_injected=false")
	require.Contains(t, line, "engine_wired=true")
	require.Contains(t, line, "scheduler_will_run=false",
		"store 缺失时调度器空转，不得报 will_run=true（那正是本期事故的假健康）")
	require.Contains(t, line, "SetLanesTaskStore", "日志要直接点名该补哪个调用")

	// 三件齐：全 true。
	full := lanePrereqStatus(true, true, true)
	require.Contains(t, full, "store_injected=true")
	require.Contains(t, full, "scheduler_will_run=true")

	// runner 缺失：即便 store 在，也没人执行 —— will_run 必须 false。
	require.Contains(t, lanePrereqStatus(true, true, false), "scheduler_will_run=false")

	// engine 未注入只影响重放（端点会 503），不影响"store 在不在"这件事的表述。
	noEngine := lanePrereqStatus(true, false, true)
	require.Contains(t, noEngine, "engine_wired=false")
	require.Contains(t, noEngine, "store_injected=true")

	// 判据顺序固定：改字段顺序会让线上 grep 失效。
	require.Less(t,
		strings.Index(full, "store_injected="), strings.Index(full, "scheduler_will_run="),
		"store_injected 必须排在最前 —— 排障时先看它")
}
