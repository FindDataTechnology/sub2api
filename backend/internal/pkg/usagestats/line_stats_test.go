package usagestats

import (
	"reflect"
	"testing"
)

// 这组测钉三件事，每条都对应一种会静默算错的情形：
//  1. 别名命中才归并；没命中一律落 unknown（不许猜）。
//  2. unknown 的原始段必须看得见——否则「报表里少了一坨」无法解释。
//  3. 同一份数据两次聚合顺序一致（可 diff），且金额/占比按声明精度四舍五入。

func TestAggregateLinesMergesAliases(t *testing.T) {
	rows := []LineStatRow{
		{Seg: "lex", Requests: 100, ActualCost: 10, ListCost: 11},
		{Seg: "lexkg", Requests: 50, ActualCost: 5, ListCost: 5.5},
		{Seg: "wanxing", Requests: 20, ActualCost: 2, ListCost: 2},
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
		{Seg: "lex", Requests: 1, ActualCost: 1},
		{Seg: "特价", Requests: 2, ActualCost: 2},
		{Seg: "openrouter", Requests: 3, ActualCost: 3},
	}

	got := AggregateLines(rows)
	if got[0].Line != UnknownLine {
		t.Fatalf("实扣最高应为 unknown（3+2 > 1），实际 %q", got[0].Line)
	}
	if got[0].Segs != nil {
		t.Errorf("unknown 不该填 Segs（语义上它是 UnknownSegs），实际 %v", got[0].Segs)
	}
	want := []string{"openrouter", "特价"}
	if !reflect.DeepEqual(got[0].UnknownSegs, want) {
		t.Errorf("无法归因的原始段必须单列且可见：期望 %v，实际 %v", want, got[0].UnknownSegs)
	}
}

func TestAggregateLinesIsDeterministic(t *testing.T) {
	rows := []LineStatRow{
		{Seg: "wanxing", Requests: 1, ActualCost: 1},
		{Seg: "lex", Requests: 1, ActualCost: 1},
		{Seg: "law", Requests: 1, ActualCost: 1},
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
	got := AggregateLines([]LineStatRow{{Seg: "lex", Requests: 0, ActualCost: 0}})
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
