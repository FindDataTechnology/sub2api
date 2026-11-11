package service

import (
	"context"
	"time"
)

// 新 key 的基线 与 稳定性口径（add-key-capacity-contract）。
//
// 用户原话：「平台侧，怎么快速统计出某个新 key 的基础情况」「以及 key 的稳定性」。
//
// 两条设计纪律（都写进 spec 并有测试）：
//
//  1. **成功率的分母必须含失败**。`usage_logs` 只有成功的计费行，只用它算会恒得 100%
//     —— 本仓「读不到当成不存在」的同族陷阱。故失败必须来自 `ops_error_logs`。
//  2. **无数据给 null，不给 0 / 不给 1**。「没调用过」不等于「很稳定」也不等于「没流量」，
//     三者必须可分。

// KeyUsageWindow 是稳定性查询的时间窗。
type KeyUsageWindow string

const (
	KeyWindow1h  KeyUsageWindow = "1h"
	KeyWindow24h KeyUsageWindow = "24h"
	KeyWindow7d  KeyUsageWindow = "7d"
)

// ParseKeyUsageWindow 解析窗口参数，空值默认 24h，未知值返回 ok=false。
func ParseKeyUsageWindow(raw string) (KeyUsageWindow, time.Duration, bool) {
	switch KeyUsageWindow(raw) {
	case KeyWindow1h:
		return KeyWindow1h, time.Hour, true
	case KeyWindow24h, "":
		return KeyWindow24h, 24 * time.Hour, true
	case KeyWindow7d:
		return KeyWindow7d, 7 * 24 * time.Hour, true
	default:
		return "", 0, false
	}
}

// KeyStabilityRaw 是仓储层返回的原始计数（不含比率计算）。
//
// 成功侧与失败侧来自两张表，因此必须在同一时间窗、同一 api_key_id 上取，
// 否则比率会漂。
type KeyStabilityRaw struct {
	SuccessCount  int64
	FailureCount  int64
	ThrottleCount int64 // 429（被限流，非上游故障）
	// P95LatencyMs 只在成功请求上有意义（失败请求的 duration 反映的是失败快，
	// 混进来会把 p95 拉低，看起来"更快"）。
	P95LatencyMs *float64
	// P50LatencyMs 只在类基线查询里填（中位数口径）。
	P50LatencyMs *float64
	// ActiveHoursUTC 有调用的 UTC 小时集合（0-23），用于"什么时候最忙"。
	ActiveHoursUTC []int
	TotalCost      float64
}

// KeyStability 是稳定性的对外形状。
type KeyStability struct {
	Window KeyUsageWindow `json:"window"`
	// SuccessRatio 的分母 = SuccessCount + FailureCount。样本为 0 时为 nil（不是 1）。
	SuccessRatio *float64 `json:"success_ratio"`
	// ThrottleRatio = 429 / (成功 + 失败)。同样是 nil 而非 0。
	ThrottleRatio  *float64 `json:"throttle_ratio"`
	P95LatencyMs   *float64 `json:"p95_latency_ms"`
	SuccessCount   int64    `json:"success_count"`
	FailureCount   int64    `json:"failure_count"`
	ThrottleCount  int64    `json:"throttle_count"`
	ActiveHoursUTC []int    `json:"active_hours_utc"`
	TotalCostUSD   float64  `json:"total_cost_usd"`
	HasData        bool     `json:"has_data"`
}

// KeyBaseline 是新 key 的"基础情况"：从同类群体继承，带置信度。
//
// 新 key 没有历史，所以基线必须来自 `(platform, model)` 类的群体统计，
// 并如实标注置信度——**小样本下给小数精度的数字就是编造**。
type KeyBaseline struct {
	// Source：population（群体继承）| personal（自身样本已足够）
	Source string `json:"source"`
	// Confidence：low | medium | high。low 时数值必须为 nil。
	Confidence     string   `json:"confidence"`
	SampleRequests int64    `json:"sample_requests"`
	SustainableRPS *float64 `json:"sustainable_rps"`
	P50LatencyMs   *float64 `json:"p50_latency_ms"`
	P95LatencyMs   *float64 `json:"p95_latency_ms"`
	ErrorRatio     *float64 `json:"error_ratio"`
	Note           string   `json:"note"`
}

// KeyStabilityView 是端点返回：基线 + 稳定性。
type KeyStabilityView struct {
	KeyID     int64        `json:"key_id"`
	Baseline  KeyBaseline  `json:"baseline"`
	Stability KeyStability `json:"stability"`
}

// KeyStabilityRepository 由 apiKey 仓储实现（同一连接、同一事务语义）。
type KeyStabilityRepository interface {
	// GetKeyStabilityRaw 取某 key 在 [since, now) 的成功/失败/429 计数与 p95、活跃小时。
	GetKeyStabilityRaw(ctx context.Context, apiKeyID int64, since time.Time) (*KeyStabilityRaw, error)
	// GetClassBaselineRaw 取某 (platform, model) 类在窗口内的群体统计（中位数口径）。
	GetClassBaselineRaw(ctx context.Context, platform, model string, since time.Time) (*KeyStabilityRaw, error)
}
