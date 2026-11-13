package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetLineStatsRows 按 **key 名首段** 聚合窗口内的用量，返回未归并的原始段。
//
// 为什么归并放在 Go 侧而不在 SQL 里：别名真源是 tools/line-aliases.json，
// 由 tools/gen-line-aliases.mjs 生成到 usagestats/line_aliases_gen.go。
// 若本查询也用一份内联 VALUES，别名就有了第二份拷贝——那正是
// ciyuan-consumer-surface 4.1 刚消灭的形态（当时全仓四份、两种形状）。
//
// 段名取 split_part(k.name,'-',1)；key 已被删除时 k.name 为 NULL，
// 统一落成 "(deleted key)"，与 tools/attribution-by-line.sql 的写法一致。
//
// 只读：一条 SELECT，无 CTE 写入、无临时表。
func (r *usageLogRepository) GetLineStatsRows(ctx context.Context, startTime, endTime time.Time) ([]usagestats.LineStatRow, error) {
	const query = `
		SELECT
			CASE
				WHEN k.name IS NULL OR k.name = '' THEN '(deleted key)'
				ELSE split_part(k.name, '-', 1)
			END AS seg,
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

	out := make([]usagestats.LineStatRow, 0, 32)
	for rows.Next() {
		var row usagestats.LineStatRow
		if err := rows.Scan(&row.Seg, &row.Requests, &row.ActualCost, &row.ListCost); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
