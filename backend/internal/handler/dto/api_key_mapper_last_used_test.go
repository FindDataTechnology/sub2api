package dto

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyFromService_MapsLastUsedAt(t *testing.T) {
	lastUsed := time.Now().UTC().Truncate(time.Second)
	lastUsedIP := "203.0.113.10"
	src := &service.APIKey{
		ID:                 1,
		UserID:             2,
		Key:                "sk-map-last-used",
		Name:               "Mapper",
		Status:             service.StatusActive,
		LastUsedAt:         &lastUsed,
		LastUsedIP:         &lastUsedIP,
		CurrentConcurrency: 3,
	}

	out := APIKeyFromService(src)
	require.NotNil(t, out)
	require.NotNil(t, out.LastUsedAt)
	require.WithinDuration(t, lastUsed, *out.LastUsedAt, time.Second)
	require.NotNil(t, out.LastUsedIP)
	require.Equal(t, lastUsedIP, *out.LastUsedIP)
	require.Equal(t, 3, out.CurrentConcurrency)
}

func TestAPIKeyFromService_MapsNilLastUsedAt(t *testing.T) {
	src := &service.APIKey{
		ID:     1,
		UserID: 2,
		Key:    "sk-map-last-used-nil",
		Name:   "MapperNil",
		Status: service.StatusActive,
	}

	out := APIKeyFromService(src)
	require.NotNil(t, out)
	require.Nil(t, out.LastUsedAt)
	require.Nil(t, out.LastUsedIP)
}

// key 没设上限但 user 池是 5：必须回报 5/user，不能显示成"不限"
// —— 否则用户看到 0 会以为随便开并发，撞墙时归因不到自己头上。
func TestAPIKeyFromService_EffectiveLimitTakesStricterLayer(t *testing.T) {
	poolOnly := &service.APIKey{ID: 1, User: &service.User{ID: 2, Concurrency: 5}}
	out := APIKeyFromService(poolOnly)
	require.Equal(t, 5, out.EffectiveConcurrencyLimit)
	require.Equal(t, "user", out.LimitSource)

	stricterKey := &service.APIKey{
		ID: 1, ConcurrencyLimit: 3,
		User: &service.User{ID: 2, Concurrency: 5},
	}
	out = APIKeyFromService(stricterKey)
	require.Equal(t, 3, out.EffectiveConcurrencyLimit)
	require.Equal(t, "key", out.LimitSource)

	unlimited := &service.APIKey{ID: 1}
	out = APIKeyFromService(unlimited)
	require.Zero(t, out.EffectiveConcurrencyLimit)
	require.Equal(t, "unlimited", out.LimitSource)
}
