package lanes

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 车道失败分级冷却时长（ciyuan-lane-pool-failover D2，口径对齐 search-relay：
// 额度/auth 类 12h、瞬态类 60s）。
const (
	laneCooldownExhausting = 12 * time.Hour
	laneCooldownTransient  = 60 * time.Second
)

// upstreamFailure 一次候选账号尝试的失败事实（轮转/冷却/最终错误的三车道
// 共同输入）。bodyExcerpt 仅进分级判据——绝不入响应与日志（不外泄纪律）。
type upstreamFailure struct {
	status        int    // 上游 HTTP 状态码；0=传输层失败（断连/超时）
	bodyExcerpt   string // 上游错误体摘录（分级判据用，只取前若干字节）
	misconfigured bool   // 账号缺凭据（车道侧配置错误：长冷却+换号）
}

// serveWithFailover 车道候选轮转共享循环（ciyuan-lane-pool-failover D3）：
// 按 priority 序遍历候选，attempt 二选一——
//   - responded=true：HTTP 应答已完整写出（成功路径，或调用者侧 400/413 等
//     换号无意义的终态），循环结束；
//   - 返回 *upstreamFailure：该账号上游失败，按分级写冷却（best-effort）后
//     换下一候选。
//
// 全池尽：按最后一个失败写既有统一错误体（400/422→400，0→502 status 0，
// 其余→502 带状态码）——对外形状与 v1 完全一致，只是到达这里前多了轮转。
func serveWithFailover(
	c *gin.Context,
	lane string,
	pool *Pool,
	candidates []*service.Account,
	attempt func(*service.Account) (bool, *upstreamFailure),
) {
	var last *upstreamFailure
	for _, account := range candidates {
		responded, failure := attempt(account)
		if responded {
			return
		}
		if failure == nil {
			// 防御：适配器既没写应答也没报事实——按瞬态处理换下一号。
			failure = &upstreamFailure{}
		}
		if failure.misconfigured {
			pool.CooldownAccount(account.ID, laneCooldownExhausting, fmt.Sprintf("lane %s: account missing credentials", lane))
		} else {
			d, kind := laneCooldownTransient, "transient"
			if classifyLaneUpstreamFailure(failure.status, failure.bodyExcerpt) {
				d, kind = laneCooldownExhausting, "quota/auth"
			}
			pool.CooldownAccount(account.ID, d, fmt.Sprintf("lane %s upstream %s failure (status %d)", lane, kind, failure.status))
		}
		last = failure
	}
	switch {
	case last == nil:
		laneUpstreamUnavailable(c, lane)
	case last.misconfigured:
		laneUpstreamMisconfigured(c, lane)
	case last.status == 0:
		laneUpstreamError(c, lane, 0)
	default:
		laneUpstreamHTTPError(c, lane, last.status)
	}
}

// laneUpstreamFailureFromResponse 适配器通用：上游非 2xx 时读一小段错误体
// （分级判据用）并构造失败事实。读体失败不影响分级（excerpt 留空）。
func laneUpstreamFailureFromResponse(resp *http.Response) *upstreamFailure {
	failure := &upstreamFailure{status: resp.StatusCode}
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	failure.bodyExcerpt = string(buf[:n])
	return failure
}
