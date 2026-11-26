package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

// 写路径不变量（add-key-capacity-contract）：形状校验 + 聚合校验。
// 运行时闸保证"用不超"，这一层保证"配得对"——池 5 被配成 3+3 时闸不会报错，
// 但配置本身是错的，而且错得看不出来。所以这层必须有测试。

type capacityKeysStub struct {
	APIKeyRepository
	keys []APIKey
}

func (s *capacityKeysStub) ListByUserID(_ context.Context, _ int64, _ pagination.PaginationParams, _ APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
	return s.keys, nil, nil
}

func newCapacityService(keys ...APIKey) *APIKeyService {
	return &APIKeyService{apiKeyRepo: &capacityKeysStub{keys: keys}}
}

func TestValidateCapacityFieldsShape(t *testing.T) {
	require.NoError(t, validateCapacityFieldsShape(0, 0, 0), "全 0 = 不限，合法")
	require.NoError(t, validateCapacityFieldsShape(3, 5, 0))
	require.NoError(t, validateCapacityFieldsShape(5, 5, 0), "request == limit 合法（严格独占）")

	require.Error(t, validateCapacityFieldsShape(-1, 0, 0), "负值非法")
	require.Error(t, validateCapacityFieldsShape(0, -1, 0))
	require.Error(t, validateCapacityFieldsShape(0, 0, -1))

	err := validateCapacityFieldsShape(6, 5, 0)
	require.Error(t, err, "request > limit 必须被拒")
	var inv *CapacityInvariantError
	inv, ok := AsCapacityInvariant(err)
	require.True(t, ok)
	require.Equal(t, 6, inv.Requested)
	require.Equal(t, 5, inv.Capacity)
}

func TestCheckCapacityAllocation_PoolUnlimitedSkipsAggregate(t *testing.T) {
	s := newCapacityService()
	// 池 0 = 不限：聚合不校验（但形状仍由上一层管）
	require.NoError(t, s.checkCapacityAllocation(context.Background(), 1, 0, 0, 999, 999))
}

func TestCheckCapacityAllocation_LimitCannotExceedPool(t *testing.T) {
	s := newCapacityService()

	err := s.checkCapacityAllocation(context.Background(), 1, 5, 0, 0, 10)
	require.Error(t, err, "硬顶超池必须被拒")
	inv, ok := AsCapacityInvariant(err)
	require.True(t, ok)
	require.Equal(t, "API_KEY_CAPACITY_LIMIT_EXCEEDS_POOL", inv.Reason)
	require.Equal(t, 10, inv.Requested)
	require.Equal(t, 5, inv.Remaining)
}

func TestCheckCapacityAllocation_SumOfRequestsCannotExceedPool(t *testing.T) {
	// 池 5，已有 3 与 2，第三个再要 1 => 总额 6 > 5，必须拒
	s := newCapacityService(
		APIKey{ID: 1, Status: StatusActive, ConcurrencyRequest: 3},
		APIKey{ID: 2, Status: StatusActive, ConcurrencyRequest: 2},
	)

	err := s.checkCapacityAllocation(context.Background(), 1, 5, 0, 1, 0)
	require.Error(t, err)
	inv, ok := AsCapacityInvariant(err)
	require.True(t, ok)
	require.Equal(t, "API_KEY_CAPACITY_REQUEST_EXCEEDS_POOL", inv.Reason)
	require.Equal(t, 1, inv.Requested)
	require.Equal(t, 0, inv.Remaining, "余量应为 0")
	require.Equal(t, 5, inv.Capacity)

	// 但要 0（不占席位）是合法的
	require.NoError(t, s.checkCapacityAllocation(context.Background(), 1, 5, 0, 0, 0))
}

func TestCheckCapacityAllocation_ExcludesSelfOnUpdate(t *testing.T) {
	// 更新 key 2 自身：它旧的 2 不应与新的 3 叠加（排除后总额 = 3 + 3 = 6 > 5 仍应拒）
	s := newCapacityService(
		APIKey{ID: 1, Status: StatusActive, ConcurrencyRequest: 3},
		APIKey{ID: 2, Status: StatusActive, ConcurrencyRequest: 2},
	)
	err := s.checkCapacityAllocation(context.Background(), 1, 5, 2, 3, 0)
	require.Error(t, err, "3 + 3 > 5")

	// 但改成 2 应合法（排除自身后 3 + 2 = 5）
	require.NoError(t, s.checkCapacityAllocation(context.Background(), 1, 5, 2, 2, 0))
}

