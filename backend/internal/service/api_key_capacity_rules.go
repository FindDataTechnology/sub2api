package service

import (
	"context"
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// 容量契约的写路径不变量（add-key-capacity-contract）。
//
// 为什么运行时闸之外还必须有这一层：闸只保证"用不超"，保证不了"配得对"。
// 池 5 被配成 3+3 时，闸照样正常工作（第三个 key 拿不到保底），但配置本身是错的，
// 而且**错得看不出来**——两个看起来都合规，合起来超了。所以写路径必须校验：
//
//	1) Σ concurrency_request（该用户全部活跃 key） <= 池容量
//	2) key.concurrency_limit <= 池容量
//	3) key.concurrency_request <= key.concurrency_limit（硬顶 > 0 时）
//
// 三条都返回 requested / remaining / capacity 三个数，便于调用方自己调整，
// **不静默截断**（静默取 min 会让用户以为拿到了 20，实际只有 5）。
//
// 说明：剩余额度用「池容量 − Σrequest」查询聚合得出，**不维护物化计数器**
// （漏减一次就永久漂移，且无自证手段）。

// CapacityInvariantError 描述一次容量不变量违反，供上层转成 400。
type CapacityInvariantError struct {
	Reason    string // 机器可读原因
	Message   string
	Requested int
	Remaining int
	Capacity  int
}

func (e *CapacityInvariantError) Error() string {
	return fmt.Sprintf("%s: %s (requested=%d remaining=%d capacity=%d)",
		e.Reason, e.Message, e.Requested, e.Remaining, e.Capacity)
}

// AsCapacityInvariant 让上层能识别这类错误（转 400 + 结构化字段）。
func AsCapacityInvariant(err error) (*CapacityInvariantError, bool) {
	var target *CapacityInvariantError
	if e, ok := err.(*CapacityInvariantError); ok {
		target = e
		return target, true
	}
	return nil, false
}

// clampNonNegative 把负余量折成 0（余量为负说明已超额，对调用方展示 0 更有意义）。
// capacityListPagination 用一个大页一次取全量：容量校验需要看到该用户所有活跃 key，
// 分页截断会导致漏算（少算别人的 request ⇒ 放过超额配置）。key 量级几十到几百，一页足够。
func capacityListPagination() pagination.PaginationParams {
	return pagination.PaginationParams{Page: 1, PageSize: 1000}
}

func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// validateCapacityFieldsShape 只做单 key 自身的形状校验（不涉及聚合）。
func validateCapacityFieldsShape(request, limit, rpm int) error {
	if request < 0 || limit < 0 || rpm < 0 {
		return infraerrors.BadRequest("API_KEY_CAPACITY_INVALID", "concurrency_request / concurrency_limit / rpm_limit must be non-negative")
	}
	if limit > 0 && request > limit {
		return &CapacityInvariantError{
			Reason:    "API_KEY_CAPACITY_REQUEST_EXCEEDS_LIMIT",
			Message:   "concurrency_request must not exceed concurrency_limit",
			Requested: request,
			Remaining: limit,
			Capacity:  limit,
		}
	}
	return nil
}

// checkCapacityAllocation 校验某个用户下的聚合不变量。
//
// excludeKeyID：更新时排除自身（它自己的旧 request 不应计入）。
// newRequest / newLimit：本次写入后的目标值（0 = 不限/不占席位）。
// poolCapacity：该用户池容量（User.Concurrency，0 = 不限池）。
func (s *APIKeyService) checkCapacityAllocation(
	ctx context.Context,
	userID int64,
	poolCapacity int,
	excludeKeyID int64,
	newRequest, newLimit int,
) error {
	if poolCapacity <= 0 {
		// 池不限：单 key 的上限也没有意义被池约束，但形状仍要合法。
		return nil
	}

	// 2) 硬顶不得超过池容量
	if newLimit > poolCapacity {
		return &CapacityInvariantError{
			Reason:    "API_KEY_CAPACITY_LIMIT_EXCEEDS_POOL",
			Message:   "concurrency_limit must not exceed the user's pool capacity",
			Requested: newLimit,
			Remaining: poolCapacity,
			Capacity:  poolCapacity,
		}
	}

	// 1) Σ request（其他活跃 key）+ 本次 request <= 池容量
	// 用分页接口取全量：key 数量级为几十到几百，一次足够。
	keys, _, err := s.apiKeyRepo.ListByUserID(ctx, userID, capacityListPagination(), APIKeyListFilters{})
	if err != nil {
		return fmt.Errorf("list api keys for capacity check: %w", err)
	}
	used := 0
	for i := range keys {
		k := &keys[i]
		if k.ID == excludeKeyID {
			continue
		}
		// 只统计"活跃"的 key：已删除/停用的不占额度（与运行时判定一致）。
		if k.Status != StatusActive {
			continue
		}
		used += k.ConcurrencyRequest
	}
	remaining := poolCapacity - used
	if newRequest > remaining {
		return &CapacityInvariantError{
			Reason:    "API_KEY_CAPACITY_REQUEST_EXCEEDS_POOL",
			Message:   "sum of concurrency_request across active keys would exceed the user's pool capacity",
			Requested: newRequest,
			Remaining: clampNonNegative(remaining),
			Capacity:  poolCapacity,
		}
	}
	return nil
}

// KeyCapacityState 是 state 端点要回报的东西：**库里的配置 vs 鉴权链实际看到的快照**。
//
// 为什么需要它：认证快照有 L1(15s)/L2(300s) 两级缓存。改完 concurrency 后，
// 运维无法判断"改的生效了吗"——只能等 15 秒然后猜。有了这个端点就能直接看：
//
//	config_applied=false ⇒ 还在旧快照，别急着验证行为
//	config_applied=true  ⇒ 已经生效
//
// 诚实边界：v1 只能判断"该 key 的快照是否已被重建"，不能保证所有实例都已切换
// （生产单副本时等价；扩副本后需改为回报实例数）。
type KeyCapacityState struct {
	KeyID int64 `json:"key_id"`

	// 库里的权威配置
	ConfiguredConcurrencyRequest int    `json:"configured_concurrency_request"`
	ConfiguredConcurrencyLimit   int    `json:"configured_concurrency_limit"`
	ConfiguredRPMLimit           int    `json:"configured_rpm_limit"`
	PoolCapacity                 int    `json:"pool_capacity"` // user.Concurrency，0=不限
	EffectiveConcurrencyLimit    int    `json:"effective_concurrency_limit"`
	EffectiveRPMLimit            int    `json:"effective_rpm_limit"`
	LimitSource                  string `json:"limit_source"`

	// 运行时
	UserInflight int64 `json:"user_inflight"`
	KeyInflight  int64 `json:"key_inflight"`
	LeakedSlots  int64 `json:"leaked_slots_recovered"` // 对账回收过的槽位总数（泄漏要可见）

	// 快照状态
	ObservedSnapshotVersion int  `json:"observed_snapshot_version"`
	ConfigApplied           bool `json:"config_applied"`

	CountBackend string `json:"count_backend"` // in_process | shared（扩副本后应变）
}

// BuildKeyCapacityState 组装状态视图。
//
//	configured: 从库读到的 key（权威）
//	cached:     从鉴权缓存读到的 key（鉴权链实际看到的值）；nil 表示缓存未命中
//	gate:       容量闸，用于回报运行时计数；可为 nil
func BuildKeyCapacityState(configured, cached *APIKey, gate *KeyCapacityGate) *KeyCapacityState {
	if configured == nil {
		return nil
	}
	st := &KeyCapacityState{
		KeyID:                        configured.ID,
		ConfiguredConcurrencyRequest: configured.ConcurrencyRequest,
		ConfiguredConcurrencyLimit:   configured.ConcurrencyLimit,
		ConfiguredRPMLimit:           configured.RPMLimit,
		CountBackend:                 "in_process",
	}
	poolCap := 0
	userRPM := 0
	if configured.User != nil {
		poolCap = configured.User.Concurrency
		userRPM = configured.User.RPMLimit
	}
	st.PoolCapacity = poolCap
	st.EffectiveConcurrencyLimit, st.LimitSource = effectiveLimitForState(configured.ConcurrencyLimit, poolCap)
	st.EffectiveRPMLimit, _ = effectiveLimitForState(configured.RPMLimit, userRPM)

	if gate != nil {
		userID := configured.UserID
		if configured.User != nil {
			userID = configured.User.ID
		}
		st.UserInflight, st.KeyInflight, st.LeakedSlots = gate.CapacitySnapshot(userID, configured.ID)
	}

	// config_applied：缓存里看到的配置与库里一致即视为已生效
	if cached != nil {
		st.ConfigApplied = cached.ConcurrencyRequest == configured.ConcurrencyRequest &&
			cached.ConcurrencyLimit == configured.ConcurrencyLimit &&
			cached.RPMLimit == configured.RPMLimit
	}
	return st
}

// effectiveLimitForState 与 lanes 包的 effectiveLimit 同语义（取更严者）。
// 这里单独一份是为了让 service 不依赖 lanes（lanes 依赖 middleware，会成环）。
func effectiveLimitForState(keyLimit, poolLimit int) (int, string) {
	switch {
	case keyLimit > 0 && poolLimit > 0:
		if keyLimit <= poolLimit {
			return keyLimit, "key"
		}
		return poolLimit, "user"
	case keyLimit > 0:
		return keyLimit, "key"
	case poolLimit > 0:
		return poolLimit, "user"
	default:
		return 0, "unlimited"
	}
}

// GetKeyCapacityState 读「库里配置」与「鉴权缓存实际看到的快照」，组装 state 视图。
//
// 缓存未命中时 cached=nil，ConfigApplied 保持 false —— 这是诚实的：
// 未命中说明鉴权链下次会回源重建，此刻无法断言"已生效"。
func (s *APIKeyService) GetKeyCapacityState(ctx context.Context, keyID int64, gate *KeyCapacityGate) (*KeyCapacityState, error) {
	configured, err := s.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if configured == nil {
		return nil, ErrAPIKeyNotFound
	}

	// 从鉴权缓存取当前生效的快照（只读，不触发重建）
	var cached *APIKey
	if entry, cerr := s.loadAuthCacheEntry(ctx, configured.Key, s.authCacheKey(configured.Key)); cerr == nil && entry != nil {
		if probe, used, aerr := s.applyAuthCacheEntry(configured.Key, entry); aerr == nil && used {
			cached = probe
		}
	}

	return BuildKeyCapacityState(configured, cached, gate), nil
}
