package lanes

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 三车道上游故障统一口径（tasks 3.4）：断连 / 超时 / 异常状态码 × 三车道，
// 断言同一形状（error.type+message+lane+engine）、无上游泄露、不计费。

// laneCase 一条车道的「最小合法请求」及其池平台。
type laneCase struct {
	lane     string
	platform string
	path     string
	content  string
	body     []byte
	// upstreamIsJSON 标记该车道上游是 JSON 响应（TTS 是二进制）。
	isTTS bool
}

func laneCases() []laneCase {
	return []laneCase{
		{
			lane: LaneRerank, platform: domain.PlatformSiliconFlow, path: "/v1/rerank",
			content: "application/json", body: []byte(`{"query":"q","documents":["a"]}`),
		},
		{
			lane: LaneWebsearch, platform: domain.PlatformSerpAPI, path: "/v1/search",
			content: "application/json", body: []byte(`{"q":"q"}`),
		},
		{
			lane: LaneAudio, platform: domain.PlatformOpenAINext, path: "/v1/audio/speech",
			content: "application/json", body: []byte(`{"input":"x"}`), isTTS: true,
		},
	}
}

// newFaultRouter 装配只含一个车道适配器的路由，上游指向给定 URL（可为 unreachable）。
func newFaultRouter(lane, platform, upstreamURL string, recorded chan *service.OpenAIRecordUsageInput, clientTimeout time.Duration) *gin.Engine {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 51, Platform: platform, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-fault-test", "base_url": upstreamURL}},
	}}
	meter := &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
		recorded <- input
		return nil
	}}
	app := &Lanes{
		Meter: meter,
		Pool:  NewPool(lister, nil),
		adapters: map[string]LaneAdapter{
			LaneRerank:    NewRerankAdapter(),
			LaneWebsearch: NewWebsearchAdapter(),
			LaneAudio:     NewAudioAdapter(),
		},
	}
	if clientTimeout > 0 {
		// 覆盖默认超时以让「超时」用例秒级结束（不真等 30s/120s）。
		app.adapters[LaneRerank] = &RerankAdapter{client: &http.Client{Timeout: clientTimeout}}
		app.adapters[LaneWebsearch] = &WebsearchAdapter{client: &http.Client{Timeout: clientTimeout}}
		app.adapters[LaneAudio] = &AudioAdapter{client: &http.Client{Timeout: clientTimeout}}
	}
	router := gin.New()
	group := router.Group("/v1")
	group.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
			ID: 100, User: &service.User{ID: 7}, Group: &service.Group{ID: 42, Platform: service.PlatformOpenAI},
		})
		c.Next()
	})
	RegisterGatewayLanes(group, app)
	_ = lane
	return router
}

// assertLaneErrorShape 统一形状断言：type/message/lane/engine 齐备。
func assertLaneErrorShape(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantType, lane string) {
	t.Helper()
	require.Equalf(t, wantStatus, w.Code, "lane=%s body=%s", lane, w.Body.String())
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Lane    string `json:"lane"`
			Engine  string `json:"engine"`
		} `json:"error"`
	}
	require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &body), "lane=%s body=%s", lane, w.Body.String())
	require.Equalf(t, wantType, body.Error.Type, "lane=%s", lane)
	require.Equalf(t, lane, body.Error.Lane, "lane=%s", lane)
	require.Equalf(t, LaneEngine(lane), body.Error.Engine, "lane=%s engine must be annotated", lane)
	require.NotEmptyf(t, body.Error.Message, "lane=%s", lane)
}