func TestCheckCapacityAllocation_InactiveKeysDoNotCount(t *testing.T) {
	// 停用/禁用的 key 不占额度（与运行时判定一致）
	s := newCapacityService(
		APIKey{ID: 1, Status: StatusActive, ConcurrencyRequest: 3},
		APIKey{ID: 2, Status: StatusDisabled, ConcurrencyRequest: 2},
	)
	require.NoError(t, s.checkCapacityAllocation(context.Background(), 1, 5, 0, 2, 0),
		"停用的 key 不占额度，3 + 2(=新) 仍 ≤ 5")
}

// ── 2b.3 并发超卖：分配不变量必须串行 ──────────────────────────────
//
// 这是容量契约里最容易"看起来对、实际错"的一处：校验读的是"其他 key 的 request 之和"，
// 写的是"本 key 的 request"。两个并发请求各自读到旧和、各自判定合法、各自写入
// ⇒ 合计超池。锁 user 行把它们串成一条队才能真正防住。
//
// 本测试用内存桩模拟"加锁 = 串行执行"，断言 20 个并发分配后 Σrequest 仍 ≤ 池。

type serializedUserRepoStub struct {
	UserRepository
	mu     sync.Mutex
	locked bool
	// violations 记录观察到"同时有两个 fn 在跑"的次数；锁生效时应恒为 0。
	violations int32
}

func (s *serializedUserRepoStub) GetByID(_ context.Context, _ int64) (*User, error) {
	return &User{ID: 1, Concurrency: 5}, nil
}

func (s *serializedUserRepoStub) WithUserRowLock(ctx context.Context, _ int64, fn func(context.Context) error) error {
	s.mu.Lock()
	if s.locked {
		atomic.AddInt32(&s.violations, 1)
	}
	s.locked = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.locked = false
		s.mu.Unlock()
	}()
	return fn(ctx)
}

// capacityAllocRepo 记录已被"分配"的 request 总量，模拟库中 Σrequest。
type capacityAllocRepo struct {
	APIKeyRepository
	mu        sync.Mutex
	allocated int
}

func (r *capacityAllocRepo) ListByUserID(_ context.Context, _ int64, _ pagination.PaginationParams, _ APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 把已分配量摊成若干把 key 返回
	keys := make([]APIKey, 0, r.allocated)
	for i := 0; i < r.allocated; i++ {
		keys = append(keys, APIKey{ID: int64(i + 1), Status: StatusActive, ConcurrencyRequest: 1})
	}
	return keys, nil, nil
}

func (r *capacityAllocRepo) Create(_ context.Context, _ *APIKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allocated++
	return nil
}

func TestCheckCapacityAllocation_ConcurrentCreateDoesNotOversell(t *testing.T) {
	userRepo := &serializedUserRepoStub{}
	keyRepo := &capacityAllocRepo{}
	svc := &APIKeyService{apiKeyRepo: keyRepo, userRepo: userRepo}

	const workers = 20 // 池只有 5，必然绝大多数被拒
	var wg sync.WaitGroup
	var granted int32
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := svc.withUserRowLock(context.Background(), 1, func(txCtx context.Context) error {
				if aerr := svc.checkCapacityAllocation(txCtx, 1, 5, 0, 1, 0); aerr != nil {
					return aerr
				}
				return svc.apiKeyRepo.Create(txCtx, &APIKey{ID: 1, Status: StatusActive, ConcurrencyRequest: 1})
			})
			if err == nil {
				atomic.AddInt32(&granted, 1)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, int32(5), atomic.LoadInt32(&granted), "池 5 只能放行 5 个分配")
	require.Equal(t, 5, keyRepo.allocated, "写入数必须等于放行数")
	require.Zero(t, atomic.LoadInt32(&userRepo.violations), "锁生效时不应观察到最后两个 fn 并发")
}
