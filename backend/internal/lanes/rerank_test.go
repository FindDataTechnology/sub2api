package lanes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// rerank 契约（tasks 3.1）：Cohere/Jina 形状校验、上游转发保真（含鉴权头与
// model 默认化）、响应透传、top_n 透传、上游失败 502 不计费、成功计量入参形状。

type rerankUpstreamCapture struct {
	mu           sync.Mutex
	authHeader   string
	body         map[string]any
	responseCode int
	responseBody string
}

func newRerankFakeUpstream(t *testing.T, capture *rerankUpstreamCapture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.mu.Lock()
		capture.authHeader = r.Header.Get("Authorization")
		_ = json.Unmarshal(body, &capture.body)
		code := capture.responseCode
		respBody := capture.responseBody
		capture.mu.Unlock()
		if code == 0 {
			code = http.StatusOK
			respBody = `{"id":"rrk-1","model":"BAAI/bge-reranker-v2-m3","usage":{"prompt_tokens":42},"results":[{"index":1,"relevance_score":0.98},{"index":0,"relevance_score":0.12}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func newRerankTestApp(t *testing.T, capture *rerankUpstreamCapture, upstream *httptest.Server, recorded chan *service.OpenAIRecordUsageInput) *Lanes {
	gin.SetMode(gin.TestMode)
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 21, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{
				"api_key":  "sk-rerank-test",
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
			LaneRerank: NewRerankAdapter(),
		},
	}
}

func serveRerank(t *testing.T, app *Lanes, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
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

func TestRerankLaneHappyPath(t *testing.T) {
	capture := &rerankUpstreamCapture{}
	upstream := newRerankFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newRerankTestApp(t, capture, upstream, recorded)

	w := serveRerank(t, app, `{"query":"什么是 sub2api","documents":["文档一","文档二"],"top_n":2}`)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "application/json")

	// 上游收到：默认化的 model + 原样 query/documents/top_n + 池账号凭据。
	capture.mu.Lock()
	require.Equal(t, "Bearer sk-rerank-test", capture.authHeader)
	require.Equal(t, "什么是 sub2api", capture.body["query"])
	require.Equal(t, []any{"文档一", "文档二"}, capture.body["documents"])
	require.Equal(t, float64(2), capture.body["top_n"])
	require.Equal(t, rerankDefaultUpstreamModel, capture.body["model"])
	capture.mu.Unlock()

	// 响应透传（排序形状保真）。
	var resp struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 2)
	require.Equal(t, 1, resp.Results[0].Index)
	require.InDelta(t, 0.98, resp.Results[0].RelevanceScore, 1e-9)

	// 计量入参：RerankRequests=1、模型、端点、账号。
	select {
	case input := <-recorded:
		require.Equal(t, 1, input.Result.RerankRequests)
		require.Equal(t, rerankDefaultUpstreamModel, input.Result.Model)
		require.Equal(t, int64(21), input.Account.ID)
		require.Equal(t, "/v1/rerank", input.InboundEndpoint)
		require.Equal(t, siliconflowRerankPath, input.UpstreamEndpoint)
	case <-time.After(2 * time.Second):
		t.Fatal("rerank usage was not recorded")
	}
}

func TestRerankLaneRequestValidation(t *testing.T) {
	capture := &rerankUpstreamCapture{}
	upstream := newRerankFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newRerankTestApp(t, capture, upstream, recorded)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing query", `{"documents":["a"]}`, "`query`"},
		{"missing documents", `{"query":"q"}`, "`documents`"},
		{"empty documents", `{"query":"q","documents":[]}`, "`documents`"},
		{"object documents v1 unsupported", `{"query":"q","documents":[{"text":"a"}]}`, "array of strings"},
		{"non-json body", `not-json`, "JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveRerank(t, app, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), tc.want)
			require.Contains(t, w.Body.String(), LaneRerank)
		})
	}
	select {
	case <-recorded:
		t.Fatal("invalid requests must not be billed")
	default:
	}
}

func TestRerankLaneUpstreamFailureNotBilled(t *testing.T) {
	capture := &rerankUpstreamCapture{responseCode: http.StatusInternalServerError, responseBody: `{"error":{"message":"boom","code":5001}}`}
	upstream := newRerankFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newRerankTestApp(t, capture, upstream, recorded)

	w := serveRerank(t, app, `{"query":"q","documents":["a"]}`)

	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "lane_upstream_error")
	require.Contains(t, w.Body.String(), LaneRerank)
	// 无上游泄露：上游错误体不透传。
	require.NotContains(t, w.Body.String(), "boom")
	require.NotContains(t, w.Body.String(), "5001")
	select {
	case <-recorded:
		t.Fatal("failed upstream calls must not be billed")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRerankLaneModelPassthrough(t *testing.T) {
	capture := &rerankUpstreamCapture{}
	upstream := newRerankFakeUpstream(t, capture)
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := newRerankTestApp(t, capture, upstream, recorded)

	w := serveRerank(t, app, `{"model":"Qwen/Qwen3-Reranker-8B","query":"q","documents":["a"]}`)
	require.Equal(t, http.StatusOK, w.Code)

	capture.mu.Lock()
	require.Equal(t, "Qwen/Qwen3-Reranker-8B", capture.body["model"])
	capture.mu.Unlock()

	select {
	case input := <-recorded:
		require.Equal(t, "Qwen/Qwen3-Reranker-8B", input.Result.Model)
	case <-time.After(2 * time.Second):
		t.Fatal("usage was not recorded")
	}
}

// 轮转（ciyuan-lane-pool-failover 3.1）：首号 402 → 冷却+换二号线成功；
// 单号 402 全池尽 → 502 不计费。
func TestRerankLaneFailoverToSecondAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	servedKeys := []string{}
	// 按 api_key 分流：sk-a 恒 402（硅基余额类），sk-b 正常回。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		servedKeys = append(servedKeys, auth)
		mu.Unlock()
		if strings.HasSuffix(auth, "sk-a") {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient balance"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.9}]}`))
	}))
	t.Cleanup(upstream.Close)

	writer := newFakeCooldownWriter()
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 1, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive, Priority: 1,
			Credentials: map[string]any{"api_key": "sk-a", "base_url": upstream.URL}},
		{ID: 2, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive, Priority: 2,
			Credentials: map[string]any{"api_key": "sk-b", "base_url": upstream.URL}},
	}}
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := &Lanes{
		Meter: &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}},
		Pool: NewPool(lister, writer),
		adapters: map[string]LaneAdapter{
			LaneRerank: NewRerankAdapter(),
		},
	}

	w := serveRerank(t, app, `{"query":"q","documents":["a"]}`)

	require.Equal(t, http.StatusOK, w.Code)
	mu.Lock()
	require.Len(t, servedKeys, 2, "both accounts must be tried in order")
	mu.Unlock()
	// 冷却只落首号（额度类 12h）。
	require.Len(t, writer.writes[1], 1)
	require.InDelta(t, laneCooldownExhausting.Seconds(), writer.writes[1][0].Seconds(), 1.0)
	require.Empty(t, writer.writes[2])
	// 计量归因二号线。
	select {
	case input := <-recorded:
		require.Equal(t, int64(2), input.Account.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("usage was not recorded")
	}
}

func TestRerankLaneExhaustedPoolNotBilled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient balance"}}`))
	}))
	t.Cleanup(upstream.Close)

	writer := newFakeCooldownWriter()
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 1, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive,
			Credentials: map[string]any{"api_key": "sk-a", "base_url": upstream.URL}},
	}}
	recorded := make(chan *service.OpenAIRecordUsageInput, 1)
	app := &Lanes{
		Meter: &Meter{recordUsage: func(_ context.Context, input *service.OpenAIRecordUsageInput) error {
			recorded <- input
			return nil
		}},
		Pool: NewPool(lister, writer),
		adapters: map[string]LaneAdapter{
			LaneRerank: NewRerankAdapter(),
		},
	}

	w := serveRerank(t, app, `{"query":"q","documents":["a"]}`)
	require.Equal(t, http.StatusBadGateway, w.Code)
	require.Contains(t, w.Body.String(), "lane_upstream_error")
	require.NotContains(t, w.Body.String(), "Insufficient")
	select {
	case <-recorded:
		t.Fatal("exhausted pool must not be billed")
	case <-time.After(200 * time.Millisecond):
	}
}
