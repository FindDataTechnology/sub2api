package service

import (
	"context"
	"fmt"
	"time"
)

// 基线 + 稳定性计算（add-key-capacity-contract）。
//
// 三条判据（都有测试钉住）：
//  1. 成功率分母 = 成功 + 失败；样本为 0 → **nil**，不是 1（"没调用过"≠"很稳定"）。
//  2. 置信度不足时数值给 nil，不给小数精度的假数字（小样本编造比不给更坏）。
//  3. 基线来源必须自证：population（群体继承）还是 personal（自身样本足够）。

const (
	// 群体样本分档：>=high 为 high，>=medium 为 medium，否则 low。
	// （「自身样本达到多少才从 population 切到 personal」复用 medium 这一档，
	//   原 baselinePersonalMinRequests 与它同值，已并入本条。）
	baselineClassHighRequests   = 2000
	baselineClassMediumRequests = 200
)

// GetKeyStability 取某 key 的基线 + 稳定性。
//
// key 由调用方保证已通过所有权校验（handler 负责 404）。
func (s *APIKeyService) GetKeyStability(ctx context.Context, apiKey *APIKey, window KeyUsageWindow, lookback time.Duration) (*KeyStabilityView, error) {
	if apiKey == nil {
		return nil, ErrAPIKeyNotFound
	}
	repo, ok := s.apiKeyRepo.(KeyStabilityRepository)
	if !ok {
		// 仓储未实现聚合：如实降级为"无数据"，不编造
		return &KeyStabilityView{
			KeyID:     apiKey.ID,
			Baseline:  KeyBaseline{Source: "population", Confidence: "low", Note: "仓储不支持稳定性聚合"},
			Stability: KeyStability{Window: window, HasData: false},
		}, nil
	}

	since := time.Now().Add(-lookback)
	raw, err := repo.GetKeyStabilityRaw(ctx, apiKey.ID, since)
	if err != nil {
		return nil, fmt.Errorf("key stability: %w", err)
	}
	st := buildKeyStability(window, raw)

	baseline, berr := s.buildKeyBaseline(ctx, repo, apiKey, raw, since)
	if berr != nil {
		return nil, berr
	}
	return &KeyStabilityView{KeyID: apiKey.ID, Baseline: *baseline, Stability: st}, nil
}

// buildKeyStability 由原始计数算比率。默认值全部为 nil/0，绝不预设"健康"。
func buildKeyStability(window KeyUsageWindow, raw *KeyStabilityRaw) KeyStability {
	if raw == nil {
		return KeyStability{Window: window, HasData: false}
	}
	total := raw.SuccessCount + raw.FailureCount
	st := KeyStability{
		Window:         window,
		SuccessCount:   raw.SuccessCount,
		FailureCount:   raw.FailureCount,
		ThrottleCount:  raw.ThrottleCount,
		P95LatencyMs:   raw.P95LatencyMs,
		ActiveHoursUTC: raw.ActiveHoursUTC,
		TotalCostUSD:   raw.TotalCost,
		HasData:        total > 0,
	}
	if total == 0 {
		return st
	}
	// 分母含失败：这是本函数存在的核心理由
	ratio := float64(raw.SuccessCount) / float64(total)
	st.SuccessRatio = &ratio
	throttle := float64(raw.ThrottleCount) / float64(total)
	st.ThrottleRatio = &throttle
	return st
}

// buildKeyBaseline 决定基线来源与置信度。
func (s *APIKeyService) buildKeyBaseline(ctx context.Context, repo KeyStabilityRepository, apiKey *APIKey, ownRaw *KeyStabilityRaw, since time.Time) (*KeyBaseline, error) {
	if apiKey == nil {
		return &KeyBaseline{Source: "personal", Confidence: "low", Note: "无 key 上下文"}, nil
	}

	// 自身样本达标 ⇒ 用自身数据，不继承群体。
	//
	// 为什么必须先判：一个用了几个月的 key 却回"继承自平台群体"是误导——用户会以为
	// 那是自己的表现。阈值复用 medium 档（baselineClassMediumRequests），与置信度分档同一口径。
	if ownRaw != nil {
		ownTotal := ownRaw.SuccessCount + ownRaw.FailureCount
		if ownTotal >= baselineClassMediumRequests {
			conf := "medium"
			if ownTotal >= baselineClassHighRequests {
				conf = "high"
			}
			bl := &KeyBaseline{
				Source:         "personal",
				Confidence:     conf,
				SampleRequests: ownTotal,
				P95LatencyMs:   ownRaw.P95LatencyMs,
				Note:           fmt.Sprintf("基于本 key 自身 %d 次样本", ownTotal),
			}
			if ownTotal > 0 {
				er := float64(ownRaw.FailureCount) / float64(ownTotal)
				bl.ErrorRatio = &er
			}
			rps := float64(ownRaw.SuccessCount) / sinceDurationSeconds(since)
			bl.SustainableRPS = &rps
			return bl, nil
		}
	}

	own := &KeyBaseline{Source: "personal", Confidence: "low", Note: "自身样本不足，且群体统计不可用"}

	platform := ""
	if apiKey.Group != nil {
		platform = apiKey.Group.Platform
	}
	// 模型取该 key 最近使用的模型不现实；类基线以 (platform, "") 为最粗粒度时
	// 交给调用方传 model（v1 用空串表示"该平台全部模型"）。
	classRaw, err := repo.GetClassBaselineRaw(ctx, platform, "", since)
	if err != nil {
		return nil, fmt.Errorf("class baseline: %w", err)
	}
	classTotal := int64(0)
	if classRaw != nil {
		classTotal = classRaw.SuccessCount + classRaw.FailureCount
	}

	if classTotal == 0 {
		own.Note = "该 (platform, model) 类在窗口内无样本，基线不可得"
		return own, nil
	}

	conf := "low"
	switch {
	case classTotal >= baselineClassHighRequests:
		conf = "high"
	case classTotal >= baselineClassMediumRequests:
		conf = "medium"
	}
	bl := &KeyBaseline{
		Source:         "population",
		Confidence:     conf,
		SampleRequests: classTotal,
		Note:           fmt.Sprintf("继承自平台 %q 的群体统计（%d 次样本）", platform, classTotal),
	}
	// low 置信度不给数值：小样本的小数精度是编造
	if conf != "low" {
		if classRaw.P95LatencyMs != nil {
			v := *classRaw.P95LatencyMs
			bl.P95LatencyMs = &v
		}
		if classRaw.P50LatencyMs != nil {
			v := *classRaw.P50LatencyMs
			bl.P50LatencyMs = &v
		}
		if classTotal > 0 {
			er := float64(classRaw.FailureCount) / float64(classTotal)
			bl.ErrorRatio = &er
		}
	}
	if classTotal > 0 {
		rps := float64(classRaw.SuccessCount) / sinceDurationSeconds(since)
		bl.SustainableRPS = &rps
	}
	return bl, nil
}

func sinceDurationSeconds(since time.Time) float64 {
	d := time.Since(since).Seconds()
	if d <= 0 {
		return 1
	}
	return d
}
