package repository

import (
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ── Per-account bounded queue (add-sub2api-fork-concurrency 2.1/2.2) ────────
//
// 病根（2026-10-27 词元配额事故同族）：http.Transport 的 MaxConnsPerHost 饱和后
// 新请求在 transport 内部队列无界等待——一个 1.1s 就报 403 的坏仓库能把整池
// 拖进分钟级排队，客户端超时被排队时长吞噬。本文件在 httpUpstream 入口加
// per-account 有界信号量：
//
//   - bound：并发上限。account.Concurrency>0 时用它，否则用默认值（env
//     GATEWAY_ACCOUNT_QUEUE_BOUND，默认 8）。
//   - waiters：等待队列上限 = bound×4（env GATEWAY_ACCOUNT_QUEUE_MAX_WAITERS）。
//     队满立即 ErrQueueFull 快速拒——handler 层映射 429 + Retry-After，客户端
//     可控重试，排队时长不再吞超时。
//   - AIMD（2.2）：与信号量同一槽位记账。success 恢复 +1（封顶 limit）；
//     5xx 连击减半（下限 1）；429 作容量信号收窄不计连击。减半不是封禁：
//     账号仍可服务，只是并发面收窄，恢复由成功流量自然驱动。
//
// 信号量是进程内的：多副本部署时总并发 = 副本数 × bound，按副本计即可。

// 有界队列快速拒哨兵：定义在 service 层（依赖方向 service→port→repo），
// 这里 alias 保持本包调用点可读。
var (
	// ErrQueueFull: 等待队列已满——调用方应立即以 429 快速拒（Retry-After 头）。
	ErrQueueFull = service.ErrAccountQueueFull
	// ErrAccountTripped: AIMD 熔开（连续故障后并发已收窄且最新探测仍在途）——
	// 同 429 语义，Retry-After 用恢复探针间隔。
	ErrAccountTripped = service.ErrAccountTrippedAIMD
)

const (
	defaultAccountQueueBound = 8
	aimdFloor                = 1
	aimdTripThreshold        = 3 // 连续失败次数到达即减半
	acctQueueBoundEnv        = "UPSTREAM_CONCURRENCY_MAX"     // 每账号默认并发上限（design 对齐）
	acctQueueWaitersEnv      = "UPSTREAM_QUEUE_BOUND"        // 每账号队列深度
	acctQueueAIMDEnv         = "UPSTREAM_AIMD_ENABLED"       // true/false；false=静态并发（有界队列仍生效）
)

// 兜底轮询：唤醒通知 cap1 会合并（多次 release 只留一个信号），等待者被
// 唤醒后若抢不到槽会重睡——通知丢失的极端情况由这层轮询自愈。生产词元请求
// 延迟秒级，200ms 兜底不可感知；测试里可调短。
var (
	aimdRecoverCooldown   = 2 * time.Second
	acctQueuePollInterval = 200 * time.Millisecond
)


type accountQueueState struct {
	bound       int32  // 当前允许的并发上限（AIMD 动态收窄，≤ limit）
	limit       int32  // 初始 bound（恢复封顶）
	inFlight    int32  // 已发出去未归还的槽位
	waiting     int32  // 在队列里等槽位的请求数
	waitMax     int32  // 等待队列上限
	wake        chan struct{} // cap1 释放通知；丢失由兜底轮询自愈
	tripped     int32  // AIMD：1 = 连续失败已触发减半（探针=inFlight 里的请求）
	failStreak  int32  // 连续失败计数（success 清零）
	lastRecover int64  // 上次 +1 恢复的 unixNano（cooldown 去抖）
	maxSeen     int32  // 观测峰值（metrics 用）
	queueFull   int64  // ErrQueueFull 计数（metrics）
	tripCount   int64  // AIMD 减半次数（metrics）
}

// accountQueues: 进程级注册表，键 = 账号 ID（隔离粒度与调度一致；
// proxy 维度不重复收窄——代理故障由连接层超时兜底，这里只管账号容量）。
var accountQueues sync.Map // int64 -> *accountQueueState

func accountQueueBoundFromEnv() int32 {
	if v := envInt(acctQueueBoundEnv, 0); v > 0 {
		return int32(v)
	}
	return defaultAccountQueueBound
}

func accountQueueWaitersFromEnv(bound int32) int32 {
	if v := envInt(acctQueueWaitersEnv, 0); v > 0 {
		return int32(v)
	}
	w := bound * 4
	if w < 1 {
		w = 1
	}
	return w
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// queueFor 取（或建）账号的队列状态。accountConcurrency>0 时以它为初始
// 上限（管理员显式设置优先于 env 默认）；热更新只升不降（降由 AIMD 管）。
func queueFor(accountID int64, accountConcurrency int) *accountQueueState {
	if v, ok := accountQueues.Load(accountID); ok {
		st, ok := v.(*accountQueueState)
		if !ok {
			return nil
		}
		if accountConcurrency > 0 {
			want := int32(accountConcurrency)
			for {
				cur := atomic.LoadInt32(&st.limit)
				if want <= cur {
					break
				}
				if atomic.CompareAndSwapInt32(&st.limit, cur, want) {
					atomic.StoreInt32(&st.bound, want)
					break
				}
			}
		}
		return st
	}
	bound := accountQueueBoundFromEnv()
	if accountConcurrency > 0 {
		bound = int32(accountConcurrency)
	}
	st := &accountQueueState{bound: bound, limit: bound, waitMax: accountQueueWaitersFromEnv(bound), wake: make(chan struct{}, 1)}
	actual, _ := accountQueues.LoadOrStore(accountID, st)
	queued, ok := actual.(*accountQueueState)
	if !ok {
		return st
	}
	return queued
}

// acquireAccountSlot: 带界排队。快速路径 CAS 占槽；慢路径入等待队列
// （受 waitMax 约束，满即 ErrQueueFull）；AIMD 熔开时只放一个探针。
func acquireAccountSlot(accountID int64, accountConcurrency int) (*accountQueueState, error) {
	st := queueFor(accountID, accountConcurrency)
	for {
		bound := atomic.LoadInt32(&st.bound)
		inFlight := atomic.LoadInt32(&st.inFlight)
		if atomic.LoadInt32(&st.tripped) == 1 && inFlight >= aimdFloor {
			// 熔开：在途的那个请求就是探针，其余快速拒。
			atomic.AddInt64(&st.queueFull, 1)
			return st, ErrAccountTripped
		}
		if inFlight < bound {
			if atomic.CompareAndSwapInt32(&st.inFlight, inFlight, inFlight+1) {
				if f := atomic.LoadInt32(&st.inFlight); f > atomic.LoadInt32(&st.maxSeen) {
					atomic.StoreInt32(&st.maxSeen, f)
				}
				return st, nil
			}
			continue
		}
		// 需要排队：waitMax 约束。
		waiting := atomic.AddInt32(&st.waiting, 1)
		if waiting > st.waitMax {
			atomic.AddInt32(&st.waiting, -1)
			atomic.AddInt64(&st.queueFull, 1)
			return st, ErrQueueFull
		}
		// 等唤醒信号（cap1 非阻塞；丢失由兜底轮询自愈）。
		select {
		case <-st.wake:
		case <-time.After(acctQueuePollInterval):
		}
		atomic.AddInt32(&st.waiting, -1)
	}
}

// releaseAccountSlot: 归还槽位并通知一个等待者。
func releaseAccountSlot(st *accountQueueState) {
	atomic.AddInt32(&st.inFlight, -1)
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// aimdRecordSuccess: 成功——失败连击清零；若 AIMD 已收窄且过了冷却期，+1 恢复。
func aimdRecordSuccess(st *accountQueueState) {
	atomic.StoreInt32(&st.failStreak, 0)
	if !aimdEnabled() {
		return
	}
	atomic.StoreInt32(&st.tripped, 0)
	bound := atomic.LoadInt32(&st.bound)
	limit := atomic.LoadInt32(&st.limit)
	if bound < limit {
		now := time.Now().UnixNano()
		last := atomic.LoadInt64(&st.lastRecover)
		if now-last > int64(aimdRecoverCooldown) && atomic.CompareAndSwapInt64(&st.lastRecover, last, now) {
			next := bound + 1
			if next > limit {
				next = limit
			}
			atomic.StoreInt32(&st.bound, next)
		}
	}
}

// aimdRecordFailure: 5xx——连击 +1；到达阈值减半（下限 1）并标记熔开。
func aimdEnabled() bool {
	v := os.Getenv(acctQueueAIMDEnv)
	return v == "" || v == "1" || v == "true" || v == "TRUE"
}

func aimdRecordFailure(st *accountQueueState) {
	if !aimdEnabled() {
		return
	}
	n := atomic.AddInt32(&st.failStreak, 1)
	if n < aimdTripThreshold {
		return
	}
	atomic.StoreInt32(&st.failStreak, 0)
	if atomic.SwapInt32(&st.tripped, 1) == 0 {
		atomic.AddInt64(&st.tripCount, 1)
	}
	bound := atomic.LoadInt32(&st.bound)
	next := int32(math.Max(float64(aimdFloor), float64(bound)/2))
	if next < bound {
		atomic.StoreInt32(&st.bound, next)
	}
}

// aimdRecordCapacityHit: 429 = 上游在说「这个账号太挤」——收窄并发面但不计
// 连击（账号本身健康，只是当前容量顶到上游限流线）。
func aimdRecordCapacityHit(st *accountQueueState) {
	if !aimdEnabled() {
		return
	}
	atomic.StoreInt32(&st.tripped, 0)
	bound := atomic.LoadInt32(&st.bound)
	next := int32(math.Max(float64(aimdFloor), float64(bound)/2))
	if next < bound {
		atomic.StoreInt32(&st.bound, next)
	}
}

// RecordAIMDResult: HTTPUpstream port 的实现（service 层喂点，2.2）。响应头
// 已到但状态码 5xx（或 429）时由账号错误面调用——transport 层失败已在 Do 内
// 记账，这里只补「响应头到了但语义失败」的分支。未建队列的账号静默忽略。
func (s *httpUpstreamService) RecordAIMDResult(accountID int64, statusCode int) {
	v, ok := accountQueues.Load(accountID)
	if !ok {
		return
	}
	st, ok := v.(*accountQueueState)
	if !ok {
		return
	}
	switch {
	case statusCode >= 500:
		aimdRecordFailure(st)
	case statusCode == 429:
		aimdRecordCapacityHit(st)
	default:
		aimdRecordSuccess(st)
	}
}

// AccountQueueMetricsSnapshot: 2.3 的 metrics 面（channel_monitor 哨兵收编消费）。
type AccountQueueMetricsSnapshot struct {
	Bound     int32 `json:"bound"`
	Limit     int32 `json:"limit"`
	InFlight  int32 `json:"in_flight"`
	Waiting   int32 `json:"waiting"`
	Tripped   bool  `json:"tripped"`
	MaxSeen   int32 `json:"max_seen"`
	QueueFull int64 `json:"queue_full_total"`
	TripCount int64 `json:"aimd_trips_total"`
}

// AccountQueueMetrics: 单账号快照（哨兵/metrics 读面）。ok=false = 该账号
// 自本进程启动以来没有发过上游请求（未建队列）。
func AccountQueueMetrics(accountID int64) (AccountQueueMetricsSnapshot, bool) {
	v, ok := accountQueues.Load(accountID)
	if !ok {
		return AccountQueueMetricsSnapshot{}, false
	}
	st := v.(*accountQueueState)
	return AccountQueueMetricsSnapshot{
		Bound:     atomic.LoadInt32(&st.bound),
		Limit:     atomic.LoadInt32(&st.limit),
		InFlight:  atomic.LoadInt32(&st.inFlight),
		Waiting:   atomic.LoadInt32(&st.waiting),
		Tripped:   atomic.LoadInt32(&st.tripped) == 1,
		MaxSeen:   atomic.LoadInt32(&st.maxSeen),
		QueueFull: atomic.LoadInt64(&st.queueFull),
		TripCount: atomic.LoadInt64(&st.tripCount),
	}, true
}

// RecordAIMDResultForTest: 单测直喂口（绕过「未建队列静默忽略」的注册表语义，
// 直接对已存在的状态机记账）。生产代码请走 (httpUpstreamService).RecordAIMDResult。
func (st *accountQueueState) RecordAIMDResultForTest(accountID int64, statusCode int) {
	switch {
	case statusCode >= 500:
		aimdRecordFailure(st)
	case statusCode == 429:
		aimdRecordCapacityHit(st)
	default:
		aimdRecordSuccess(st)
	}
}

// AccountQueueAIMDState: 哨兵面（channel_monitor aimd_state 收编）——真状态
// 字符串，非 SQL 猜形：open（熔开）/ half_open（收窄恢复中）/ closed（满并发）。
func AccountQueueAIMDState(accountID int64) string {
	snap, ok := AccountQueueMetrics(accountID)
	if !ok {
		return "unknown"
	}
	switch {
	case snap.Tripped:
		return "open"
	case snap.Bound < snap.Limit:
		return "half_open"
	default:
		return "closed"
	}
}

// AllAccountQueueMetrics: 全量枚举（metrics 端点一次拉全）。
func AllAccountQueueMetrics() map[int64]AccountQueueMetricsSnapshot {
	out := make(map[int64]AccountQueueMetricsSnapshot)
	accountQueues.Range(func(k, v any) bool {
		if id, ok := k.(int64); ok {
			if snap, exists := AccountQueueMetrics(id); exists {
				out[id] = snap
			}
		}
		return true
	})
	return out
}
