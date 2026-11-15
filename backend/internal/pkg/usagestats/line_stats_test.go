package usagestats

import (
	"reflect"
	"testing"
)

// 这组测钉五件事，每条都对应一种会静默算错的情形：
//  1. 别名命中才归并；没命中一律落 unknown（不许猜）。
//  2. unknown 的原始段必须看得见——否则「报表里少了一坨」无法解释。
//  3. 同一份数据两次聚合顺序一致（可 diff），且金额/占比按声明精度四舍五入。
//  4. **声明前缀优先于首段，且只影响它自己那一族**——同 seg 下未声明的键必须原样留在原线。
//  5. 前缀归的键与前缀规则原文一起落到 `Prefixes`，读面能判断「这个数被改写过」。
//
// 仓库层给的是**完整 key 名**（LineStatRow.KeyName），不是首段：前缀规则要靠完整名字才生效。
//
// 历史注记：旧版 `TestAggregateLinesKeepsUnknownSegsVisible` 拿 `特价`/`openrouter` 当未知样本，
// 而 cc5c60e06（2026-11-15）已把这两族登记成「自有平台」却没同步改测试 ⇒ main 上那条用例
// 在本分支之前的 HEAD 就已经是红的。未知样本换成真正未登记的 `fd-*` / 纯数字段。

func TestAggregateLinesMergesAliases(t *testing.T) {
	rows := []LineStatRow{
		{KeyName: "lex-a", Requests: 100, ActualCost: 10, ListCost: 11},
		{KeyName: "lexkg-chat-g1", Requests: 50, ActualCost: 5, ListCost: 5.5},
		{KeyName: "wanxing-a", Requests: 20, ActualCost: 2, ListCost: 2},
	}

	got := AggregateLines(rows)

	if len(got) != 2 {
		t.Fatalf("期望归并成 2 条线，实际 %d 条：%+v", len(got), got)
	}
	// 识律 lex 应含 lex + lexkg
	if got[0].Line != "识律 lex" {
		t.Fatalf("实扣最高的线应为识律 lex，实际 %q", got[0].Line)
	}
	if got[0].Requests != 150 {
		t.Errorf("识律请求数应为 150（100+50），实际 %d", got[0].Requests)
	}
	if want := []string{"lex", "lexkg"}; !reflect.DeepEqual(got[0].Segs, want) {
		t.Errorf("识律的 Segs 应为 %v，实际 %v", want, got[0].Segs)
	}
	// 占比：10+5=15 占 17 的 88.2%
	if got[0].SharePct != 88.2 {
		t.Errorf("占比应为 88.2%%，实际 %v", got[0].SharePct)
	}
}

func TestAggregateLinesKeepsUnknownSegsVisible(t *testing.T) {
	rows := []LineStatRow{
		{KeyName: "lex-a", Requests: 1, ActualCost: 1},
		// 这两个是**真的**未登记段：`特价`/`openrouter` 已于 2026-11-15 归入「自有平台」，
		// 再用它们当未知样本，这条用例就会在归并表更新那天变成「其实测的是别的线」。
		{KeyName: "fd-memory", Requests: 2, ActualCost: 2},
		{KeyName: "413621549-chat", Requests: 3, ActualCost: 3},
	}

	got := AggregateLines(rows)
	if got[0].Line != UnknownLine {
		t.Fatalf("实扣最高应为 unknown（3+2 > 1），实际 %q", got[0].Line)
	}
	if got[0].Segs != nil {
		t.Errorf("unknown 不该填 Segs（语义上它是 UnknownSegs），实际 %v", got[0].Segs)
	}
	want := []string{"413621549", "fd"}
	if !reflect.DeepEqual(got[0].UnknownSegs, want) {
		t.Errorf("无法归因的原始段必须单列且可见：期望 %v，实际 %v", want, got[0].UnknownSegs)
	}
}

func TestAggregateLinesIsDeterministic(t *testing.T) {
	rows := []LineStatRow{
		{KeyName: "wanxing-a", Requests: 1, ActualCost: 1},
		{KeyName: "lex-a", Requests: 1, ActualCost: 1},
		{KeyName: "law-a", Requests: 1, ActualCost: 1},
	}
	first := AggregateLines(rows)
	for i := 0; i < 20; i++ {
		again := AggregateLines(rows)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("同输入两次聚合结果不一致，报表不可 diff：%+v vs %+v", first, again)
		}
	}
	// 同额按线名升序
	for i := 1; i < len(first); i++ {
		if first[i-1].Line > first[i].Line {
			t.Fatalf("同额时线名应升序，实际 %q 排在 %q 前", first[i-1].Line, first[i].Line)
		}
	}
}

