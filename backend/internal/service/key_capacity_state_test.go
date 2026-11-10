package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// state 视图（add-key-capacity-contract）：核心判据是 config_applied —— 缓存看到的
// 配置与库里一致才算"已生效"。这是把"改完等 15 秒猜"变成"直接看"的关键。

func TestBuildKeyCapacityState_NotAppliedWhenCacheDiffers(t *testing.T) {
	configured := &APIKey{
		ID: 1, UserID: 7,
		ConcurrencyRequest: 2, ConcurrencyLimit: 5, RPMLimit: 60,
		User: &User{ID: 7, Concurrency: 10},
	}
	// 缓存还是旧值（limit 3，库里已改 5）→ 未生效
	cached := &APIKey{
		ID: 1, UserID: 7,
		ConcurrencyRequest: 2, ConcurrencyLimit: 3, RPMLimit: 60,
	}

	st := BuildKeyCapacityState(configured, cached, nil)

	require.False(t, st.ConfigApplied, "缓存里还是旧配置，必须报未生效")
	require.Equal(t, 5, st.ConfiguredConcurrencyLimit)
	require.Equal(t, 10, st.PoolCapacity)
	require.Equal(t, "key", st.LimitSource, "key 5 < 池 10，生效来源是 key")
	require.Equal(t, "in_process", st.CountBackend)
}

func TestBuildKeyCapacityState_AppliedWhenCacheMatches(t *testing.T) {
	configured := &APIKey{
		ID: 1, UserID: 7,
		ConcurrencyRequest: 2, ConcurrencyLimit: 5,
		User: &User{ID: 7, Concurrency: 10},
	}
	cached := &APIKey{ID: 1, UserID: 7, ConcurrencyRequest: 2, ConcurrencyLimit: 5}

	st := BuildKeyCapacityState(configured, cached, nil)
	require.True(t, st.ConfigApplied, "缓存与库一致 = 已生效")
}

func TestBuildKeyCapacityState_CacheMissIsHonest(t *testing.T) {
	configured := &APIKey{ID: 1, UserID: 7, User: &User{ID: 7, Concurrency: 10}}

	// 缓存未命中：无法断言"已生效"，必须 false（诚实，不是宽容）
	st := BuildKeyCapacityState(configured, nil, nil)
	require.False(t, st.ConfigApplied, "缓存未命中不得声称已生效")
}

func TestBuildKeyCapacityState_PoolStricterThanKey(t *testing.T) {
	configured := &APIKey{
		ID: 1, UserID: 7, ConcurrencyLimit: 20,
		User: &User{ID: 7, Concurrency: 5},
	}
	st := BuildKeyCapacityState(configured, nil, nil)

	require.Equal(t, 5, st.EffectiveConcurrencyLimit, "池更严时取池")
	require.Equal(t, "user", st.LimitSource)
}

func TestBuildKeyCapacityState_Unlimited(t *testing.T) {
	configured := &APIKey{ID: 1, UserID: 7, User: &User{ID: 7, Concurrency: 0}}
	st := BuildKeyCapacityState(configured, nil, nil)
	require.Equal(t, 0, st.EffectiveConcurrencyLimit)
	require.Equal(t, "unlimited", st.LimitSource)
}
