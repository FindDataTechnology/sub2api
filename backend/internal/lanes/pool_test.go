package lanes

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// 池单测：lane→平台映射、登记面、候选语义（v1.1 Candidates 全列表 priority
// ASC）、冷却写（分级时长+节流+best-effort）。
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
		// 镜像 repo 谓词：status=active（schedulable/冷却过滤由真 repo 承担，
		// Candidates 不重复过滤——冷却生效=账号从列表消失，见 Candidates 注释）。
		if a.Platform == platform && a.Status == service.StatusActive {
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

// ListSchedulableByPlatforms 多平台取数（audio 跨协议候选）：与单平台同一过滤
// 与排序口径，仅放宽平台条件。
func (f *fakeLaneAccountLister) ListSchedulableByPlatforms(_ context.Context, platforms []string) ([]service.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	want := make(map[string]struct{}, len(platforms))
	for _, p := range platforms {
		want[p] = struct{}{}
	}
	var out []service.Account
	for _, a := range f.accounts {
		if _, ok := want[a.Platform]; ok && a.Status == service.StatusActive {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// fakeCooldownWriter 冷却写桩：记录 (id, until, reason) 供断言。
type fakeCooldownWriter struct {
	mu      sync.Mutex
	writes  map[int64][]time.Duration // id -> durations（相对写入时刻）
	reasons map[int64]string
}

func newFakeCooldownWriter() *fakeCooldownWriter {
	return &fakeCooldownWriter{writes: map[int64][]time.Duration{}, reasons: map[int64]string{}}
}

func (f *fakeCooldownWriter) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes[id] = append(f.writes[id], time.Until(until))
	f.reasons[id] = reason
	return nil
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

func TestPoolCandidates(t *testing.T) {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 2, Platform: domain.PlatformSerpAPI, Status: service.StatusActive, Priority: 10},
		// priority 更小者先调度（repo ORDER BY priority ASC，fake 保持同序）
		{ID: 1, Platform: domain.PlatformSerpAPI, Status: service.StatusActive, Priority: 1},
		{ID: 3, Platform: domain.PlatformSerpAPI, Status: service.StatusDisabled, Priority: 0},
		{ID: 4, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive},
	}}
	pool := NewPool(lister, nil)

	// v1.1：全候选列表（轮转循环在适配器层），序=priority ASC。
	accs, err := pool.Candidates(context.Background(), domain.PlatformSerpAPI)
	require.NoError(t, err)
	require.Len(t, accs, 2)
	require.Equal(t, int64(1), accs[0].ID)
	require.Equal(t, int64(2), accs[1].ID)

	accs, err = pool.Candidates(context.Background(), domain.PlatformSiliconFlow)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	require.Equal(t, int64(4), accs[0].ID)
}

func TestPoolCandidatesEmptyAndErrors(t *testing.T) {
	var nilPool *Pool
	_, err := nilPool.Candidates(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount)

	emptyLister := &fakeLaneAccountLister{}
	_, err = NewPool(emptyLister, nil).Candidates(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount)

	_, err = NewPool(nil, nil).Candidates(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount)

	dbErr := errors.New("db down")
	_, err = NewPool(&fakeLaneAccountLister{err: dbErr}, nil).Candidates(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, dbErr)

	_, err = NewPool(&fakeLaneAccountLister{accounts: []service.Account{
		{ID: 9, Platform: domain.PlatformSerpAPI, Status: service.StatusDisabled},
	}}, nil).Candidates(context.Background(), domain.PlatformSerpAPI)
	require.ErrorIs(t, err, ErrNoLaneAccount) // 全停用=池空
}

// 冷却写：时长透传 + 同账号 60s 节流（第二笔被吞）+ 不同账号互不影响。
func TestPoolCooldownThrottle(t *testing.T) {
	writer := newFakeCooldownWriter()
	pool := NewPool(nil, writer)

	pool.CooldownAccount(1, laneCooldownExhausting, "quota")
	pool.CooldownAccount(1, laneCooldownTransient, "transient-soon") // 节流窗内：被吞
	pool.CooldownAccount(2, laneCooldownTransient, "transient")

	require.Len(t, writer.writes[1], 1)
	require.Len(t, writer.writes[2], 1)
	require.InDelta(t, laneCooldownExhausting.Seconds(), writer.writes[1][0].Seconds(), 1.0)
	require.InDelta(t, laneCooldownTransient.Seconds(), writer.writes[2][0].Seconds(), 1.0)
	require.Contains(t, writer.reasons[1], "quota")

	// 非法入参直接忽略。
	pool.CooldownAccount(0, laneCooldownTransient, "bad-id")
	pool.CooldownAccount(1, 0, "bad-duration")
	var nilPool *Pool
	nilPool.CooldownAccount(1, laneCooldownTransient, "nil-pool")
	require.Len(t, writer.writes[0], 0)
	require.Len(t, writer.writes[1], 1) // 仍是 1：后两笔没生效
}

func TestCandidatesForPlatforms_MergesAcrossProtocolsInPlatformOrder(t *testing.T) {
	// audio 跨协议取候选：按给定平台序归并（同平台内 priority ASC），
	// 使「首选平台优先」成为确定序，失败轮转才有可预期行为。
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 3, Platform: "tencent", Status: service.StatusActive, Priority: 1},
		{ID: 2, Platform: "doubao", Status: service.StatusActive, Priority: 5},
		{ID: 1, Platform: "openainext", Status: service.StatusActive, Priority: 9},
		{ID: 4, Platform: "doubao", Status: service.StatusActive, Priority: 1},
	}}
	pool := NewPool(lister, nil)
	got, err := pool.CandidatesForPlatforms(context.Background(), []string{"openainext", "doubao", "tencent"})
	if err != nil {
		t.Fatalf("CandidatesForPlatforms: %v", err)
	}
	ids := make([]int64, len(got))
	for i, a := range got {
		ids[i] = a.ID
	}
	// openainext 先，然后 doubao（priority 1 先于 5），最后 tencent
	want := []int64{1, 4, 2, 3}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v (platform order must dominate)", ids, want)
		}
	}
}

func TestCandidatesForPlatforms_EmptyWhenNoAccounts(t *testing.T) {
	pool := NewPool(&fakeLaneAccountLister{}, nil)
	if _, err := pool.CandidatesForPlatforms(context.Background(), []string{"doubao", "tencent"}); err == nil {
		t.Fatal("expected ErrNoLaneAccount for empty pool")
	}
}
