package lanes

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 能力快照的 pricing / capacity 两段（add-ciyuan-task-lane S1）。
//
// S1 的定位：**零 schema、零写路径**，只让调用方回答两个问题：
//
//	「现在贵不贵、什么时候便宜？」  → pricing
//	「这类模型现在跑得动多快？」    → capacity
//
// 两条硬纪律：
//   - pricing 的数据源只有既有倍率函数（service.DeepSeekPeakMultiplierAt 等），
//     **不新增任何配置项**，也不缓存整窗（现读现算，避免与账单价漂移）。
//   - capacity **绝不暴露账号数、槽位数或 account.Concurrency** —— 那是账号层，
//     账号会变，暴露出去就是把易变拓扑写进对外契约。只给观测值 + 置信度，
//     样本不足时给 null（**不拿 0 冒充**）。

const (
	capacityWindow = 3 * 24 * time.Hour
	// 置信度分档：与 key 稳定性端点同一口径，避免同一件事两套话术。
	capacityMinActiveMinutesMedium = 60
	capacityMinActiveMinutesHigh   = 600
)

type capabilitiesPricing struct {
	// Peak 当前是否处于高峰（倍率 > 1）。
	Peak bool `json:"peak"`
	// Multiplier 是当前复合倍率（分组高峰 × 上游峰谷），1.0 = 低谷。
	Multiplier float64 `json:"multiplier"`
	// PeakEndsAt 当前高峰段结束时刻（UTC RFC3339）；不在高峰时为 null。
	PeakEndsAt *string `json:"peak_ends_at"`
	// NextOffpeakStart / NextOffpeakEnd 下一个低谷窗口（UTC RFC3339）；已在低谷时给出当前窗口。
	NextOffpeakStart *string `json:"next_offpeak_start"`
	NextOffpeakEnd   *string `json:"next_offpeak_end"`
	Timezone         string  `json:"timezone"`
	Source           string  `json:"source"`
	// Advisory：所有决策建议都是 advisory，不承诺发起时仍成立（窗口边界可能刚好跨过）。
	Advisory bool `json:"advisory"`
}

type capabilitiesCapacity struct {
	Model string `json:"model"`
	// SustainableRPS 是观测到的可持续速率（分钟级 p95）；样本不足时为 null。
	SustainableRPS *float64 `json:"sustainable_rps"`
	// ErrorRatio 是该模型近窗失败占比（分母含成功）；无样本为 null。
	ErrorRatio *float64 `json:"error_ratio"`
	// SampleActiveMinutes 是样本量（有请求的分钟数），让调用方自己判断置信度。
	SampleActiveMinutes int64  `json:"sample_active_minutes"`
	Confidence          string `json:"confidence"` // low | medium | high
	Source              string `json:"source"`
	Advisory            bool   `json:"advisory"`
}

// buildCapabilitiesPricing 组装价格段。
func buildCapabilitiesPricing(group *service.Group, now time.Time) capabilitiesPricing {
	snap := priceSnapshotAt(group, now)
	p := capabilitiesPricing{
		Peak:       snap.PeakMultiplier > 1.0,
		Multiplier: snap.PeakMultiplier,
		Timezone:   service.DeckPeakTimezone,
		Source:     "deepseek_peak_multiplier + group_peak_rate",
		Advisory:   true,
	}
	if !snap.PeakEndsAt.IsZero() {
		v := snap.PeakEndsAt.UTC().Format(time.RFC3339)
		p.PeakEndsAt = &v
	}
	start := snap.NextOffpeakStart
	if start.IsZero() && snap.PeakMultiplier <= 1.0 {
		// 已在低谷：给出「当前这段低谷」的窗口，而不是 null ——
		// 调用方的问题是「什么时候能便宜地跑」，答案就是「现在，直到 X 点」。
		start = now.Truncate(time.Minute)
	}
	if !start.IsZero() {
		s := start.UTC().Format(time.RFC3339)
		p.NextOffpeakStart = &s
		if end := service.NextDeepSeekOffPeakEnd(start); !end.IsZero() {
			e := end.UTC().Format(time.RFC3339)
			p.NextOffpeakEnd = &e
		}
	}
	return p
}

// buildCapabilitiesCapacity 组装容量段。
//
// model 为空或观测不足时数值为 nil 并如实标注 confidence=low ——
// 「没观测到」不等于「容量是 0」，也不等于「无限」。
func buildCapabilitiesCapacity(ctx context.Context, model string, now time.Time) capabilitiesCapacity {
	cap := capabilitiesCapacity{
		Model:    model,
		Source:   "observed_usage_logs_3d",
		Advisory: true,
	}
	store := service.GetLanesTaskStore()
	if store == nil || model == "" {
		cap.Confidence = "low"
		cap.Source = "task_store_unavailable"
		return cap
	}
	since := now.Add(-capacityWindow)
	rps, activeMinutes, err := store.ClassThroughput(ctx, model, since)
	if err != nil {
		cap.Confidence = "low"
		cap.Source = "observe_failed"
		return cap
	}
	cap.SampleActiveMinutes = activeMinutes
	ratio, rerr := store.ErrorRatioForModel(ctx, model, since)
	if rerr == nil {
		cap.ErrorRatio = ratio
	}
	switch {
	case activeMinutes >= capacityMinActiveMinutesHigh:
		cap.Confidence = "high"
		cap.SustainableRPS = &rps
	case activeMinutes >= capacityMinActiveMinutesMedium:
		cap.Confidence = "medium"
		cap.SustainableRPS = &rps
	default:
		// 样本不足：不给数值。小样本的小数精度是编造。
		cap.Confidence = "low"
	}
	return cap
}

// primaryModelFor 选出「这个 key 实际在跑」的模型：近 7 天用得最多的那个。
// 取不到时退回能力快照里的第一个可调度模型。
func primaryModelFor(ctx context.Context, apiKeyID int64, available []string) string {
	if store := service.GetLanesTaskStore(); store != nil {
		if m, err := store.TopModelForKey(ctx, apiKeyID, 7*24*time.Hour); err == nil && m != "" {
			return m
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return ""
}

// capabilitiesAddendum 是 pricing + capacity 的挂载点（ServeCapabilities 调用）。
func (r *capabilitiesResponse) attachPricingAndCapacity(p capabilitiesPricing, cp capabilitiesCapacity) {
	r.Pricing = &p
	r.Capacity = &cp
}
