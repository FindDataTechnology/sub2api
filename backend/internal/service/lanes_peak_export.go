package service

import "time"

// 峰谷窗口的**只读导出**（add-ciyuan-task-lane S1.1 / S4）。
//
// 为什么需要这一层：计费侧的 `deepseekPeakMultiplierAt`（billing_service.go）
// 是未导出的纯函数。任务车道要回答「现在贵不贵、下次什么时候便宜」，
// 只能借用同一个实现——**绝不能在车道里重写一份窗口表**：两份窗口一旦漂移，
// 就会出现「估价按旧窗口推迟、账单按新窗口计费」的分叉。
//
// 因此这里只做包装，不引入任何新配置、不读取任何外部状态。

// DeepSeekPeakMultiplierAt 返回指定时刻的 DeepSeek 官方峰谷因子（1.0 或 2.0）。
// 与 billing_service.go 的 deepseekPeakMultiplierAt 是同一个实现。
func DeepSeekPeakMultiplierAt(now time.Time) float64 {
	return deepseekPeakMultiplierAt(now)
}

// peakScanStep 是窗口扫描步长。窗口边界都落在整点，分钟级扫描足够；
// 只在提交与快照时调用（非热路径），代价可忽略。
const peakScanStep = time.Minute

// peakScanHorizon 是扫描上限。DeepSeek 窗口以天为周期，7 天必能覆盖
// （含周末全低谷的情形）。
const peakScanHorizon = 8 * 24 * time.Hour

// NextDeepSeekOffPeakStart 返回 now（含）之后第一个低谷时刻。
// 若 now 已是低谷，原样返回 now。
func NextDeepSeekOffPeakStart(now time.Time) time.Time {
	t := now.Truncate(peakScanStep)
	for i := time.Duration(0); i <= peakScanHorizon; i += peakScanStep {
		if deepseekPeakMultiplierAt(t) <= 1.0 {
			return t
		}
		t = t.Add(peakScanStep)
	}
	return now
}

// NextPeakWindowEnd 返回 now 所在/之后第一个高峰段的结束时刻。
// 不在高峰时返回 zero（调用方据此不展示「高峰到几点」）。
func NextPeakWindowEnd(now time.Time) time.Time {
	t := now.Truncate(peakScanStep)
	if deepseekPeakMultiplierAt(t) <= 1.0 {
		return time.Time{}
	}
	for i := time.Duration(0); i <= peakScanHorizon; i += peakScanStep {
		if deepseekPeakMultiplierAt(t) <= 1.0 {
			return t
		}
		t = t.Add(peakScanStep)
	}
	return time.Time{}
}

// NextDeepSeekOffPeakEnd 返回 from 之后第一个高峰的开始时刻（即「这段低谷到几点结束」）。
// 扫描上限 8 天；一直低谷时返回 zero。
func NextDeepSeekOffPeakEnd(from time.Time) time.Time {
	t := from.Truncate(peakScanStep)
	for i := time.Duration(0); i <= peakScanHorizon; i += peakScanStep {
		if deepseekPeakMultiplierAt(t) > 1.0 {
			return t
		}
		t = t.Add(peakScanStep)
	}
	return time.Time{}
}

// DeckPeakTimezone 是窗口判定所用时区口径的对外说明。
// 窗口本身按 UTC 小时 + 北京时间工作日判定（见 deepseekPeakMultiplierAt），
// 对外如实用 UTC 表达，避免调用方以为可以按本地时区推算。
const DeckPeakTimezone = "UTC"
