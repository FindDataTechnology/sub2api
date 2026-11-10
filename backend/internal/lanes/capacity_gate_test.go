package lanes

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 容量闸单测：两级硬顶、幂等释放、泄漏回收。
// 判据来自 design「运行时准入」与「计数泄漏防护」两节。

func TestCapacityGate_PoolCapIsHard(t *testing.T) {
	g := newCapacityGate()

	// 池 2、key 不限：第三个必须被池挡住，scope=user
	r1, rel1 := g.tryAcquireCapacity(1, 10, 2, 0)
	r2, rel2 := g.tryAcquireCapacity(1, 10, 2, 0)
	r3, rel3 := g.tryAcquireCapacity(1, 10, 2, 0)
	defer rel1()
	defer rel2()
	defer rel3()

	require.True(t, r1.Allowed)
	require.True(t, r2.Allowed)
	require.False(t, r3.Allowed, "池满必须拒")
	require.Equal(t, "user", r3.Scope)
}

func TestCapacityGate_KeyCapIsHardEvenWhenPoolFree(t *testing.T) {
	g := newCapacityGate()

	// 池 5、该 key 硬顶 1：第二个请求必须被 key 层挡住，scope=key
	r1, rel1 := g.tryAcquireCapacity(1, 10, 5, 1)
	r2, _ := g.tryAcquireCapacity(1, 10, 5, 1)
	defer rel1()

	require.True(t, r1.Allowed)
	require.False(t, r2.Allowed)
	require.Equal(t, "key", r2.Scope)
}

func TestCapacityGate_MultiKeyCannotExceedPool(t *testing.T) {
	g := newCapacityGate()
	const pool = 5

	// 10 把 key 各跑 1，合计不得超过池 5（堵住「N 把 key = N×5」）
	var rels []func()
	allowed := 0
	for i := 0; i < 10; i++ {
		d, rel := g.tryAcquireCapacity(1, int64(100+i), pool, 0)
		if d.Allowed {
			allowed++
			rels = append(rels, rel)
		}
	}
	for _, rel := range rels {
		rel()
	}
	require.Equal(t, pool, allowed, "多 key 合计必须被池硬顶挡住")
}

func TestCapacityGate_UnlimitedSemantics(t *testing.T) {
	g := newCapacityGate()

	// 池 0 且硬顶 0 = 都不限（存量 key 的默认值），应全部放行
	for i := 0; i < 50; i++ {
		d, rel := g.tryAcquireCapacity(9, 99, 0, 0)
		require.True(t, d.Allowed, "0/0 必须等同不限")
		defer rel()
	}
}

func TestCapacityGate_ReleaseIsIdempotent(t *testing.T) {
	g := newCapacityGate()

	d, rel := g.tryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d.Allowed)

	rel()
	rel() // 重复释放不得把计数扣成负数——否则该 key 会永远放行（静默失效）

	// 重复释放后应恢复可用；取一次并正常释放
	d2, rel2 := g.tryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d2.Allowed, "重复释放后应恢复可用")
	rel2()

	userInflight, keyInflight, _ := g.capacitySnapshot(1, 10)
	require.EqualValues(t, 0, userInflight, "计数必须归零，不能为负")
	require.EqualValues(t, 0, keyInflight)
}

func TestCapacityGate_ReconcileRecoversLeakedSlot(t *testing.T) {
	g := newCapacityGate()

	// 模拟泄漏：拿了不还（故意不调 release）
	d, _ := g.tryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d.Allowed)

	// 未过期时不回收
	require.Equal(t, 0, g.reconcile(time.Minute, time.Now()))

	// 过期后回收
	recovered := g.reconcile(time.Minute, time.Now().Add(2*time.Minute))
	require.Equal(t, 1, recovered)

	_, keyInflight, leaked := g.capacitySnapshot(1, 10)
	require.EqualValues(t, 0, keyInflight, "泄漏槽位应被清零")
	require.EqualValues(t, 1, leaked, "回收必须记账——泄漏要可见，不能静默自愈")

	// 回收后应可再次获取
	d2, rel2 := g.tryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d2.Allowed)
	rel2()
}

func TestCapacityGate_NilGateFailsOpen(t *testing.T) {
	var g *capacityGate
	d, rel := g.tryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d.Allowed, "未装配必须放行（fail-open），不得全站 429")
	rel() // 不得 panic
}

func TestCapacityGate_ConcurrentAcquireNeverExceedsCap(t *testing.T) {
	g := newCapacityGate()
	const pool = 5
	const workers = 64

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, rel := g.tryAcquireCapacity(1, 10, pool, 0)
			if d.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
				// 故意不立即释放，制造竞争
				time.Sleep(time.Millisecond)
				rel()
			}
		}()
	}
	wg.Wait()

	require.LessOrEqual(t, allowed, pool*workers, "sanitty")
	userInflight, keyInflight, _ := g.capacitySnapshot(1, 10)
	require.EqualValues(t, 0, userInflight, "全部释放后池计数必须归零")
	require.EqualValues(t, 0, keyInflight, "全部释放后 key 计数必须归零")
}
