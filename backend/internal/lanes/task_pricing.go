package lanes

import (
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 价格维度（add-ciyuan-task-lane S4）：只用既有倍率，且**只决定「何时发起」**。
//
// 硬边界（D3）：实扣永远由请求发起后的既有 PricingAt 决定。
// 这里算出来的一切都带 `_estimate` 后缀，只用于「值不值得等」，
// 不参与对账、不覆盖账单 —— 允许两者不等。
//
// 为什么不读价卡：车道拿不到 BillingService，而按模型价卡再算一遍就等于
// 造第二套计价。改用「该模型近 7 天实扣单次均值」作基数，乘以峰谷因子，
// 得的是**经验估价**：口径写清、来源可查（usage_logs），且不会与账单算法分叉。

// priceSnapshot 是决策时刻的价格快照。价格一律现读，不缓存整窗（Risks 末条）。
type priceSnapshot struct {
	// PeakMultiplier 是当前生效的复合因子（分组高峰 × DeepSeek 官方峰谷）。
	// 1.0 = 低谷。
	PeakMultiplier float64
	// DeepSeekMultiplier 单独保留，用于判断「能否估算下一个低谷时刻」。
	DeepSeekMultiplier float64
	// GroupMultiplier 是分组自身的高峰因子（订阅型分组的 PeakRate）。
	GroupMultiplier float64
	// NextOffpeakStart 下一个低谷时刻（UTC）。仅在 DeepSeek 处于高峰时非零。
	NextOffpeakStart time.Time
	// PeakEndsAt 当前高峰段结束时刻（UTC）；不在高峰时为零值。
	PeakEndsAt time.Time
}

// priceSnapshotAt 读取指定时刻的价格快照。group 可为 nil。
func priceSnapshotAt(group *service.Group, now time.Time) priceSnapshot {
	ds := service.DeepSeekPeakMultiplierAt(now)
	grp := 1.0
	if group != nil {
		grp = group.PeakMultiplierAt(now)
	}
	snap := priceSnapshot{
		PeakMultiplier:     ds * grp,
		DeepSeekMultiplier: ds,
		GroupMultiplier:    grp,
	}
	if ds > 1.0 {
		snap.NextOffpeakStart = service.NextDeepSeekOffPeakStart(now)
		snap.PeakEndsAt = service.NextPeakWindowEnd(now)
	}
	return snap
}

// deferDecision 是推迟判定的结论。**否决原因必须可读**（task 4.2：
// 可观测，不静默）——调用方要能回答「它为什么没等」。
type deferDecision struct {
	Defer         bool
	DeferredUntil *time.Time
	SavingRatio   float64
	Reason        string
}

// decideDefer 按「省得够 + 来得及」两条同时成立才推迟。
//
//	minSaving 默认 0.2：S0 实测峰谷 1.9×，单次省 ≈47%，5% 的门槛会让
//	「省 6%」也去等 4 小时 —— 便宜不值这个等待。
func decideDefer(now time.Time, deadline *time.Time, snap priceSnapshot, minSaving float64) deferDecision {
	d := deferDecision{}
	if snap.PeakMultiplier <= 1.0 {
		d.Reason = "already_offpeak"
		return d
	}
	d.SavingRatio = 1.0 - 1.0/snap.PeakMultiplier
	if snap.GroupMultiplier > 1.0 && snap.DeepSeekMultiplier <= 1.0 {
		// 分组高峰但上游低谷：分组的窗口配置在本车道不可见（PeakStart/PeakEnd 未导出），
		// 无法估算「几点结束」。此时**不推迟**——宁可立刻跑，也不收下一个说不清何时开始的等待。
		d.Reason = "group_peak_window_unknown"
		return d
	}
	if d.SavingRatio < minSaving {
		d.Reason = fmt.Sprintf("saving_too_small: ratio=%.2f < min=%.2f", d.SavingRatio, minSaving)
		return d
	}
	if snap.NextOffpeakStart.IsZero() {
		d.Reason = "offpeak_start_unknown"
		return d
	}
	if deadline != nil && !snap.NextOffpeakStart.Add(taskEstDuration).Before(*deadline) {
		// 硬约束：推迟后的估计完成时刻必须早于截止（Risks「推迟导致错过 SLO」）。
		d.Reason = fmt.Sprintf("deadline_too_tight: offpeak_start=%s + est_duration=%s >= deadline=%s",
			snap.NextOffpeakStart.UTC().Format(time.RFC3339),
			taskEstDuration, deadline.UTC().Format(time.RFC3339))
		return d
	}
	until := snap.NextOffpeakStart
	d.Defer = true
	d.DeferredUntil = &until
	d.Reason = fmt.Sprintf("deferred_to_offpeak: saving=%.2f peak_multiplier=%.2f until=%s",
		d.SavingRatio, snap.PeakMultiplier, until.UTC().Format(time.RFC3339))
	return d
}

// costEstimate 是估价。**字段名带 _estimate 是纪律**：它与 actual_cost 并存且允许不等。
type costEstimate struct {
	// BaseAvgCostUSD 是基数：该模型近窗实扣单次均值（来自 usage_logs）。
	BaseAvgCostUSD *float64
	// AtPeakUSD = base × 当前因子。
	AtPeakUSD *float64
	// AtOffpeakUSD = base × 1.0（低谷因子恒为 1）。
	AtOffpeakUSD *float64
	Source       string
}

// buildCostEstimate 用经验均值 × 峰谷因子生成估价。
// base 为 nil（该模型无历史样本）时三个数值全 nil ——**不编造**。
func buildCostEstimate(base *float64, snap priceSnapshot) costEstimate {
	est := costEstimate{
		BaseAvgCostUSD: base,
		Source:         "usage_logs_7d_avg_per_request",
	}
	if base == nil {
		est.Source = "no_historical_samples"
		return est
	}
	peak := *base * snap.PeakMultiplier
	off := *base
	est.AtPeakUSD = &peak
	est.AtOffpeakUSD = &off
	return est
}

// feasibleByDeadline 判定「收下这个任务后能否在 deadline 前跑完」。
//
// queue_wait_estimate = 队列中排在它前面的工作量 / 观测完成速率。
// 速率**只来自实测**（CompletedSince），不来自账号数量或 Concurrency（D5/I4）。
// 速率无样本时用 taskUnknownRPS 兜底（保守，宁可拒）。
func feasibleByDeadline(now time.Time, deadline *time.Time, pendingAhead int64, observedRPS float64) (bool, time.Duration, time.Duration) {
	if deadline == nil {
		return true, 0, 0
	}
	rps := observedRPS
	if rps <= 0 {
		rps = taskUnknownRPS
	}
	queueWait := time.Duration(float64(pendingAhead)/rps) * time.Second
	available := deadline.Sub(now)
	return queueWait+taskEstDuration <= available, queueWait, available
}
