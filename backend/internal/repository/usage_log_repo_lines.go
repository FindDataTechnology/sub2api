package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetLineStatsRows 按 **完整 key 名** 聚合窗口内的用量，返回未归并的键名。
//
// 为什么给完整键名、而不是像旧版那样在 SQL 里 `split_part(name,'-',1)`：
// 前缀修正规则（`wanxing-lex-console-copilot` 实际替识律调）要看完整名字才生效，
// 一旦在 SQL 里截成首段，Go 侧就再也分不出「首段相同、归属不同」的键了。
// 归属解析只在 Go 侧（usagestats.LineAliasForName）与报表 SQL 的 resolved CTE 两处，
// 两者都由 tools/line-aliases.json 生成/校准，--check 抓漂移。
//
// key 已被删除时 k.name 为 NULL：落成空串，由归并侧记为 "(deleted key)"（与报表同义）。
//
// 只读：一条 SELECT，无 CTE 写入、无临时表。
func (r *usageLogRepository) GetLineStatsRows(ctx context.Context, startTime, endTime time.Time) ([]usagestats.LineStatRow, error) {
	const query = `
		SELECT
			COALESCE(k.name, '') AS key_name,
			COUNT(*) AS requests,
			COALESCE(SUM(u.actual_cost), 0) AS actual_cost,
			COALESCE(SUM(u.total_cost), 0) AS list_cost
		FROM usage_logs u
		LEFT JOIN api_keys k ON k.id = u.api_key_id
		WHERE u.created_at >= $1 AND u.created_at < $2
		GROUP BY 1
		ORDER BY 1
	`

	rows, err := r.sql.QueryContext(ctx, query, startTime, endTime)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	out := make([]usagestats.LineStatRow, 0, 64)
	for rows.Next() {
		var row usagestats.LineStatRow
		if err := rows.Scan(&row.KeyName, &row.Requests, &row.ActualCost, &row.ListCost); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
