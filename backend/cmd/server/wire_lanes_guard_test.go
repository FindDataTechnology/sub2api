package main

import (
	"os"
	"strings"
	"testing"
)

// wire 接线的结构守卫（二）：**任务车道的 store 必须真的被注入**。
//
// 同类事故的第二个受害者（2026-11-11，与 wire_encryptor_guard_test.go 同一批改动）：
// Release A 为了让 secretEncryptor 排在 accountRepository 之前，在分支上**重新生成**了
// wire。wire 只认识构造依赖，而 `service.SetLanesTaskStore(...)` 是包级 setter
// ——它不是依赖，于是重新生成时被静默覆盖掉。
//
// 后果不是"少个优化"，是整条车道哑火：GetLanesTaskStore() 返回 nil，
// task_api 直接 taskUnavailable() → POST/GET /v1/ciyuan/tasks 恒 503 lane_unavailable，
// 调度器也永远不启动。而编译器不报、unit 用例不红（没有任何用例断言生产装配里有这行），
// 线上只看得到 503 —— 所以只能拿生成文件本身当判据。
//
// 注意：判据是「装配层有调用」，不是「函数被定义」。
// service/lanes_task_store.go 里的定义与 dbtest 里的自注入都不能算数。
func TestWire_InjectsLanesTaskStore(t *testing.T) {
	raw, err := os.ReadFile("wire_gen.go")
	if err != nil {
		t.Fatalf("读 wire_gen.go: %v", err)
	}
	src := string(raw)

	const want = "service.SetLanesTaskStore(service.NewLanesTaskStore(db))"
	if !strings.Contains(src, want) {
		t.Fatalf("wire_gen.go 里没有 %s —— 任务车道在线上会恒 503 lane_unavailable。\n"+
			"这不是依赖，wire 重新生成不会替你带回来，必须手工补在 db 之后。", want)
	}
}
