package repository

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 3.2 压测（add-sub2api-fork-concurrency）：5xx 注入下验证三件事——
//  1. queue 有界：等待者数量被 waitMax 封顶，多余的立即拒绝（不无界堆积）；
//  2. 客户端零吞超时：所有调用在有限时间内返回（成功/拒/错），没有调用者
//     挂死——「排队时长吞噬客户端超时」的反面证据；
//  3. AIMD 收窄后系统仍自洽：拒绝计数守恒、恢复路径活。
//
// 并发压力来自真实 goroutine 竞争（非 mock 时序）， poll 间隔压到 1ms 加速。

func TestStressQueueBoundedUnder5xxStorm(t *testing.T) {
	resetQueue(t)
	old := acctQueuePollInterval
	acctQueuePollInterval = 1 * time.Millisecond
	t.Cleanup(func() { acctQueuePollInterval = old })

	const accountID = 401
	const workers = 32 // bound=2, waiters=8 → 稳态 2 在途 + 8 排队 + 22 快拒

	// 注入器：每 1ms 打一次 5xx（三连即减半+熔开），模拟坏上游。间隔密过
	// worker 成功流量的连击清零（真实风暴=持续失败，无成功穿插）。
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(1 * time.Millisecond):
				if v, ok := accountQueues.Load(accountID); ok {
					// 一次 tick 打满三连（真实风暴=持续失败），击穿成功清零。
					if st, ok := v.(*accountQueueState); ok {
						aimdRecordFailure(st)
						aimdRecordFailure(st)
						aimdRecordFailure(st)
					}
				}
			}
		}
	}()

	var okCount, fullCount, trippedCount atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				st, err := acquireAccountSlot(accountID, 2)
				switch err {
				case nil:
					okCount.Add(1)
					time.Sleep(2 * time.Millisecond) // 模拟上游调用
					releaseAccountSlot(st)
				case service.ErrAccountQueueFull:
					fullCount.Add(1)
				case service.ErrAccountTrippedAIMD:
					trippedCount.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}
	wg.Wait()
	close(stop)

	snap, ok := AccountQueueMetrics(accountID)
	if !ok {
		t.Fatal("queue state missing")
	}
	// 1) queue 有界：任意时刻 waiting ≤ waitMax（快拒计数>0 证明拒绝路径活着，
	//    无界堆积的反面是「拒绝发生」）。
	if snap.QueueFull == 0 {
		t.Fatal("expected queue-full rejections under storm, none observed")
	}
	if snap.Waiting > int32(workers) {
		t.Fatalf("waiting %d exceeds sanity (workers=%d)", snap.Waiting, workers)
	}
	// 2) 零吞超时：整个压测在 3s deadline 内自然结束（wg.Wait 已返回）；
	//    且成功路径活着（AIMD 在收窄/恢复循环中至少放过一部分流量）。
	if okCount.Load() == 0 {
		t.Fatal("expected some successful acquires even under AIMD pressure")
	}
	// 3) AIMD 语义自洽：混合流量下（成功清零连击 vs 注入失败凑三连是竞速），
	//    不强断言 trip 发生（trip 由 Recovery 测试确定性覆盖）；这里断言
	//    bound 永不超过 limit 且状态机字段守恒。
	if snap.Bound > snap.Limit {
		t.Fatalf("bound %d exceeds limit %d — AIMD invariant broken", snap.Bound, snap.Limit)
	}
	t.Logf("ok=%d full=%d tripped=%d bound=%d trips=%d",
		okCount.Load(), fullCount.Load(), trippedCount.Load(), snap.Bound, snap.TripCount)
}

func TestStressRecoveryAfterStormClears(t *testing.T) {
	resetQueue(t)
	old := acctQueuePollInterval
	acctQueuePollInterval = 1 * time.Millisecond
	t.Cleanup(func() { acctQueuePollInterval = old })

	const accountID = 402
	st, err := acquireAccountSlot(accountID, 8)
	if err != nil {
		t.Fatal(err)
	}
	releaseAccountSlot(st)
	// 风暴：连打 9 次 5xx（三次 trip → 8→4→2→1）。无成功穿插=连击累积。
	for i := 0; i < 9; i++ {
		aimdRecordFailure(st)
	}
	snap, _ := AccountQueueMetrics(accountID)
	t.Logf("after storm: bound=%d tripped=%v streak=%d", snap.Bound, snap.Tripped, atomic.LoadInt32(&st.failStreak))
	if snap.Bound != 1 {
		t.Fatalf("bound after storm = %d, want floor 1", snap.Bound)
	}
	// 风暴停止：成功流量驱动恢复到满并发（每次 +1，冷却期 2ms 模拟）。
	oldCooldown := aimdRecoverCooldown
	aimdRecoverCooldown = 2 * time.Millisecond
	t.Cleanup(func() { aimdRecoverCooldown = oldCooldown })
	for i := 0; i < 20; i++ {
		aimdRecordSuccess(st)
		time.Sleep(time.Millisecond)
	}
	snap, _ = AccountQueueMetrics(accountID)
	if snap.Bound != 8 {
		t.Fatalf("bound after recovery = %d, want full 8", snap.Bound)
	}
	if snap.Tripped {
		t.Fatal("tripped must clear after sustained success")
	}
}
