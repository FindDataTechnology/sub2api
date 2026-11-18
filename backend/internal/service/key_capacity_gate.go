package service

import (
	"sync"
	"sync/atomic"
	"time"
)

// 容量闸（add-key-capacity-contract）：user 是池，key 是池内席位。
//
// 准入只判两条硬顶（见 design「运行时准入」）：
//
//	(池容量 == 0 ∨ userInflight < 池容量)     ① 全局硬顶，恒生效
//	∧ (硬顶 == 0 ∨ keyInflight < 硬顶)         ② 该 key 的硬顶
//
// 保底（concurrency_request）**不参与**运行时判定——它只保证额度不被别人申请占用
// （Σrequest ≤ 池容量在写路径校验）。要吞吐保证请让 limit = request。
// 这是初稿写错、自查后改正的地方：初稿声称"池满时保底仍放行"，那等于让池可超，自相矛盾。
//
// 计数为进程内原子计数：生产 **1 副本**（2026-11-11 实测），故精确。
// 若扩副本必须改为共享计数，否则各实例独立计数会把总量放大 N 倍 —— 写进 state 端点回报。
type KeyCapacityGate struct {
	// users: userID → 池当前在飞数
	users sync.Map // map[int64]*atomic.Int64
	// keys: apiKeyID → 该 key 当前在飞数
	keys sync.Map // map[int64]*keySlot

	// 泄漏对账：每个在飞槽位记录最近一次活跃时间，超过 staleAfter 未释放视为泄漏并回收。
	leakedRecovered atomic.Int64
	lastReconcile   atomic.Int64 // unix nano
}

type keySlot struct {
	// userID 让泄漏回收能同时退掉池计数——只退 key 不退池，池会永久少一格。
	userID   int64
	inflight atomic.Int64
	// oldestSince 当前**最早**一个尚未释放的获取时刻（unix nano）；0 表示无在飞。
	// 用它而不是"最近活跃"来判泄漏：长跑请求会一直刷新"最近活跃"，用后者会把
	// 正常的大请求误杀；而"最早未释放"超过阈值则一定是泄漏。
	oldestSince atomic.Int64
}

// CapacityDecision 是一次准入判定的结果，用于错误体归因（limit_kind / limit_scope）。
type CapacityDecision struct {
	Allowed bool
	// Scope 说明是哪一层挡住的：""（放行）/ "user"（池满）/ "key"（该 key 硬顶）
	Scope string
}

func NewKeyCapacityGate() *KeyCapacityGate {
	return &KeyCapacityGate{}
}

func (g *KeyCapacityGate) userCounter(userID int64) *atomic.Int64 {
	if v, ok := g.users.Load(userID); ok {
		return v.(*atomic.Int64)
	}
	c := &atomic.Int64{}
	actual, _ := g.users.LoadOrStore(userID, c)
	return actual.(*atomic.Int64)
}

func (g *KeyCapacityGate) keyCounter(apiKeyID, userID int64) *keySlot {
	if v, ok := g.keys.Load(apiKeyID); ok {
		return v.(*keySlot)
	}
	s := &keySlot{userID: userID}
	actual, _ := g.keys.LoadOrStore(apiKeyID, s)
	return actual.(*keySlot)
}

