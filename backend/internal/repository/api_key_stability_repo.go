package repository

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// key 稳定性聚合（add-key-capacity-contract）。
//
// 两条口径纪律：
//  1. **成功率的分母必须含失败** —— usage_logs 只有成功计费行，只用它算会恒得 100%
//     （本仓"读不到当成不存在"的同族陷阱）。失败与 429 一律取自 ops_error_logs。
//  2. **p95 只用成功请求** —— 失败的 duration 反映"失败得快"，混进来会把 p95 拉低，
//     看起来更健康，是典型的自证陷阱。
//
// 时间窗两端都由参数传入，不在 SQL 里写 now()：窗口起点只有一个来源，测试也能固定时刻。
// 活跃小时用 string_agg 而不是 array_agg：少一层数组扫描，SQL 与解析都简单。

const keyStabilitySQL = `
WITH succ AS (
    SELECT count(*) AS n,
           percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95,
           COALESCE(sum(actual_cost), 0) AS cost,
           string_agg(DISTINCT (extract(hour FROM created_at AT TIME ZONE 'UTC')::int)::text, ',') AS hours
    FROM usage_logs
    WHERE api_key_id = $1 AND created_at >= $2 AND created_at < $3
),
fail AS (
    SELECT count(*) AS n,
           count(*) FILTER (WHERE status_code = 429) AS throttled,
           string_agg(DISTINCT (extract(hour FROM created_at AT TIME ZONE 'UTC')::int)::text, ',') AS hours
    FROM ops_error_logs
    WHERE api_key_id = $1 AND created_at >= $2 AND created_at < $3
)
SELECT COALESCE(succ.n, 0), succ.p95, COALESCE(succ.cost, 0), COALESCE(succ.hours, ''),
       COALESCE(fail.n, 0), COALESCE(fail.throttled, 0), COALESCE(fail.hours, '')
FROM succ, fail
`

func (r *apiKeyRepository) GetKeyStabilityRaw(ctx context.Context, apiKeyID int64, since time.Time) (*service.KeyStabilityRaw, error) {
	if r == nil || r.sql == nil {
		return nil, fmt.Errorf("nil sql executor")
	}
	var (
		succN, failN, throttled int64
		p95                     *float64
		cost                    float64
		succHours, failHours    string
	)
	rows, err := r.sql.QueryContext(ctx, keyStabilitySQL, apiKeyID, since, time.Now())
	if err != nil {
		return nil, fmt.Errorf("query key stability: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return &service.KeyStabilityRaw{}, nil
	}
	if err := rows.Scan(&succN, &p95, &cost, &succHours, &failN, &throttled, &failHours); err != nil {
		return nil, fmt.Errorf("scan key stability: %w", err)
	}
	return &service.KeyStabilityRaw{
		SuccessCount:   succN,
		FailureCount:   failN,
		ThrottleCount:  throttled,
		P95LatencyMs:   p95,
		TotalCost:      cost,
		ActiveHoursUTC: mergeHours(parseHours(succHours), parseHours(failHours)),
	}, nil
}

// 类基线：按 (platform, model) 取群体统计。p50 即中位数语义，用作"典型延迟"。
const classBaselineSQL = `
WITH succ AS (
    SELECT count(*) AS n,
           percentile_cont(0.5)  WITHIN GROUP (ORDER BY u.duration_ms) AS p50,
           percentile_cont(0.95) WITHIN GROUP (ORDER BY u.duration_ms) AS p95,
           COALESCE(sum(u.actual_cost), 0) AS cost,
           string_agg(DISTINCT (extract(hour FROM u.created_at AT TIME ZONE 'UTC')::int)::text, ',') AS hours
    FROM usage_logs u
    JOIN accounts a ON a.id = u.account_id
    WHERE a.platform = $1 AND lower(u.model) = lower($2)
      AND u.created_at >= $3 AND u.created_at < $4
),
fail AS (
    SELECT count(*) AS n,
           count(*) FILTER (WHERE status_code = 429) AS throttled
    FROM ops_error_logs e
    WHERE lower(COALESCE(e.model, '')) = lower($2)
      AND e.created_at >= $3 AND e.created_at < $4
)
SELECT COALESCE(succ.n, 0), succ.p50, succ.p95, COALESCE(succ.cost, 0),
       COALESCE(fail.n, 0), COALESCE(fail.throttled, 0)
FROM succ, fail
`

func (r *apiKeyRepository) GetClassBaselineRaw(ctx context.Context, platform, model string, since time.Time) (*service.KeyStabilityRaw, error) {
	if r == nil || r.sql == nil {
		return nil, fmt.Errorf("nil sql executor")
	}
	var (
		succN, failN, throttled int64
		p50, p95                *float64
		cost                    float64
	)
	rows, err := r.sql.QueryContext(ctx, classBaselineSQL, platform, model, since, time.Now())
	if err != nil {
		return nil, fmt.Errorf("query class baseline: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return &service.KeyStabilityRaw{}, nil
	}
	if err := rows.Scan(&succN, &p50, &p95, &cost, &failN, &throttled); err != nil {
		return nil, fmt.Errorf("scan class baseline: %w", err)
	}
	return &service.KeyStabilityRaw{
		SuccessCount:  succN,
		FailureCount:  failN,
		ThrottleCount: throttled,
		P50LatencyMs:  p50,
		P95LatencyMs:  p95,
		TotalCost:     cost,
	}, nil
}

// parseHours 解析 "1,5,9" 形状的活跃小时列表。
func parseHours(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// mergeHours 合并两侧小时集合并按 0-23 升序去重（便于人读"什么时候最忙"）。
func mergeHours(a, b []int) []int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := [24]bool{}
	for _, v := range append(append([]int{}, a...), b...) {
		if v >= 0 && v < 24 {
			seen[v] = true
		}
	}
	out := make([]int, 0, 24)
	for h, ok := range seen {
		if ok {
			out = append(out, h)
		}
	}
	return out
}
