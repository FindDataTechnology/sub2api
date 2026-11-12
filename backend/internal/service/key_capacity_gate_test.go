package service

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 容量闸单测（add-key-capacity-contract）：两级硬顶、幂等释放、泄漏对账。
// 判据来自 design「运行时准入」与「计数泄漏防护」两节。

func TestKeyCapacityGate_PoolCapIsHard(t *testing.T) {
	g := NewKeyCapacityGate()

	r1, rel1 := g.TryAcquireCapacity(1, 10, 2, 0)
	r2, rel2 := g.TryAcquireCapacity(1, 10, 2, 0)
	r3, rel3 := g.TryAcquireCapacity(1, 10, 2, 0)
	defer rel1()
	defer rel2()
	defer rel3()

	require.True(t, r1.Allowed)
	require.True(t, r2.Allowed)
	require.False(t, r3.Allowed, "池满必须拒")
	require.Equal(t, "user", r3.Scope, "归因必须是 user 层")
}

func TestKeyCapacityGate_KeyCapIsHardEvenWhenPoolFree(t *testing.T) {
	g := NewKeyCapacityGate()

	r1, rel1 := g.TryAcquireCapacity(1, 10, 5, 1)
	r2, _ := g.TryAcquireCapacity(1, 10, 5, 1)
	defer rel1()

	require.True(t, r1.Allowed)
	require.False(t, r2.Allowed)
	require.Equal(t, "key", r2.Scope, "归因必须是 key 层")
}

func TestKeyCapacityGate_MultiKeyCannotExceedPool(t *testing.T) {
	g := NewKeyCapacityGate()
	const pool = 5

	var rels []func()
	allowed := 0
	for i := 0; i < 10; i++ {
		d, rel := g.TryAcquireCapacity(1, int64(100+i), pool, 0)
		if d.Allowed {
			allowed++
			rels = append(rels, rel)
		}
	}
	for _, rel := range rels {
		rel()
	}
	require.Equal(t, pool, allowed, "多 key 合计必须被池硬顶挡住（堵 N 把 key = N×5）")
}

func TestKeyCapacityGate_ZeroMeansUnlimited(t *testing.T) {
	g := NewKeyCapacityGate()
	for i := 0; i < 50; i++ {
		d, rel := g.TryAcquireCapacity(9, 99, 0, 0)
		require.True(t, d.Allowed, "0/0 必须等同不限（存量 key 全在此）")
		defer rel()
	}
}

func TestKeyCapacityGate_ReleaseIsIdempotent(t *testing.T) {
	g := NewKeyCapacityGate()

	d, rel := g.TryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d.Allowed)

	rel()
	rel() // 重复释放不得把计数扣成负数——否则该 key 永远放行（静默失效）

	d2, rel2 := g.TryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d2.Allowed, "重复释放后应恢复可用")
	rel2()

	userInflight, keyInflight, _ := g.CapacitySnapshot(1, 10)
	require.EqualValues(t, 0, userInflight, "计数必须归零，不能为负")
	require.EqualValues(t, 0, keyInflight)
}

func TestKeyCapacityGate_ReconcileRecoversLeakOnBothLevels(t *testing.T) {
	g := NewKeyCapacityGate()

	// 模拟泄漏：拿了不还
	d, _ := g.TryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d.Allowed)

	require.Equal(t, 0, g.Reconcile(time.Minute, time.Now()), "未过期不回收")

	recovered := g.Reconcile(time.Minute, time.Now().Add(2*time.Minute))
	require.Equal(t, 1, recovered)

	userInflight, keyInflight, leaked := g.CapacitySnapshot(1, 10)
	require.EqualValues(t, 0, keyInflight, "key 计数应清零")
	require.EqualValues(t, 0, userInflight, "**池计数也必须清零**——只退 key 会让池永久少一格")
	require.EqualValues(t, 1, leaked, "回收必须记账，泄漏要可见不能静默自愈")

	// 回收后应可再次获取（池也真的腾出来了）
	d2, rel2 := g.TryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d2.Allowed)
	rel2()
}

func TestKeyCapacityGate_NilGateFailsOpen(t *testing.T) {
	var g *KeyCapacityGate
	d, rel := g.TryAcquireCapacity(1, 10, 1, 0)
	require.True(t, d.Allowed, "未装配必须放行（fail-open），不得全站 429")
	rel()
}

func TestKeyCapacityGate_ConcurrentAcquireKeepsCountsConsistent(t *testing.T) {
	g := NewKeyCapacityGate()
	const pool = 5
	const workers = 64

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, rel := g.TryAcquireCapacity(1, 10, pool, 0)
			if d.Allowed {
				time.Sleep(time.Millisecond)
				rel()
			}
		}()
	}
	wg.Wait()

	userInflight, keyInflight, _ := g.CapacitySnapshot(1, 10)
	require.EqualValues(t, 0, userInflight, "全部释放后池计数必须归零")
	require.EqualValues(t, 0, keyInflight, "全部释放后 key 计数必须归零")
}
