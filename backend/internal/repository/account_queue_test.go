package repository

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// resetQueue: 单测间清空注册表（sync.Map 无 Reset，换账号 ID 亦可，但显式
// 清理让断言更好读）。
func resetQueue(t *testing.T) {
	t.Helper()
	accountQueues.Range(func(k, _ any) bool {
		accountQueues.Delete(k)
		return true
	})
	t.Setenv(acctQueueBoundEnv, "")
	t.Setenv(acctQueueWaitersEnv, "")
	// 唤醒丢失的兜底轮询在测试里缩短：收敛快，断言窗仍足够宽。
	old := acctQueuePollInterval
	acctQueuePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { acctQueuePollInterval = old })
}

func TestAcquireBoundedByAccountConcurrency(t *testing.T) {
	resetQueue(t)
	const accountID = 101
	done := make(chan struct{})
	held := make([]*accountQueueState, 0, 3)
	for i := 0; i < 3; i++ {
		st, err := acquireAccountSlot(accountID, 3)
		if err != nil {
			t.Fatalf("acquire %d: unexpected error %v", i, err)
		}
		held = append(held, st)
	}
	// 满了：队列未满（waitMax=12）时等待而非拒绝——起 goroutine 等待。
	go func() {
		_, err := acquireAccountSlot(accountID, 3)
		if err != nil {
			t.Errorf("4th acquire should wait, got %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("4th acquire returned while 3 slots still held")
	case <-time.After(300 * time.Millisecond):
		// 预期：仍在等待。
	}
	// 归还一个槽位 → 等待者放行。
	releaseAccountSlot(held[0])
	select {
	case <-done:
		// 预期路径
	case <-time.After(3 * time.Second):
		t.Fatal("4th acquire never released after slot return")
	}
}

func TestAcquireQueueFullFastReject(t *testing.T) {
	resetQueue(t)
	const accountID = 102
	held := make([]*accountQueueState, 0, 2)
	for i := 0; i < 2; i++ {
		h, err := acquireAccountSlot(accountID, 2)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		held = append(held, h)
	}
	// 占满 waitMax（bound=2 → waiters=8）：12 个并发等待者——8 个入队轮询、
	// 4 个应快速拒。主测程随后逐个归还槽位，等待者自然放行（否则它们按设计
	// 会一直等——无界等不是 bug，正是「有界排队」的行为面）。
	var rejected atomic.Int64
	var wg sync.WaitGroup
	// 批 1：8 个先入队（waiting 满），确保后续到达者看到队满。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := acquireAccountSlot(accountID, 2)
			if errors.Is(err, service.ErrAccountQueueFull) {
				rejected.Add(1)
				return
			}
			if err == nil {
				defer releaseAccountSlot(st)
			}
		}()
	}
	// 等 waiting 爬满（waitMax=8）：轮询窗口 2s。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := AccountQueueMetrics(accountID); ok && snap.Waiting >= 8 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap, ok := AccountQueueMetrics(accountID); ok {
		t.Logf("gate exit: waiting=%d inFlight=%d", snap.Waiting, snap.InFlight)
	}
	// 批 2（主线程同步）：waiting=8 已满，必然快速拒——确定性无竞态。
	for i := 0; i < 4; i++ {
		if _, err := acquireAccountSlot(accountID, 2); !errors.Is(err, service.ErrAccountQueueFull) {
			t.Fatalf("batch2 acquire %d: want ErrQueueFull, got %v", i, err)
		}
		rejected.Add(1)
	}
	// 主测程释放自己占的 2 个槽 → 等待者开始放行 → 队列腾空 → 拒绝发生。
	for _, h := range held {
		releaseAccountSlot(h)
	}
	wg.Wait()
	if rejected.Load() == 0 {
		t.Fatal("expected at least one ErrQueueFull with 2-slot bound and 12 waiters")
	}
}

