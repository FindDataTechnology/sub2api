package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// wire 接线的结构守卫：**加密器必须真的被传进 account repository**。
//
// 为什么只能这么测（2026-11-11 全站 503 事故）：`cryptor` 是接口，`nil` 也是一个合法实参，
// 所以"必填参数"只挡住"忘了传"，挡不住"传了 nil"。而真正出事故的那次，代码本身没错 ——
// 是 `wire_gen.go` 里这一路用了非 Crypto 构造器（等价于传 nil），
// 而所有单测都自己显式传了 cryptor，于是永远测不出来。
// 回填之后每个账号都有密文，这一路读第一行就硬失败 → 调度快照整批重建失败 → 全站 503。
//
// 判据：wire 生成的调用里，凡构造 account repository 的，实参必须含 `secretEncryptor`。
func TestWire_AccountRepositoryReceivesEncryptor(t *testing.T) {
	raw, err := os.ReadFile("wire_gen.go")
	if err != nil {
		t.Fatalf("读 wire_gen.go: %v", err)
	}
	src := string(raw)

	cases := []struct {
		fn   string
		want string
	}{
		{"repository.NewAccountRepository(", "secretEncryptor"},
		{"repository.NewAdminAccountRepository(", "secretEncryptor"},
	}
	// 抓调用整体（直到右括号），避免只匹配到函数名。
	callRE := func(fn string) *regexp.Regexp {
		return regexp.MustCompile(regexp.QuoteMeta(fn) + `[^)]*\)`)
	}
	for _, c := range cases {
		re := callRE(c.fn)
		calls := re.FindAllString(src, -1)
		if len(calls) == 0 {
			t.Fatalf("wire_gen.go 里找不到 %s 调用 —— 生成文件变了？本守卫需要跟着更新", c.fn)
		}
		for _, call := range calls {
			if !strings.Contains(call, c.want) {
				t.Errorf("%s\n  没有传 %s：回填后的账号有密文，这一路会硬失败（生产事故形态）",
					strings.Join(strings.Fields(call), " "), c.want)
			}
		}
	}
}