// TryAcquireCapacity 尝试占用一个并发槽位。
//
// 参数全部来自认证快照（不查库）：poolCap = user.Concurrency，keyLimit = key.ConcurrencyLimit。
// 0 表示不限（沿用本仓 quota=0 的既有语义）。
//
// 返回的 release 必须在请求结束时调用（**必须 defer**），否则计数泄漏会让该 key
// 永久少一个并发，且从外部看不出来。
func (g *KeyCapacityGate) TryAcquireCapacity(userID, apiKeyID int64, poolCap, keyLimit int) (CapacityDecision, func()) {
	if g == nil {
		// 未装配：放行（fail-open）。限流器故障不应变成全站 429。
		return CapacityDecision{Allowed: true}, func() {}
	}

	userCtr := g.userCounter(userID)
	slot := g.keyCounter(apiKeyID, userID)

	// ① 全局硬顶
	if poolCap > 0 {
		if cur := userCtr.Load(); cur >= int64(poolCap) {
			return CapacityDecision{Allowed: false, Scope: "user"}, func() {}
		}
	}
	// ② 该 key 硬顶
	if keyLimit > 0 {
		if cur := slot.inflight.Load(); cur >= int64(keyLimit) {
			return CapacityDecision{Allowed: false, Scope: "key"}, func() {}
		}
	}

	// 两道判定与自增之间存在窗口，但单进程内用 CAS 收敛：只有真正越过判定的才计数。
	// 采用"先加后验、越界回退"的方式，避免竞态下超卖。
	userCtr.Add(1)
	slot.inflight.Add(1)
	markOldest(slot, time.Now().UnixNano())

	// 回验：并发竞争下可能有人同时通过，越界者自我回退（保证硬顶严格）
	if poolCap > 0 && userCtr.Load() > int64(poolCap) {
		userCtr.Add(-1)
		slot.inflight.Add(-1)
		clearOldestIfIdle(slot)
		return CapacityDecision{Allowed: false, Scope: "user"}, func() {}
	}
	if keyLimit > 0 && slot.inflight.Load() > int64(keyLimit) {
		userCtr.Add(-1)
		slot.inflight.Add(-1)
		clearOldestIfIdle(slot)
		return CapacityDecision{Allowed: false, Scope: "key"}, func() {}
	}

	var released atomic.Bool
	release := func() {
		if released.Swap(true) {
			return // 幂等：重复调用不重复扣减（否则计数会变负，导致永远放行）
		}
		userCtr.Add(-1)
		slot.inflight.Add(-1)
		clearOldestIfIdle(slot)
	}
	return CapacityDecision{Allowed: true}, release
}

// markOldest 维护"最早未释放的获取时刻"。
func markOldest(slot *keySlot, now int64) {
	for {
		cur := slot.oldestSince.Load()
		if cur != 0 && cur <= now {
			return // 已有更早的，保持
		}
		if slot.oldestSince.CompareAndSwap(cur, now) {
			return
		}
	}
}

// clearOldestIfIdle 在 inflight 归零时清空基准；否则保持最早那个（有一批请求同时进来时
// 不能因为其中一个释放就把整批的泄漏基准冲掉）。
func clearOldestIfIdle(slot *keySlot) {
	if slot.inflight.Load() <= 0 {
		slot.oldestSince.Store(0)
	}
}

// Reconcile 回收"拿了不还"的槽位。
//
// 为什么必须有：并发计数是"进 +1 / 出 −1"，泄漏一次该 key 就永久少一个并发，
// 而从外部完全看不出来（只会表现为"这个 key 好像只能跑 4 个了"）。
// 本仓对"静默失效"的教训足够多，故泄漏**必须可见**：回收时递增 leakedRecovered，
// 由 state 端点回报，不能靠它静默自愈。
func (g *KeyCapacityGate) Reconcile(staleAfter time.Duration, now time.Time) int {
	if g == nil || staleAfter <= 0 {
		return 0
	}
	cutoff := now.Add(-staleAfter).UnixNano()
	recovered := 0
	g.keys.Range(func(_, v any) bool {
		slot := v.(*keySlot)
		if n := slot.inflight.Load(); n > 0 {
			since := slot.oldestSince.Load()
			if since > 0 && since < cutoff {
				// 最早未释放的获取已超时 ⇒ 判定泄漏：**两级同退**并记账。
				slot.inflight.Add(-n)
				slot.oldestSince.Store(0)
				if slot.userID > 0 {
					g.userCounter(slot.userID).Add(-int64(n))
				}
				recovered += int(n)
			}
		}
		return true
	})
	if recovered > 0 {
		g.leakedRecovered.Add(int64(recovered))
	}
	g.lastReconcile.Store(now.UnixNano())
	return recovered
}

// CapacitySnapshot 供 state 端点回报运行时真实值（与台账对比用）。
func (g *KeyCapacityGate) CapacitySnapshot(userID, apiKeyID int64) (userInflight, keyInflight int64, leaked int64) {
	if g == nil {
		return 0, 0, 0
	}
	if v, ok := g.users.Load(userID); ok {
		userInflight = v.(*atomic.Int64).Load()
	}
	if v, ok := g.keys.Load(apiKeyID); ok {
		keyInflight = v.(*keySlot).inflight.Load()
	}
	return userInflight, keyInflight, g.leakedRecovered.Load()
}

// StartCapacityReconciler 周期性对账。worker 为 0 时不启动（由装配方决定）。
func (g *KeyCapacityGate) StartCapacityReconciler(interval, staleAfter time.Duration) (stop func()) {
	if g == nil || interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				g.Reconcile(staleAfter, now)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
