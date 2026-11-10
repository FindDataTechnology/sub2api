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
