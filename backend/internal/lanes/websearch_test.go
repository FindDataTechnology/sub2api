package lanes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// websearch 契约（tasks 3.3）：统一信封形状、engine 切换零改造、未知 engine
// 拒绝（含可用列表、不计费）、零结果合法空集、上游失败 502 不泄露不计费、
// num 夹取、透传白名单、成功计量入参。

type websearchUpstreamCapture struct {
	mu           sync.Mutex
	query        url.Values
	responseCode int
	responseBody string
}

func newWebsearchFakeUpstream(t *testing.T, capture *websearchUpstreamCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		capture.mu.Lock()
		capture.query = r.URL.Query()
		code := capture.responseCode
		respBody := capture.responseBody
		capture.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
			if respBody == "" {
				respBody = `{"search_metadata":{"status":"Success"},"organic_results":[{"position":1,"title":"壹座","link":"https://paas.example/1","snippet":"平台一"},{"position":2,"title":"词元","link":"https://paas.example/2","snippet":"平台二"},{"position":3,"title":"第三","link":"https://paas.example/3","snippet":"三"}]}`
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func newWebsearchTestApp(upstream *httptest.Server, recorded chan *service.OpenAIRecordUsageInput) *Lanes {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 31, Platform: domain.PlatformSerpAPI, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{
				"api_key":  "serpapi-test-key",
				"base_url": upstream.URL,
			}},
	}}
	meter := &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
		recorded <- input
		return nil
	}}
	return &Lanes{
		Meter: meter,
		Pool:  NewPool(lister, nil),
		adapters: map[string]LaneAdapter{
			LaneWebsearch: NewWebsearchAdapter(),
		},
	}
}

func serveWebsearch(t *testing.T, app *Lanes, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router := gin.New()
	group := router.Group("/v1")
	group.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
			ID: 100, User: &service.User{ID: 7}, Group: &service.Group{ID: 42, Platform: service.PlatformOpenAI},
		})
		c.Next()
	})
	RegisterGatewayLanes(group, app)
	router.ServeHTTP(w, req)
	return w
}

func TestWebsearchLaneEnvelopeShape(t *testing.T) {
	capture := &websearchUpstreamCapture{}
	upstream := newWebsearchFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newWebsearchTestApp(upstream, recorded)

	w := serveWebsearch(t, app, `{"engine":"serpapi","q":"壹座 平台","num":2}`)
	require.Equal(t, http.StatusOK, w.Code)

	// 统一信封：meta（engine/latency）+归一化 results（title/url/snippet/position）+usage。
	var env struct {
		Meta struct {
			Engine    string `json:"engine"`
			LatencyMs int64  `json:"latency_ms"`
		} `json:"meta"`
		Results []struct {
			Title    string `json:"title"`
			URL      string `json:"url"`
			Snippet  string `json:"snippet"`
			Position int    `json:"position"`
		} `json:"results"`
		Usage struct {
			Requests int `json:"requests"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.Equal(t, "serpapi", env.Meta.Engine)
	require.GreaterOrEqual(t, env.Meta.LatencyMs, int64(0))
	require.Equal(t, 1, env.Usage.Requests)

	// num=2 夹取生效（上游给了 3 条）。
	require.Len(t, env.Results, 2)
	require.Equal(t, "壹座", env.Results[0].Title)
	require.Equal(t, "https://paas.example/1", env.Results[0].URL)
	require.Equal(t, "平台一", env.Results[0].Snippet)
	require.Equal(t, 1, env.Results[0].Position)
	require.Equal(t, 2, env.Results[1].Position)

	// 上游收到：google_light 引擎 + q + num + api_key；透传参数随行、白名单外忽略。
	capture.mu.Lock()
	require.Equal(t, serpAPILightEngine, capture.query.Get("engine"))
	require.Equal(t, "壹座 平台", capture.query.Get("q"))
	require.Equal(t, "2", capture.query.Get("num"))
	require.Equal(t, "serpapi-test-key", capture.query.Get("api_key"))
	capture.mu.Unlock()

	// 计量入参：WebSearchCalls=1（按次）、模型/端点标注。
	select {
	case input := <-recorded:
		require.Equal(t, 1, input.Result.WebSearchCalls)
		require.Equal(t, "websearch:serpapi", input.Result.Model)
		require.Equal(t, "serpapi:google_light", input.Result.UpstreamModel)
		require.Equal(t, int64(31), input.Account.ID)
		require.Equal(t, "/v1/search", input.InboundEndpoint)
		require.Equal(t, serpAPISearchPath, input.UpstreamEndpoint)
	case <-time.After(2 * time.Second):
		t.Fatal("websearch usage was not recorded")
	}
}

func TestWebsearchLaneEngineSelection(t *testing.T) {
	capture := &websearchUpstreamCapture{}
	upstream := newWebsearchFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newWebsearchTestApp(upstream, recorded)

	t.Run("defaults to serpapi when engine omitted", func(t *testing.T) {
		w := serveWebsearch(t, app, `{"q":"无引擎字段"}`)
		require.Equal(t, http.StatusOK, w.Code)
		require.Contains(t, w.Body.String(), `"engine":"serpapi"`)
		// 取走这笔成功计费记录，避免污染后续「拒绝不计费」的空通道断言。
		select {
		case <-recorded:
		case <-time.After(2 * time.Second):
			t.Fatal("default engine search was not billed")
		}
	})

	t.Run("unknown engine rejected with available list and not billed", func(t *testing.T) {
		w := serveWebsearch(t, app, `{"engine":"google","q":"x"}`)
		require.Equal(t, http.StatusBadRequest, w.Code)
		var body struct {
			Error struct {
				Type             string   `json:"type"`
				Lane             string   `json:"lane"`
				AvailableEngines []string `json:"available_engines"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "lane_invalid_engine", body.Error.Type)
		require.Equal(t, LaneWebsearch, body.Error.Lane)
		require.Contains(t, body.Error.AvailableEngines, "serpapi")
	})
	select {
	case <-recorded:
		t.Fatal("rejected engine requests must not be billed")
	default:
	}
}

