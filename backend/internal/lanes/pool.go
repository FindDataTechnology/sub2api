package lanes

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"go.uber.org/zap"
)

// 车道标识（错误体 / 计量入参 / 池映射共用）。
const (
	LaneRerank    = "rerank"
	LaneWebsearch = "websearch"
	LaneAudio     = "audio"
)

// LanePlatform 车道上游平台映射：rerank=硅基（原生 /v1/rerank）、
// audio=多种上游（见 LanePlatforms）、websearch=SerpAPI（search-relay 既有 key 池）。
//
// audio 自 audio-lane-multi-upstream 起支持跨协议上游，单平台映射已不足以
// 表达候选集合；本函数保留以兼容既有调用（返回 audio 的首选平台），
// 取候选请用 LanePlatforms。
func LanePlatform(lane string) string {
	switch lane {
	case LaneRerank:
		return domain.PlatformSiliconFlow
	case LaneAudio:
		return domain.PlatformOpenAINext
	case LaneWebsearch:
		return domain.PlatformSerpAPI
	default:
		return ""
	}
}

// LanePlatforms 该车道可取候选的**全部**平台（跨协议上游用）。
// 返回顺序即候选归并时的平台优先序（同平台内仍按账号 priority 升序）。
//
// audio 的车道语义是「能完成 OpenAI 音频形状的上游」，与具体协议无关——
// 协议由账号凭据 audio_protocol 决定（见 audio.go 的分派）。因此登记在此的
// 平台都会进入 audio 候选池：账号建在哪个平台，就在哪个协议族里竞逐。
func LanePlatforms(lane string) []string {
	switch lane {
	case LaneAudio:
		return []string{
			domain.PlatformOpenAINext,
			domain.PlatformDoubao,
			domain.PlatformTencent,
		}
	default:
		if p := LanePlatform(lane); p != "" {
			return []string{p}
		}
		return nil
	}
}

// AccountLister 池取数面（生产=OpenAIGatewayService.accountRepo，经只读访问器）。
type AccountLister interface {
	ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error)
	ListSchedulableByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error)
}

// AccountCooldownWriter 池冷却写面（ciyuan-lane-pool-failover D1）：复用上游
// SetTempUnschedulable——冷却落 DB（temp_unschedulable_until），跨重启、admin
// 账号面可见；ListSchedulableByPlatform 本就按该字段过滤，写冷却=账号退出候选。
// 生产=同一 accountRepo（零新增缝点）。
type AccountCooldownWriter interface {
	SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error
}

// ErrNoLaneAccount 池空：车道暂无可用上游（未配置或全部停用/冷却），响应 503。
var ErrNoLaneAccount = errors.New("lane upstream pool empty")

// 冷却写节流窗：同账号 60s 内不重复写（上游全挂时每请求一次 DB 写的护栏）。
const laneCooldownWriteThrottle = 60 * time.Second

// Pool 车道上游账号池：车道上游 key = 原生 Account 行（平台=车道上游、
// Gateway=lane 不被 chat 网关认领），经既有 admin 账号管理面增删/启停。
// v1.1（ciyuan-lane-pool-failover）：Candidates 返回全候选列表（priority ASC
// 稳定序），失败轮转与冷却分级在适配器层（failover.go 共享循环）。
type Pool struct {
	accounts AccountLister
	cooldown AccountCooldownWriter

	mu          sync.Mutex
	lastCooldown map[int64]time.Time // account id -> 上次冷却写时刻（节流）
}

// NewPool accounts 为 nil 时池恒空（车道 503）。cooldown 可为 nil（冷却写退化为
// 纯日志——测试桩/只读仓储场景；轮转本身不受影响）。
func NewPool(accounts AccountLister, cooldown AccountCooldownWriter) *Pool {
	return &Pool{accounts: accounts, cooldown: cooldown, lastCooldown: make(map[int64]time.Time)}
}

// Candidates 取该平台当前全部可调度候选（ListSchedulableByPlatform 已过滤
// status/schedulable/temp_unschedulable/overload/rate_limit 并按 priority ASC）。
// 空列表=池空（ErrNoLaneAccount 语义）。
func (p *Pool) Candidates(ctx context.Context, platform string) ([]*service.Account, error) {
	if p == nil || p.accounts == nil || platform == "" {
		return nil, ErrNoLaneAccount
	}
	list, err := p.accounts.ListSchedulableByPlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNoLaneAccount
	}
	out := make([]*service.Account, 0, len(list))
	for i := range list {
		out = append(out, &list[i])
	}
	return out, nil
}

// CandidatesForPlatforms 取**多个平台**的候选并归并（跨协议上游用，如 audio）。
//
// 归并语义：按 platforms 给出的平台序做稳定分组（同平台内保持 priority ASC），
// 使「首选平台优先、其内按账号优先级」成为确定序；失败轮转仍由适配器层的
// serveWithFailover 逐个尝试。空列表=池空。
func (p *Pool) CandidatesForPlatforms(ctx context.Context, platforms []string) ([]*service.Account, error) {
	if p == nil || p.accounts == nil || len(platforms) == 0 {
		return nil, ErrNoLaneAccount
	}
	list, err := p.accounts.ListSchedulableByPlatforms(ctx, platforms)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNoLaneAccount
	}
	// 按平台序稳定归组（ListSchedulableByPlatforms 的返回序不保证跨平台次序）。
	order := make(map[string]int, len(platforms))
	for i, pl := range platforms {
		if _, ok := order[pl]; !ok {
			order[pl] = i
		}
	}
	out := make([]*service.Account, 0, len(list))
	for i := range list {
		out = append(out, &list[i])
	}
	sort.SliceStable(out, func(a, b int) bool {
		oa, okA := order[out[a].Platform]
		ob, okB := order[out[b].Platform]
		if !okA {
			oa = len(platforms)
		}
		if !okB {
			ob = len(platforms)
		}
		return oa < ob
	})
	return out, nil
}

// CooldownAccount best-effort 写账号冷却（failover 循环调用）：节流窗内同账号
// 不重复写；写失败只记日志不阻塞换号重试（冷却丢一拍=多试一次，无害）。
// detached ctx：冷却写与调用者取消脱钩（同 meter 用量写口径）。
func (p *Pool) CooldownAccount(accountID int64, d time.Duration, reason string) {
	if p == nil || p.cooldown == nil || accountID <= 0 || d <= 0 {
		return
	}
	now := time.Now()
	p.mu.Lock()
	if last, ok := p.lastCooldown[accountID]; ok && now.Sub(last) < laneCooldownWriteThrottle {
		p.mu.Unlock()
		return
	}
	p.lastCooldown[accountID] = now
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 3*time.Second)
	defer cancel()
	if err := p.cooldown.SetTempUnschedulable(ctx, accountID, now.Add(d), reason); err != nil {
		logger.L().With(
			zap.String("component", "lanes.pool"),
			zap.Int64("account_id", accountID),
		).Warn("lanes.cooldown_write_failed", zap.String("reason", reason), zap.Error(err))
	}
}