func TestLaneUpstreamFaultsAreUniform(t *testing.T) {
	// 1) 断连：上游端口无人监听（连接被拒）。
	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		deadURL := "http://" + ln.Addr().String()
		_ = ln.Close() // 释放端口 → 连接必被拒

		for _, lc := range laneCases() {
			recorded := make(chan *service.OpenAIRecordUsageInput, 1)
			router := newFaultRouter(lc.lane, lc.platform, deadURL, recorded, 2*time.Second)
			req := httptest.NewRequest(http.MethodPost, lc.path, bytesReader(lc.body))
			req.Header.Set("Content-Type", lc.content)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assertLaneErrorShape(t, w, http.StatusBadGateway, "lane_upstream_error", lc.lane)
			assertNotBilled(t, recorded, lc.lane)
		}
	})

	// 2) 超时：上游挂住不响应，客户端超时先到。
	t.Run("upstream timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(3 * time.Second)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		for _, lc := range laneCases() {
			recorded := make(chan *service.OpenAIRecordUsageInput, 1)
			router := newFaultRouter(lc.lane, lc.platform, server.URL, recorded, 200*time.Millisecond)
			req := httptest.NewRequest(http.MethodPost, lc.path, bytesReader(lc.body))
			req.Header.Set("Content-Type", lc.content)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			assertLaneErrorShape(t, w, http.StatusBadGateway, "lane_upstream_error", lc.lane)
			assertNotBilled(t, recorded, lc.lane)
		}
	})

	// 3) 异常状态码：401/429/500 → 502（上游侧故障）；400/422 → 400（内容问题）。
	t.Run("upstream error status codes", func(t *testing.T) {
		for _, lc := range laneCases() {
			for _, tc := range []struct {
				upstreamStatus int
				wantStatus     int
				wantType       string
			}{
				{http.StatusUnauthorized, http.StatusBadGateway, "lane_upstream_error"},
				{http.StatusTooManyRequests, http.StatusBadGateway, "lane_upstream_error"},
				{http.StatusInternalServerError, http.StatusBadGateway, "lane_upstream_error"},
				{http.StatusBadRequest, http.StatusBadRequest, "invalid_request_error"},
				{http.StatusUnprocessableEntity, http.StatusBadRequest, "invalid_request_error"},
			} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					// 上游错误体含账户信息：必须不透传。
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.upstreamStatus)
					_, _ = w.Write([]byte(`{"error":"Invalid API key: sk-SECRET-12345 for account acct-9"}`))
				}))
				recorded := make(chan *service.OpenAIRecordUsageInput, 1)
				router := newFaultRouter(lc.lane, lc.platform, server.URL, recorded, 0)

				req := httptest.NewRequest(http.MethodPost, lc.path, bytesReader(lc.body))
				req.Header.Set("Content-Type", lc.content)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				assertLaneErrorShape(t, w, tc.wantStatus, tc.wantType, lc.lane)
				// 无上游泄露：错误文本与凭据片段都不入响应。
				require.NotContainsf(t, w.Body.String(), "Invalid API key", "lane=%s", lc.lane)
				require.NotContainsf(t, w.Body.String(), "sk-SECRET-12345", "lane=%s", lc.lane)
				require.NotContainsf(t, w.Body.String(), "acct-9", "lane=%s", lc.lane)
				assertNotBilled(t, recorded, lc.lane)
				_ = server.Close()
			}
		}
	})
}

// 池空（无可用上游）与上游故障是两件事：503 vs 502。
func TestLanePoolEmptyIsDistinctFromUpstreamFault(t *testing.T) {
	for _, lc := range laneCases() {
		recorded := make(chan *service.OpenAIRecordUsageInput, 1)
		// 池里给一个非 active 账号 → Pick 报池空。
		lister := &fakeLaneAccountLister{accounts: []service.Account{
			{ID: 61, Platform: lc.platform, Status: service.StatusDisabled, Credentials: map[string]any{"api_key": "k"}},
		}}
		meter := &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}}
		app := &Lanes{Meter: meter, Pool: NewPool(lister, nil), adapters: map[string]LaneAdapter{
			LaneRerank: NewRerankAdapter(), LaneWebsearch: NewWebsearchAdapter(), LaneAudio: NewAudioAdapter(),
		}}
		router := gin.New()
		group := router.Group("/v1")
		group.Use(func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 100, User: &service.User{ID: 7}, Group: &service.Group{ID: 42, Platform: service.PlatformOpenAI},
			})
			c.Next()
		})
		RegisterGatewayLanes(group, app)

		req := httptest.NewRequest(http.MethodPost, lc.path, bytesReader(lc.body))
		req.Header.Set("Content-Type", lc.content)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assertLaneErrorShape(t, w, http.StatusServiceUnavailable, "lane_upstream_unavailable", lc.lane)
		assertNotBilled(t, recorded, lc.lane)
	}
}

// 无凭据账号=车道侧配置错误：仍 502（运行问题）、不暴露账号细节、不计费。
func TestLaneUpstreamMissingCredentialsIsNotLeaked(t *testing.T) {
	for _, lc := range laneCases() {
		recorded := make(chan *service.OpenAIRecordUsageInput, 1)
		lister := &fakeLaneAccountLister{accounts: []service.Account{
			{ID: 71, Platform: lc.platform, Status: service.StatusActive, Credentials: map[string]any{}},
		}}
		meter := &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}}
		app := &Lanes{Meter: meter, Pool: NewPool(lister, nil), adapters: map[string]LaneAdapter{
			LaneRerank: NewRerankAdapter(), LaneWebsearch: NewWebsearchAdapter(), LaneAudio: NewAudioAdapter(),
		}}
		router := gin.New()
		group := router.Group("/v1")
		group.Use(func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
				ID: 100, User: &service.User{ID: 7}, Group: &service.Group{ID: 42, Platform: service.PlatformOpenAI},
			})
			c.Next()
		})
		RegisterGatewayLanes(group, app)

		req := httptest.NewRequest(http.MethodPost, lc.path, bytesReader(lc.body))
		req.Header.Set("Content-Type", lc.content)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assertLaneErrorShape(t, w, http.StatusBadGateway, "lane_upstream_error", lc.lane)
		require.Containsf(t, w.Body.String(), "missing credentials", "lane=%s", lc.lane)
		require.NotContainsf(t, w.Body.String(), "71", "account id must not leak: lane=%s", lc.lane)
		assertNotBilled(t, recorded, lc.lane)
	}
}

func assertNotBilled(t *testing.T, recorded chan *service.OpenAIRecordUsageInput, lane string) {
	t.Helper()
	select {
	case <-recorded:
		t.Fatalf("lane=%s: failed requests must not be billed", lane)
	case <-time.After(150 * time.Millisecond):
	}
}