func TestWebsearchLaneRequestValidation(t *testing.T) {
	capture := &websearchUpstreamCapture{}
	upstream := newWebsearchFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newWebsearchTestApp(upstream, recorded)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing q", `{"engine":"serpapi"}`, "`q`"},
		{"blank q", `{"q":"   "}`, "`q`"},
		{"non-integer num", `{"q":"x","num":2.5}`, "`num`"},
		{"non-json", `[]`, "JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveWebsearch(t, app, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), tc.want)
		})
	}
	select {
	case <-recorded:
		t.Fatal("invalid requests must not be billed")
	default:
	}
}

func TestWebsearchLaneZeroResultsIsValid(t *testing.T) {
	capture := &websearchUpstreamCapture{responseBody: `{"error":"Google hasn't returned any results for this query."}`}
	upstream := newWebsearchFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newWebsearchTestApp(upstream, recorded)

	w := serveWebsearch(t, app, `{"q":"没有结果的短语"}`)
	require.Equal(t, http.StatusOK, w.Code) // 零结果=合法空集（上游额度已消耗，照常计费）
	require.Contains(t, w.Body.String(), `"results":[]`)
	select {
	case input := <-recorded:
		require.Equal(t, 1, input.Result.WebSearchCalls)
	case <-time.After(2 * time.Second):
		t.Fatal("zero-result search still consumes upstream quota and must be billed")
	}
}

func TestWebsearchLaneUpstreamFailureNotBilled(t *testing.T) {
	capture := &websearchUpstreamCapture{responseCode: http.StatusUnauthorized, responseBody: `{"error":"Invalid API key: abcdef123456"}`}
	upstream := newWebsearchFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newWebsearchTestApp(upstream, recorded)

	w := serveWebsearch(t, app, `{"q":"x"}`)
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "lane_upstream_error")
	// 无上游泄露：错误文本/api key 片段不透传。
	require.NotContains(t, w.Body.String(), "Invalid API key")
	require.NotContains(t, w.Body.String(), "abcdef123456")
	select {
	case <-recorded:
		t.Fatal("failed upstream calls must not be billed")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWebsearchLanePassthroughWhitelist(t *testing.T) {
	capture := &websearchUpstreamCapture{}
	upstream := newWebsearchFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newWebsearchTestApp(upstream, recorded)

	w := serveWebsearch(t, app, `{"q":"x","gl":"cn","hl":"zh-cn","api_key":"caller-key","engine":"serpapi"}`)
	require.Equal(t, http.StatusOK, w.Code)

	capture.mu.Lock()
	require.Equal(t, "cn", capture.query.Get("gl"))
	require.Equal(t, "zh-cn", capture.query.Get("hl"))
	// 调用者不能覆盖 api_key 控制参数（白名单外）。
	require.Equal(t, "serpapi-test-key", capture.query.Get("api_key"))
	capture.mu.Unlock()
}

// ── BYOK 三态（ciyuan-lane-pool-failover 4.1/4.2）────────────────────────────
// 按 api_key 分流的上游桩：user-key 成功/user-key 失败/pool-key 正常 三路。
func newWebsearchBYOKUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("api_key")
		mu.Lock()
		*seen = append(*seen, key)
		mu.Unlock()
		switch key {
		case "user-good-key":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"organic_results":[{"title":"用户key结果","link":"https://u.example/1","snippet":"来自用户 key"}]}`))
		case "user-bad-key":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid API key. Please check your account."}`))
		default: // 池账号 key
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"organic_results":[{"title":"池结果","link":"https://p.example/1","snippet":"来自池"}]}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func newWebsearchBYOKApp(upstream *httptest.Server, recorded chan *service.OpenAIRecordUsageInput, writer *fakeCooldownWriter) *Lanes {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 31, Platform: domain.PlatformSerpAPI, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{"api_key": "pool-key", "base_url": upstream.URL}},
	}}
	return &Lanes{
		Meter: &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}},
		Pool: NewPool(lister, writer),
		adapters: map[string]LaneAdapter{
			LaneWebsearch: NewWebsearchAdapter(),
		},
	}
}

