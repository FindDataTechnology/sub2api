package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 基线与稳定性（add-key-capacity-contract）。
//
// 这里刻意只测**容易写错的判据**，不测平凡真理：
//   - 成功率分母含失败（否则恒 100%）
//   - 无数据给 nil，不给 1 也不给 0
//   - 低置信度不给数值（小样本编造比不给更坏）

type stabilityRepoStub struct {
	APIKeyRepository
	keyRaw   *KeyStabilityRaw
	classRaw *KeyStabilityRaw
	err      error
}

func (s *stabilityRepoStub) GetKeyStabilityRaw(context.Context, int64, time.Time) (*KeyStabilityRaw, error) {
	return s.keyRaw, s.err
}

func (s *stabilityRepoStub) GetClassBaselineRaw(context.Context, string, string, time.Time) (*KeyStabilityRaw, error) {
	return s.classRaw, s.err
}

func fptr(v float64) *float64 { return &v }

// ── 稳定性：分母必须含失败 ────────────────────────────────

func TestBuildKeyStability_DenominatorIncludesFailures(t *testing.T) {
	st := buildKeyStability(KeyWindow24h, &KeyStabilityRaw{
		SuccessCount: 80, FailureCount: 20, ThrottleCount: 5,
	})

	require.NotNil(t, st.SuccessRatio)
	require.InDelta(t, 0.8, *st.SuccessRatio, 1e-9, "80/(80+20)=0.8，不是 100%")
	require.InDelta(t, 0.05, *st.ThrottleRatio, 1e-9, "5/(80+20)=0.05")
	require.True(t, st.HasData)
}

func TestBuildKeyStability_NoDataIsNilNotOne(t *testing.T) {
	st := buildKeyStability(KeyWindow1h, &KeyStabilityRaw{})

	require.Nil(t, st.SuccessRatio, "无数据必须是 nil —— 「没调用过」不等于「很稳定」")
	require.Nil(t, st.ThrottleRatio, "无数据不给 0")
	require.False(t, st.HasData)
}

func TestBuildKeyStability_AllFailuresIsZero(t *testing.T) {
	st := buildKeyStability(KeyWindow24h, &KeyStabilityRaw{SuccessCount: 0, FailureCount: 10, ThrottleCount: 10})

	require.NotNil(t, st.SuccessRatio)
	require.InDelta(t, 0.0, *st.SuccessRatio, 1e-9, "全失败就是 0，不能因除零给 nil 冒充无数据")
	require.InDelta(t, 1.0, *st.ThrottleRatio, 1e-9)
}

// ── 基线：置信度分档 + 低置信不给数值 ──────────────────────

