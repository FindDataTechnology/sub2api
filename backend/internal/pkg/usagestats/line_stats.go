package usagestats

import (
	"sort"
	"strings"
)

// ── 词元「按线」聚合（ciyuan-consumer-surface 4.3 / fix-line-attribution-declared-prefix）──
//
// 归并真源是 tools/line-aliases.json（生成到 line_aliases_gen.go），不是本文件。
// 本文件只做两件事：把「key 名」映射成线，并把无法归因的键**单列**。
//
// 三条口径纪律（与 tools/line-quota.sql 的 resolved CTE 同一套，改一处要同时改另一处）：
//  1. 线身份默认 = key 名首段（第一个 '-' 之前）；**声明式整名前缀优先于首段**（最长匹配）。
//  2. 归一只认别名表；没命中的落 unknown，并保留首段原文。
//  3. unknown 的原始段必须能看见——报表读者要能判断「是没人归并，还是键名脏」。
//
// 为什么仓库层给的是**完整 key 名**而不是首段：前缀规则要看完整名字才生效
// （`wanxing-lex-console-copilot` 与 `wanxing-foo` 的首段相同、归属不同）。
// 在 SQL 里截首段等于在 Go 侧永久放弃这件事。

// LinePrefixAlias 是「首段撒谎」的修正规则：整名前缀 → 线。
// 顺序即优先级（最长前缀在前），由 tools/gen-line-aliases.mjs 保证。
type LinePrefixAlias struct {
	Prefix string
	Line   string
	Why    string
}

// LineStatRow 是仓库层返回的一行：按**完整 key 名**聚合（未归并）。
// 计算放在 Go 侧而不是 SQL，是因为别名真源是 json；让 SQL 也认一份就等于第二份拷贝。
type LineStatRow struct {
	// KeyName 是 api_keys.name 原文；key 已删除或缺失时为空串。
	KeyName string
	// Requests 该键在窗口内的请求数。
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
	// Segs 按**首段**归并进这条线的段，便于核对「漢字拼写/短名」有没有漏。
	Segs []string `json:"segs,omitempty"`
	// Prefixes 按**声明前缀**归并进这条线的键，空表示这条线的数全来自首段。
	// 读面靠它回答「这个数是不是被前缀规则改写过」。
	Prefixes []string `json:"prefixes,omitempty"`
	// UnknownSegs 仅在 Line=="unknown" 时有值：无法归因的原始段。
	UnknownSegs []string `json:"unknown_segs,omitempty"`
}

// UnknownLine 是归并表的兜底桶名。**它不是一个产品线**，只是「未能归因」。
const UnknownLine = "unknown"

// 归因依据的三档，与 SQL 侧 resolved.by_rule 逐字一致。
const (
	ByRuleSeg    = "seg"
	ByRulePrefix = "prefix"
	ByRuleNone   = "none"
)

// LineAliasFor 返回该段被归并到的线；未登记时返回 ("", false)。
func LineAliasFor(seg string) (string, bool) {
	line, ok := lineAliases[seg]
	return line, ok
}

// FirstSegment 取 key 名首段（第一个 '-' 之前）。
func FirstSegment(name string) string {
	if i := strings.Index(name, "-"); i >= 0 {
		return name[:i]
	}
	return name
}

// LineAliasForName 按「最长匹配前缀 → 首段」解析一个 key 名的归属。
// 返回 (line, why, byRule, rule)：rule 是命中的那条规则原文（前缀或段名）；
// 未归因时 line 为空、rule 仍是首段原文（空键名时为空串）——unknown 桶要靠它点名。
func LineAliasForName(name string) (line, why, byRule, rule string) {
	for _, pa := range linePrefixAliases {
		if strings.HasPrefix(name, pa.Prefix) {
			return pa.Line, pa.Why, ByRulePrefix, pa.Prefix
		}
	}
	seg := FirstSegment(name)
	if l, ok := lineAliases[seg]; ok {
		return l, LineAliasReason[seg], ByRuleSeg, seg
	}
	return "", "", ByRuleNone, seg
}

// AggregateLines 把按键名的行压成按线，并保证 unknown 桶的原始段可见。
//
// 纯函数：不碰 db、不看时钟。测试因此可以覆盖「前缀优先」「回退首段」「未知桶」「排序」，
// 不需要起库——真实 SQL 的验证另由 tools/line-quota.sql 在真库上跑。
func AggregateLines(rows []LineStatRow) []LineStat {
	type acc struct {
		requests int64
		actual   float64
		list     float64
		segs     []string
		prefixes []string
		unknown  []string
	}
	byLine := map[string]*acc{}

	for _, row := range rows {
		line, _, byRule, rule := LineAliasForName(row.KeyName)
		if line == "" {
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
		switch byRule {
		case ByRulePrefix:
			cur.prefixes = append(cur.prefixes, rule)
		case ByRuleSeg:
			cur.segs = append(cur.segs, rule)
		default:
			// 空键名说明 key 行已不在（删了或 id 悬空）——与 SQL 侧 '(deleted key)' 同义。
			if rule == "" {
				rule = "(deleted key)"
			}
			cur.unknown = append(cur.unknown, rule)
		}
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
			Prefixes:   sortStrings(cur.prefixes),
		}
		if line == UnknownLine {
			// unknown 的 UnknownSegs 语义就是「无法归因的原始段」，用同一个切片，避免两处不一致
			stat.UnknownSegs = sortStrings(cur.unknown)
			stat.Segs = nil
			stat.Prefixes = nil
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