func serveWebsearchWithBYOK(t *testing.T, app *Lanes, userKey string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"engine":"serpapi","q":"测试","num":3}`))
	req.Header.Set("Content-Type", "application/json")
	if userKey != "" {
		req.Header.Set(websearchBYOKHeader, userKey)
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
	router.ServeHTTP(w, req)
	return w
}

// 状态①用户 key 优先：user 来源标注 + 0 费计量（byok: 归因 + WebSearchCalls=1 + AccountID=0）。
func TestWebsearchBYOKUserKeyServes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream, seen := newWebsearchBYOKUpstream(t)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	writer := newFakeCooldownWriter()
	app := newWebsearchBYOKApp(upstream, recorded, writer)

	w := serveWebsearchWithBYOK(t, app, "user-good-key")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, websearchSourceUser, w.Header().Get("x-search-source"))
	require.Contains(t, w.Body.String(), "用户key结果")

	select {
	case input := <-recorded:
		require.Equal(t, 1, input.Result.WebSearchCalls)
		// 账号位锚定池首号（usage_logs.account_id 外键约束，v0.2.16-5 线上实证）；
		// 归因真相在 UpstreamModel 前缀。
		require.Equal(t, int64(31), input.Account.ID)
		require.True(t, strings.HasPrefix(input.Result.UpstreamModel, websearchBYOKMarker), "归因=byok:<hash8>")
	case <-time.After(2 * time.Second):
		t.Fatal("byok usage was not recorded")
	}
	// 池 key 不被消耗。
	for _, k := range *seen {
		require.Equal(t, "user-good-key", k)
	}
}

// 状态②用户 key 失败回退池：pool-after-user-failure 如实标注 + 正常计费 + hash8 冷却。
func TestWebsearchBYOKFallbackToPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream, _ := newWebsearchBYOKUpstream(t)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	writer := newFakeCooldownWriter()
	app := newWebsearchBYOKApp(upstream, recorded, writer)

	w := serveWebsearchWithBYOK(t, app, "user-bad-key")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, websearchSourcePoolAfterUserFailure, w.Header().Get("x-search-source"))
	require.Contains(t, w.Body.String(), "池结果")

	// 用户 key（401=额度/auth 类）进 12h 冷却。
	require.True(t, byokCooling(byokTagOf("user-bad-key")))

	select {
	case input := <-recorded:
		require.Equal(t, int64(31), input.Account.ID, "回退池笔归因池账号")
		require.Equal(t, 1, input.Result.WebSearchCalls)
		require.Equal(t, "serpapi:google_light", input.Result.UpstreamModel)
	case <-time.After(2 * time.Second):
		t.Fatal("fallback usage was not recorded")
	}
}

// 状态③冷却中的用户 key 直接走池（不再打用户 key），来源仍如实标注。
func TestWebsearchBYOKCoolingSkipsUserKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream, seen := newWebsearchBYOKUpstream(t)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	writer := newFakeCooldownWriter()
	app := newWebsearchBYOKApp(upstream, recorded, writer)

	byokSetCooldown(byokTagOf("user-good-key"), laneCooldownExhausting) // 预置冷却
	w := serveWebsearchWithBYOK(t, app, "user-good-key")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, websearchSourcePoolAfterUserFailure, w.Header().Get("x-search-source"))
	require.Contains(t, w.Body.String(), "池结果")
	for _, k := range *seen {
		require.Equal(t, "pool-key", k, "cooling user key must not be tried")
	}
}

// key 值不泄露：错误体/响应不含用户 key 原文；归因仅 hash8。
func TestWebsearchBYOKKeyNeverLeaked(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream, _ := newWebsearchBYOKUpstream(t)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	writer := newFakeCooldownWriter()
	app := newWebsearchBYOKApp(upstream, recorded, writer)

	w := serveWebsearchWithBYOK(t, app, "user-bad-key")
	require.NotContains(t, w.Body.String(), "user-bad-key")
	select {
	case input := <-recorded:
		require.NotContains(t, input.Result.UpstreamModel, "user-bad-key")
	case <-time.After(2 * time.Second):
	}
	require.Equal(t, 8, len(byokTagOf("user-bad-key")), "归因=8 hex 字符 hash 前缀")
}
