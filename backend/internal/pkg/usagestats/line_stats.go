package usagestats

import "sort"

// ── 词元「按线」聚合（ciyuan-consumer-surface 4.3）────────────────────────────
//
// 归并真源是 tools/line-aliases.json（生成到 line_aliases_gen.go），不是本文件。
// 本文件只做两件事：把「key 名首段」映射成线，并把无法归因的段**单列**。
//
// 三条口径纪律（与 tools/attribution-by-line.sql 同一套，改一处要同时改另一处）：
//  1. 线身份 = key 名首段（第一个 '-' 之前）。
//  2. 归一只认别名表；没命中的落 unknown，并保留原始段名。
//  3. unknown 的原始段必须能看见——报表读者要能判断「是没人归并，还是键名脏」。

// LineStatRow 是仓库层返回的一行：按**原始段**聚合（未归并）。
// 计算放在 Go 侧而不是 SQL，是因为别名真源是 json；让 SQL 也认一份就等于第二份拷贝。
type LineStatRow struct {
	// Seg 是 split_part(key.name,'-',1)；key 已删除时为 "(deleted key)"。
	Seg string
	// Requests 该段在窗口内的请求数。
	Requests int64
	// ActualCost 实扣（SUM(actual_cost)）。
	ActualCost float64
	// ListCost 标价（SUM(total_cost)）。
	ListCost float64
}

// LineStat 是归并后的线统计。
type LineStat struct {
	Line       string  `json:"line"`
	Requests   int64   `json:"requests"`
	ActualCost float64 `json:"actual_cost"`
	ListCost   float64 `json:"list_cost"`
	// SharePct 占窗口内实扣总额的百分比（保留一位小数）。
	SharePct float64 `json:"share_pct"`
	// Segs 归并进这条线的原始段，便于核对「漢字拼写/短名」有没有漏。
	Segs []string `json:"segs"`
	// UnknownSegs 仅在 Line=="unknown" 时有值：无法归因的原始段。
	UnknownSegs []string `json:"unknown_segs,omitempty"`
}

// UnknownLine 是归并表的兜底桶名。**它不是一个产品线**，只是「未能归因」。
const UnknownLine = "unknown"

// LineAliasFor 返回该段被归并到的线；未登记时返回 ("", false)。
func LineAliasFor(seg string) (string, bool) {
	line, ok := lineAliases[seg]
	return line, ok
}

// AggregateLines 把按段的行压成按线，并保证 unknown 桶的原始段可见。
//
// 纯函数：不碰 db、不看时钟。测试因此可以覆盖「归并」「未知桶」「排序」三件事，
// 不需要起库——真实 SQL 的验证另由 tools/line-quota.sql 在真库上跑。
func AggregateLines(rows []LineStatRow) []LineStat {
	type acc struct {
		requests int64
		actual   float64
		list     float64
		segs     []string
	}
	byLine := map[string]*acc{}

	for _, row := range rows {
		line, ok := LineAliasFor(row.Seg)
		if !ok {
			line = UnknownLine
		}
		cur, exists := byLine[line]
		if !exists {
			cur = &acc{}
			byLine[line] = cur
		}
		cur.requests += row.Requests
		cur.actual += row.ActualCost
		cur.list += row.ListCost
		cur.segs = append(cur.segs, row.Seg)
	}

	totalActual := 0.0
	for _, cur := range byLine {
		totalActual += cur.actual
	}

	out := make([]LineStat, 0, len(byLine))
	for line, cur := range byLine {
		stat := LineStat{
			Line:       line,
			Requests:   cur.requests,
			ActualCost: round4(cur.actual),
			ListCost:   round4(cur.list),
			Segs:       sortStrings(cur.segs),
		}
		if line == UnknownLine {
			// unknown 的 Segs 语义就是「无法归因的原始段」，用同一个切片，避免两处不一致
			stat.UnknownSegs = stat.Segs
			stat.Segs = nil
		}
		if totalActual > 0 {
			stat.SharePct = round1(cur.actual / totalActual * 100)
		}
		out = append(out, stat)
	}

	// 排序：实扣降序；同额按线名升序，保证同一份数据两次查询顺序一致（可 diff）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].ActualCost != out[j].ActualCost {
			return out[i].ActualCost > out[j].ActualCost
		}
		return out[i].Line < out[j].Line
	})
	return out
}

func sortStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	// 去重：同一段只出现一次
	deduped := out[:1]
	for _, s := range out[1:] {
		if s != deduped[len(deduped)-1] {
			deduped = append(deduped, s)
		}
	}
	return deduped
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