func TestAIMDTripHalvesAndRecovers(t *testing.T) {
	resetQueue(t)
	const accountID = 103
	st, err := acquireAccountSlot(accountID, 8)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	releaseAccountSlot(st)
	// 三连 5xx → 减半 + 熔开。
	for i := 0; i < 3; i++ {
		st.RecordAIMDResultForTest(accountID, 503)
	}
	snap, ok := AccountQueueMetrics(accountID)
	if !ok {
		t.Fatal("queue state missing")
	}
	if snap.Bound != 4 {
		t.Fatalf("bound after trip = %d, want 4 (8/2)", snap.Bound)
	}
	if !snap.Tripped {
		t.Fatal("tripped flag expected after 3 consecutive 5xx")
	}
	if snap.TripCount != 1 {
		t.Fatalf("trip count = %d, want 1", snap.TripCount)
	}
	// 熔开态：成功即恢复 +1/冷却期，两次成功（跨过冷却窗）恰好 +2。
	// 第一次成功：清 tripped；lastRecover=0 → now-0 必超冷却 → bound 4→5。
	st.RecordAIMDResultForTest(accountID, 200)
	snap, _ = AccountQueueMetrics(accountID)
	if snap.Bound != 5 {
		t.Fatalf("bound after first success = %d, want 5", snap.Bound)
	}
	// 第二次成功：若在冷却窗内则不动；显式睡过冷却窗 → 5→6。
	time.Sleep(aimdRecoverCooldown + 100*time.Millisecond)
	st.RecordAIMDResultForTest(accountID, 200)
	snap, _ = AccountQueueMetrics(accountID)
	if snap.Bound != 6 {
		t.Fatalf("bound after second success = %d, want 6", snap.Bound)
	}
	if snap.Tripped {
		t.Fatal("tripped must clear on success")
	}
}

func TestAIMD429CapacityShrinkWithoutStreak(t *testing.T) {
	resetQueue(t)
	const accountID = 104
	st, _ := acquireAccountSlot(accountID, 8)
	releaseAccountSlot(st)
	// 两次 429：每次减半（8→4→2），但不触发 trip 语义。
	st.RecordAIMDResultForTest(accountID, 429)
	st.RecordAIMDResultForTest(accountID, 429)
	snap, _ := AccountQueueMetrics(accountID)
	if snap.Bound != 2 {
		t.Fatalf("bound after two 429 = %d, want 2", snap.Bound)
	}
	if snap.Tripped {
		t.Fatal("429 is a capacity signal, not a trip")
	}
	if snap.TripCount != 0 {
		t.Fatalf("trip count = %d, want 0 (429 never trips)", snap.TripCount)
	}
}

func TestClientCancelNeverFeedsAIMD(t *testing.T) {
	resetQueue(t)
	const accountID = 105
	// 这个不变量由 Do 的 req.Context().Err() 分支保证——此处验证 4xx 语义错误
	// 走恢复路径（不清零 bound 也不 trip）。
	st, _ := acquireAccountSlot(accountID, 8)
	releaseAccountSlot(st)
	st.RecordAIMDResultForTest(accountID, 400)
	st.RecordAIMDResultForTest(accountID, 401)
	snap, _ := AccountQueueMetrics(accountID)
	if snap.Bound != 8 {
		t.Fatalf("4xx must not shrink bound, got %d", snap.Bound)
	}
	if snap.Tripped {
		t.Fatal("4xx must not trip")
	}
}

func TestConcurrencyHotUpgrade(t *testing.T) {
	resetQueue(t)
	const accountID = 106
	// 先以默认 env bound 建队列。
	st, _ := acquireAccountSlot(accountID, 0)
	releaseAccountSlot(st)
	snap, _ := AccountQueueMetrics(accountID)
	if snap.Limit != defaultAccountQueueBound {
		t.Fatalf("initial limit = %d, want default %d", snap.Limit, defaultAccountQueueBound)
	}
	// 管理员把账号并发调大 → 热更新。
	st2, _ := acquireAccountSlot(accountID, 16)
	releaseAccountSlot(st2)
	snap, _ = AccountQueueMetrics(accountID)
	if snap.Limit != 16 || snap.Bound != 16 {
		t.Fatalf("hot upgrade: limit=%d bound=%d, want 16/16", snap.Limit, snap.Bound)
	}
}

func TestMetricsUnknownAccount(t *testing.T) {
	resetQueue(t)
	if _, ok := AccountQueueMetrics(999999); ok {
		t.Fatal("unknown account must report ok=false")
	}
}
