//go:build unit

package lanes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 能力快照 capacity 段的两个口径（add-ciyuan-task-lane，真机验证后补）。
//
// 背景：上线后真机观测到 `DeepSeek-V4-Pro-Official` 报
// `error_ratio=1` 而 `sample_active_minutes=0`（实测 usage_logs=0、
// ops_error_logs=1）。算术没错，但一个样本的「100% 失败」是噪音不是信号。
// 这条测试钉住「样本不足就不报」的口径，防止它再退化。

func TestAttachErrorRatioIsGatedByConfidence(t *testing.T) {
	ratio := 1.0

	low := capabilitiesCapacity{}
	attachErrorRatio(&low, &ratio, "low")
	require.Nil(t, low.ErrorRatio, "低置信度下不得回报错误率：1 个样本不是信号")

	medium := capabilitiesCapacity{}
	attachErrorRatio(&medium, &ratio, "medium")
	require.NotNil(t, medium.ErrorRatio)
	require.InDelta(t, 1.0, *medium.ErrorRatio, 1e-9)

	high := capabilitiesCapacity{}
	attachErrorRatio(&high, &ratio, "high")
	require.NotNil(t, high.ErrorRatio)

	// 无样本（ratio=nil）时即便置信度够也必须留空，不能变 0。
	noSample := capabilitiesCapacity{}
	attachErrorRatio(&noSample, nil, "high")
	require.Nil(t, noSample.ErrorRatio, "没查到与 0% 是两件事")
}

// 容量数值同样只在有样本时给：低置信度必须 sustainable_rps=null。
// 这里用纯函数层面的断言（buildCapabilitiesCapacity 依赖 DB，归 dbtest 覆盖）。
func TestCapacityConfidenceThresholds(t *testing.T) {
	require.EqualValues(t, 600, capacityMinActiveMinutesHigh)
	require.EqualValues(t, 60, capacityMinActiveMinutesMedium)
	require.Less(t, capacityMinActiveMinutesMedium, capacityMinActiveMinutesHigh,
		"两档门槛必须递增，否则高/中档判定会互相覆盖")
}
