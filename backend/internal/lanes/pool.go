package lanes

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 车道标识（错误体 / 计量入参 / 池映射共用）。
const (
	LaneRerank    = "rerank"
	LaneWebsearch = "websearch"
	LaneAudio     = "audio"
)

// LanePlatform 车道 v1 上游平台映射：audio/rerank=硅基（原生 ASR/TTS/rerank，
// 一平台供两车道）、websearch=SerpAPI（search-relay 既有 key 池可播种，运营连续）。
func LanePlatform(lane string) string {
	switch lane {
	case LaneAudio, LaneRerank:
		return domain.PlatformSiliconFlow
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

// ErrNoLaneAccount 池空：车道暂无可用上游（未配置或全部停用），响应 503。
var ErrNoLaneAccount = errors.New("lane upstream pool empty")

// Pool 车道上游账号池（tasks 2.4）：车道上游 key = 原生 Account 行（平台=车道
// 上游、Gateway=lane 不被 chat 网关认领），经既有 admin 账号管理面增删/启停，
// 健康面=账号 status/error_message 既有字段（3.x 适配器回写探测结果）。
// 取数复用 ListSchedulableByPlatform（priority ASC 稳定序，同 chat 调度口径）；
// v1 选择=列表首个 active 账号，多账号轮转随 3.x 按并发/冷却细化。
type Pool struct {
	accounts AccountLister
}

// NewPool accounts 为 nil 时池恒空（车道 503）。
func NewPool(accounts AccountLister) *Pool { return &Pool{accounts: accounts} }

// Pick 取该平台当前可用的一枚上游账号。
func (p *Pool) Pick(ctx context.Context, platform string) (*service.Account, error) {
	if p == nil || p.accounts == nil || platform == "" {
		return nil, ErrNoLaneAccount
	}
	list, err := p.accounts.ListSchedulableByPlatform(ctx, platform)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Status == service.StatusActive {
			return &list[i], nil
		}
	}
	return nil, ErrNoLaneAccount
}