func TestAggregateLinesEmptyInputIsEmpty(t *testing.T) {
	if got := AggregateLines(nil); len(got) != 0 {
		t.Fatalf("无数据应返回空切片，实际 %+v", got)
	}
	// 无数据时占比不能是 NaN（除零）
	got := AggregateLines([]LineStatRow{{KeyName: "lex-a", Requests: 0, ActualCost: 0}})
	if got[0].SharePct != 0 {
		t.Fatalf("总额为 0 时占比应为 0，实际 %v", got[0].SharePct)
	}
}

func TestLineAliasForIsGenerated(t *testing.T) {
	// 生成件的存在性检查：真源改了却没跑生成器，这里会立刻失败。
	if line, ok := LineAliasFor("lexkg"); !ok || line != "识律 lex" {
		t.Fatalf("lexkg 应归并到识律 lex，实际 (%q,%v)", line, ok)
	}
	if _, ok := LineAliasFor("这个段没登记"); ok {
		t.Fatal("未登记的段不许被归并——猜测比缺失更坏")
	}
	if LineAliasReason["wx"] == "" {
		t.Fatal("每条别名都必须有归并理由（生成件里缺 reason）")
	}
}

// TestLineAliasForNamePrefixBeatsSegment 是本单的核心回归：
// 同一个首段（wanxing）下，声明了前缀的键改归属、没声明的键**必须原样不动**。
// 少了后半个断言，「前缀规则把整族一起搬走」这种错误会一路绿。
func TestLineAliasForNamePrefixBeatsSegment(t *testing.T) {
	cases := []struct {
		name, wantLine, wantByRule, wantRule string
	}{
		{"wanxing-lex-console-copilot", "识律 lex", ByRulePrefix, "wanxing-lex-"},
		{"wanxing-probe-1791014842023", "非产品流量", ByRulePrefix, "wanxing-probe-"},
		// 同 seg、未声明 ⇒ 仍按首段归萬星（这条就是「不许连带改写」）
		{"wanxing-wire-cluster-macro", "萬星 wanxing", ByRuleSeg, "wanxing"},
		{"wanxing", "萬星 wanxing", ByRuleSeg, "wanxing"},
		{"lex-console-copilot", "识律 lex", ByRuleSeg, "lex"},
		// 未归因时 rule 仍是首段原文——unknown 桶靠它点名（空键名才是空串）
		{"413621549-chat", "", ByRuleNone, "413621549"},
		{"", "", ByRuleNone, ""},
	}
	for _, c := range cases {
		line, why, byRule, rule := LineAliasForName(c.name)
		if line != c.wantLine || byRule != c.wantByRule || rule != c.wantRule {
			t.Errorf("%q: 期望 (%q,%q,%q)，实际 (%q,%q,%q)",
				c.name, c.wantLine, c.wantByRule, c.wantRule, line, byRule, rule)
		}
		if byRule != ByRuleNone && why == "" {
			t.Errorf("%q: 命中规则必须带理由（%s）", c.name, rule)
		}
	}
}

// TestAggregateLinesSeparatesByRule 钉「前缀归的键不能与前缀规则本身的字面重复计入」，
// 并确认两条线各自拿到自己的 Segs / Prefixes。
func TestAggregateLinesSeparatesByRule(t *testing.T) {
	rows := []LineStatRow{
		{KeyName: "wanxing-lex-console-copilot", Requests: 10, ActualCost: 7},
		{KeyName: "wanxing-a", Requests: 5, ActualCost: 3},
	}
	got := AggregateLines(rows)
	if len(got) != 2 {
		t.Fatalf("期望 2 条线（识律 + 萬星），实际 %+v", got)
	}
	byLine := map[string]LineStat{}
	for _, s := range got {
		byLine[s.Line] = s
	}
	lex := byLine["识律 lex"]
	if lex.ActualCost != 7 || !reflect.DeepEqual(lex.Prefixes, []string{"wanxing-lex-"}) {
		t.Errorf("识律应只拿到前缀那 7 元且 Prefixes=[wanxing-lex-]，实际 %+v", lex)
	}
	if lex.Segs != nil {
		t.Errorf("识律这一行全来自前缀，Segs 应为空，实际 %v", lex.Segs)
	}
	wx := byLine["萬星 wanxing"]
	if wx.ActualCost != 3 || !reflect.DeepEqual(wx.Segs, []string{"wanxing"}) || wx.Prefixes != nil {
		t.Errorf("萬星应只拿到未声明那 3 元，实际 %+v", wx)
	}
}

