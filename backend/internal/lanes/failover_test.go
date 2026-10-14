package lanes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 轮转与分级（ciyuan-lane-pool-failover 2.1/3.x）：分级判据表（对齐
// search-relay isExhausting 口径）+ serveWithFailover 三形态（换号成功/全池尽/
// 终态 400 不换号）+ 冷却写联动。

func TestClassifyLaneUpstreamFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		// 额度/auth 类（长冷却）——真实样例：SerpAPI 额度尽、硅基余额、裸 401/402/403。
		{"serpapi quota text", 429, `{"error":"You've run out of searches"}`, true},
		{"serpapi quota text 200", 200, `{"error":"Your account doesn't have enough searches left"}`, true},
		{"siliconflow balance", 402, `{"error":{"message":"Insufficient balance"}}`, true},
		{"siliconflow balance 401", 401, `{"error":{"message":"invalid api key"}}`, true},
		{"bare 401", 401, "", true},
		{"bare 402", 402, "", true},
		{"bare 403", 403, "", true},
		{"api key text", 500, `{"error":"invalid API key"}`, true},
		{"unauthorized text", 200, `{"error":"Unauthorized"}`, true},
		// 瞬态类（短冷却）——裸 429（并发限）、5xx、断连。
		{"bare 429 transient", 429, "", false},
		{"bare 500 transient", 500, "", false},
		{"bare 503 transient", 503, "", false},
		{"transport failure", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyLaneUpstreamFailure(tc.status, tc.body))
		})
	}
}

func serveFailoverlane(t *testing.T, pool *Pool, candidates []*service.Account, attempt func(*gin.Context, *service.Account) (bool, *upstreamFailure)) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/rerank", nil)
	serveWithFailover(c, LaneRerank, pool, candidates, func(acc *service.Account) (bool, *upstreamFailure) {
		return attempt(c, acc)
	})
	return w
}

// 首号 402（额度类）→ 冷却 12h + 换二号线成功。
func TestServeWithFailoverRotatesOnExhausting(t *testing.T) {
	writer := newFakeCooldownWriter()
	pool := NewPool(nil, writer)
	candidates := []*service.Account{
		{ID: 1, Platform: domain.PlatformSerpAPI, Status: service.StatusActive},
		{ID: 2, Platform: domain.PlatformSerpAPI, Status: service.StatusActive},
	}
	visited := []int64{}
	w := serveFailoverlane(t, pool, candidates, func(c *gin.Context, acc *service.Account) (bool, *upstreamFailure) {
		visited = append(visited, acc.ID)
		if acc.ID == 1 {
			return false, &upstreamFailure{status: 402, bodyExcerpt: `{"error":"Insufficient balance"}`}
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return true, nil
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []int64{1, 2}, visited)
	// 冷却：账号 1 落 12h 额度类；账号 2 成功不冷却。
	require.Len(t, writer.writes[1], 1)
	require.InDelta(t, laneCooldownExhausting.Seconds(), writer.writes[1][0].Seconds(), 1.0)
	require.Empty(t, writer.writes[2])
}

// 瞬态失败 → 60s 短冷却；全池尽 → 最后失败形状的 502。
func TestServeWithFailoverAllExhausted(t *testing.T) {
	writer := newFakeCooldownWriter()
	pool := NewPool(nil, writer)
	candidates := []*service.Account{
		{ID: 1, Platform: domain.PlatformSerpAPI, Status: service.StatusActive},
		{ID: 2, Platform: domain.PlatformSerpAPI, Status: service.StatusActive},
	}
	w := serveFailoverlane(t, pool, candidates, func(*gin.Context, *service.Account) (bool, *upstreamFailure) {
		return false, &upstreamFailure{status: 500}
	})
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "lane_upstream_error")
	require.Contains(t, w.Body.String(), "status 500")
	// 两号都落瞬态短冷却。
	require.Len(t, writer.writes[1], 1)
	require.Len(t, writer.writes[2], 1)
	require.InDelta(t, laneCooldownTransient.Seconds(), writer.writes[1][0].Seconds(), 1.0)
}

// 终态（调用者内容问题）不换号：首号直接写完应答，二号不被触碰。
func TestServeWithFailoverTerminalNoRotate(t *testing.T) {
	writer := newFakeCooldownWriter()
	pool := NewPool(nil, writer)
	candidates := []*service.Account{
		{ID: 1, Platform: domain.PlatformSerpAPI, Status: service.StatusActive},
		{ID: 2, Platform: domain.PlatformSerpAPI, Status: service.StatusActive},
	}
	visited := []int64{}
	w := serveFailoverlane(t, pool, candidates, func(c *gin.Context, acc *service.Account) (bool, *upstreamFailure) {
		visited = append(visited, acc.ID)
		laneUpstreamHTTPError(c, LaneRerank, http.StatusBadRequest)
		return true, nil
	})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, []int64{1}, visited)
	require.Empty(t, writer.writes[1])
}

// 缺凭据账号：长冷却+换号；全缺 → misconfigured 502 形状。
func TestServeWithFailoverMisconfigured(t *testing.T) {
	writer := newFakeCooldownWriter()
	pool := NewPool(nil, writer)
	candidates := []*service.Account{{ID: 5, Platform: domain.PlatformSerpAPI, Status: service.StatusActive}}
	w := serveFailoverlane(t, pool, candidates, func(*gin.Context, *service.Account) (bool, *upstreamFailure) {
		return false, &upstreamFailure{misconfigured: true}
	})
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "missing credentials")
	require.Len(t, writer.writes[5], 1)
	require.InDelta(t, laneCooldownExhausting.Seconds(), writer.writes[5][0].Seconds(), 1.0)
}