func TestBuildKeyBaseline_LowConfidenceGivesNoNumbers(t *testing.T) {
	repo := &stabilityRepoStub{classRaw: &KeyStabilityRaw{
		SuccessCount: 10, FailureCount: 2, P50LatencyMs: fptr(900), P95LatencyMs: fptr(3000),
	}}
	s := &APIKeyService{apiKeyRepo: repo}

	bl, err := s.buildKeyBaseline(context.Background(), repo, &APIKey{ID: 1, Group: &Group{Platform: "openai"}}, nil, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	require.Equal(t, "population", bl.Source)
	require.Equal(t, "low", bl.Confidence)
	require.Nil(t, bl.P95LatencyMs, "低置信度不得给小数精度的数字")
	require.Nil(t, bl.P50LatencyMs)
	require.Nil(t, bl.ErrorRatio)
}

func TestBuildKeyBaseline_MediumAndHighGiveNumbers(t *testing.T) {
	repo := &stabilityRepoStub{classRaw: &KeyStabilityRaw{
		SuccessCount: 3000, FailureCount: 1000, P50LatencyMs: fptr(800), P95LatencyMs: fptr(2500),
	}}
	s := &APIKeyService{apiKeyRepo: repo}

	bl, err := s.buildKeyBaseline(context.Background(), repo, &APIKey{ID: 1, Group: &Group{Platform: "openai"}}, nil, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	require.Equal(t, "high", bl.Confidence)
	require.NotNil(t, bl.P95LatencyMs)
	require.InDelta(t, 0.25, *bl.ErrorRatio, 1e-9, "1000/4000")
	require.NotNil(t, bl.SustainableRPS)
}

func TestBuildKeyBaseline_NoClassSampleIsHonest(t *testing.T) {
	repo := &stabilityRepoStub{classRaw: &KeyStabilityRaw{}}
	s := &APIKeyService{apiKeyRepo: repo}

	bl, err := s.buildKeyBaseline(context.Background(), repo, &APIKey{ID: 1, Group: &Group{Platform: "openai"}}, nil, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	require.Nil(t, bl.P95LatencyMs)
	require.Contains(t, bl.Note, "无样本")
}

// ── 仓储不支持聚合时如实降级 ────────────────────────────

type noStabilityRepo struct{ APIKeyRepository }

func TestGetKeyStability_DegradesHonestlyWhenRepoUnsupported(t *testing.T) {
	s := &APIKeyService{apiKeyRepo: &noStabilityRepo{}}
	view, err := s.GetKeyStability(context.Background(), &APIKey{ID: 9}, KeyWindow24h, time.Hour)
	require.NoError(t, err)

	require.False(t, view.Stability.HasData)
	require.Nil(t, view.Stability.SuccessRatio)
	require.Equal(t, "low", view.Baseline.Confidence, "不支持聚合时不得声称高置信")
}

// ── 窗口解析 ──────────────────────────────────────────

func TestParseKeyUsageWindow(t *testing.T) {
	_, d, ok := ParseKeyUsageWindow("")
	require.True(t, ok)
	require.Equal(t, 24*time.Hour, d, "空值默认 24h")

	_, d, ok = ParseKeyUsageWindow("7d")
	require.True(t, ok)
	require.Equal(t, 7*24*time.Hour, d)

	_, _, ok = ParseKeyUsageWindow("30d")
	require.False(t, ok, "未知窗口必须拒绝，不能静默回落")
}

// ── 5.2 基线来源：自身样本达标必须切 personal ─────────────────
//
// 判据是"用了很久的 key 却回'继承自平台群体'"——那会让用户以为那是自己的表现。
// 阈值与置信度分档同一口径（medium=200 / high=2000）。

func TestBuildKeyBaseline_SwitchesToPersonalWhenOwnSamplesEnough(t *testing.T) {
	repo := &stabilityRepoStub{
		// 群体有大量样本，但本 key 自己也够 200 —— 必须用自身，不能继承群体
		classRaw: &KeyStabilityRaw{SuccessCount: 5000, FailureCount: 0, P95LatencyMs: fptr(900)},
	}
	own := &KeyStabilityRaw{SuccessCount: 180, FailureCount: 20, P95LatencyMs: fptr(123)}
	s := &APIKeyService{}
	bl, err := s.buildKeyBaseline(context.Background(), repo, &APIKey{ID: 1, Group: &Group{Platform: "openai"}}, own, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, "personal", bl.Source, "自身样本达阈值时必须切 personal")
	require.Equal(t, "medium", bl.Confidence)
	require.EqualValues(t, 200, bl.SampleRequests)
	require.NotNil(t, bl.P95LatencyMs)
	require.InDelta(t, 123, *bl.P95LatencyMs, 0.001, "必须用自身 p95，不是群体的 900")
	require.NotNil(t, bl.ErrorRatio)
	require.InDelta(t, 0.1, *bl.ErrorRatio, 0.001, "错误率来自自身：20/200")
}

func TestBuildKeyBaseline_HighConfidenceAtHighThreshold(t *testing.T) {
	s := &APIKeyService{}
	own := &KeyStabilityRaw{SuccessCount: 2000, FailureCount: 0}
	bl, err := s.buildKeyBaseline(context.Background(), &stabilityRepoStub{}, &APIKey{ID: 1}, own, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, "personal", bl.Source)
	require.Equal(t, "high", bl.Confidence)
}

func TestBuildKeyBaseline_FallsBackToPopulationBelowThreshold(t *testing.T) {
	repo := &stabilityRepoStub{
		classRaw: &KeyStabilityRaw{SuccessCount: 500, FailureCount: 0, P95LatencyMs: fptr(777)},
	}
	own := &KeyStabilityRaw{SuccessCount: 100, FailureCount: 0} // 不足 200
	s := &APIKeyService{}
	bl, err := s.buildKeyBaseline(context.Background(), repo, &APIKey{ID: 1, Group: &Group{Platform: "openai"}}, own, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, "population", bl.Source, "自身不足阈值时继承群体")
	require.EqualValues(t, 500, bl.SampleRequests)
	require.InDelta(t, 777, *bl.P95LatencyMs, 0.001)
}
