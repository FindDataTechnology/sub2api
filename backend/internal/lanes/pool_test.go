package lanes

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// 池单测（tasks 2.4）：lane→平台映射、登记面、选择语义（priority ASC 首个
// active；非 active 跳过；空池/nil 池=ErrNoLaneAccount）。
type fakeLaneAccountLister struct {
	service.AccountRepository
	accounts []service.Account
	err      error
}

func (f *fakeLaneAccountLister) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []service.Account
	for _, a := range f.accounts {
		if a.Platform == platform {
			out = append(out, a)
		}
	}
	// 镜像 repo 口径：ORDER BY priority ASC, id ASC。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func TestLanePlatformMapping(t *testing.T) {
	require.Equal(t, domain.PlatformOpenAINext, LanePlatform(LaneAudio))
	require.Equal(t, domain.PlatformSiliconFlow, LanePlatform(LaneRerank))
	require.Equal(t, domain.PlatformSerpAPI, LanePlatform(LaneWebsearch))
	require.Equal(t, "", LanePlatform("unknown"))
}

// 车道上游平台必须走 platformList 登记（台账§二「平台清单」）：账号管理面
// 下拉即达、IsConcretePlatform/分组校验等派生面一致生效。
func TestLanePlatformsRegistered(t *testing.T) {
	for _, id := range []string{domain.PlatformSiliconFlow, domain.PlatformSerpAPI, domain.PlatformOpenAINext} {
		require.Truef(t, domain.IsConcretePlatform(id), "%s should be registered in platformList", id)
		spec, ok := domain.LookupPlatform(id)
		require.Truef(t, ok, "%s should be lookup-able", id)
		require.Equalf(t, domain.PlatformGatewayLane, spec.Gateway, "%s must not be claimed by chat gateways", id)
		require.Falsef(t, domain.UsesOpenAIGateway(id), "%s must not route to OpenAI gateway", id)
		require.NotEmptyf(t, spec.DisplayName, "%s needs a display name for admin UI", id)
	}
}

func TestPoolPick(t *testing.T) {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 2, Platform: domain.PlatformSerpAPI, Status: service.StatusActive, Priority: 10},
		// priority 更小者先调度（repo ORDER BY priority ASC，fake 保持同序）
		{ID: 1, Platform: domain.PlatformSerpAPI, Status: service.StatusActive, Priority: 1},
		{ID: 3, Platform: domain.PlatformSerpAPI, Status: service.StatusDisabled, Priority: 0},
		{ID: 4, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive},
	}}
	pool := NewPool(lister)

	acc, err := pool.Pick(context.Background(), domain.PlatformSerpAPI)
	require.NoError(t, err)
	require.Equal(t, int64(1), acc.ID) // 首个 active（paused 的 priority=0 被跳过）

	acc, err = pool.Pick(context.Background(), domain.PlatformSiliconFlow)
	require.NoError(t, err)
	require.Equal(t, int64(4), acc.ID)
}

func TestPoolPickEmptyAndErrors(t *testing.T) {
	var nilPool *Pool
	_, err := nilPool.Pick(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount)

	emptyLister := &fakeLaneAccountLister{}
	_, err = NewPool(emptyLister).Pick(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount)

	_, err = NewPool(nil).Pick(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount)

	dbErr := errors.New("db down")
	_, err = NewPool(&fakeLaneAccountLister{err: dbErr}).Pick(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, dbErr)

	_, err = NewPool(&fakeLaneAccountLister{accounts: []service.Account{
		{ID: 9, Platform: domain.PlatformSerpAPI, Status: service.StatusDisabled},
	}}).Pick(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount) // 全停用=池空
}
