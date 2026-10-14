package lanes

import (
	"context"
	"errors"
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

// LanePlatform 车道 v1 上游平台映射：rerank=硅基（原生 /v1/rerank）、
// audio=openai-next 聚合（OpenAI 形状 whisper/tts 系；运营分工：rerank=硅基、
// audio/image/video=openai-next）、websearch=SerpAPI（search-relay 既有 key 池）。
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

// AccountLister 池取数面（生产=OpenAIGatewayService.accountRepo，经只读访问器）。
type AccountLister interface {
	ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error)
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