// TestAggregateLinesDetailedSplitsRouteKeys 钉住「路由键的花费不改变归属，但必须单列，
// 且可判读份额要把未归因与路由键都扣掉」——这是控制台那一侧能力不再骗人的全部依据。
func TestAggregateLinesDetailedSplitsRouteKeys(t *testing.T) {
	rows := []LineStatRow{
		{KeyName: "特价-a", Requests: 100, ActualCost: 200},
		{KeyName: "openrouter-b", Requests: 50, ActualCost: 20},
		{KeyName: "lex-a", Requests: 10, ActualCost: 20},
		{KeyName: "413621549-c", Requests: 5, ActualCost: 5},
	}
	agg := AggregateLinesDetailed(rows)

	// 1) 归属不变：路由键仍落在别名表声明的那条线上（可见性变更 ≠ 归因变更）
	byLine := map[string]LineStat{}
	for _, s := range agg.Lines {
		byLine[s.Line] = s
	}
	own := byLine["自有平台"]
	if own.ActualCost != 220 {
		t.Fatalf("自有平台应仍是 220（200+20），实际 %+v", own)
	}
	// 2) 但那一行必须自己说明它含哪些路由键
	if !reflect.DeepEqual(own.RouteKeys, []string{"openrouter", "特价"}) {
		t.Errorf("自有平台应标出 route_keys=[openrouter 特价]，实际 %v", own.RouteKeys)
	}

	// 3) 路由键单列，按实扣降序
	if len(agg.RouteKeys) != 2 {
		t.Fatalf("期望 2 条路由键读数，实际 %+v", agg.RouteKeys)
	}
	if agg.RouteKeys[0].Seg != "特价" || agg.RouteKeys[0].ActualCost != 200 {
		t.Errorf("路由键首项应是 特价 200，实际 %+v", agg.RouteKeys[0])
	}
	if agg.RouteKeyUSD != 220 || agg.RouteKeyShare != 0.8980 {
		t.Errorf("路由键合计/份额应为 220 / 0.8980，实际 %v / %v", agg.RouteKeyUSD, agg.RouteKeyShare)
	}

	// 4) 可判读 = 1 − 未归因(5/245) − 路由键(220/245)
	if agg.ProductReadableShare == nil {
		t.Fatal("窗口内有花费时 product_readable_share 不应为 nil")
	}
	if got := *agg.ProductReadableShare; got != 0.0816 {
		t.Errorf("可判读份额应为 0.0816（1 − 5/245 − 220/245），实际 %v", got)
	}
	if agg.TotalUSD != 245 {
		t.Errorf("分母应为 245，实际 %v", agg.TotalUSD)
	}
}

// TestAggregateLinesDetailedEmptyWindowIsNotFullCoverage 钉住「空窗口不是 100% 可判读」。
// nil 与 1.0 在这一栏含义完全相反：前者是「没有可判读的东西」，后者是「全都读得懂」。
func TestAggregateLinesDetailedEmptyWindowIsNotFullCoverage(t *testing.T) {
	agg := AggregateLinesDetailed(nil)
	if agg.ProductReadableShare != nil {
		t.Fatalf("空窗口的 product_readable_share 必须是 nil，实际 %v", *agg.ProductReadableShare)
	}
	if len(agg.RouteKeys) != 0 {
		t.Errorf("没有输入时不应伪造路由键读数，实际 %+v", agg.RouteKeys)
	}

	// 零花费但有条目：同样不能报 100%
	agg = AggregateLinesDetailed([]LineStatRow{{KeyName: "lex-a", Requests: 3}})
	if agg.ProductReadableShare != nil {
		t.Fatalf("实扣为 0 时 product_readable_share 必须是 nil，实际 %v", *agg.ProductReadableShare)
	}
}

// TestAggregateLinesDetailedRouteKeysNeedDeclaredSeg 钉住「路由键只在按首段命中时成立」：
// 前缀规则是更具体的受益方声明，不能被路由键覆盖（同一条纪律：归因只能有一处生效）。
func TestAggregateLinesDetailedRouteKeysNeedDeclaredSeg(t *testing.T) {
	// wanxing-lex-console-copilot 由前缀规则改归识律；它不是路由键，也不该进 route_keys。
	agg := AggregateLinesDetailed([]LineStatRow{
		{KeyName: "wanxing-lex-console-copilot", Requests: 1, ActualCost: 5},
	})
	for _, s := range agg.Lines {
		if len(s.RouteKeys) != 0 {
			t.Errorf("前缀归因的行不该带 route_keys，实际 %+v", s)
		}
	}
	if len(agg.RouteKeys) != 0 || agg.RouteKeyUSD != 0 {
		t.Errorf("前缀归因不应产出路由键读数，实际 %+v / %v", agg.RouteKeys, agg.RouteKeyUSD)
	}
	if agg.ProductReadableShare == nil || *agg.ProductReadableShare != 1 {
		t.Errorf("全部可判读时应为 1，实际 %v", agg.ProductReadableShare)
	}
}
