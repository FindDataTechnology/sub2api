package lanes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// SerpAPI 上游常量（tasks 3.3；引擎参数与结果切片口径对齐平台 search-relay
// 既有生产实现 services/search-relay/index.js）。
const (
	serpAPIDefaultBaseURL  = "https://serpapi.com"
	serpAPISearchPath      = "/search.json"
	serpAPILightEngine     = "google_light"
	websearchEngineSerpAPI = "serpapi"
	websearchDefaultNum    = 10
	websearchMinNum        = 1
	websearchMaxNum        = 20
)

// websearchEngines 已接入 engine 集合（v1：serpapi）。统一协议里 engine=上游供应
// 商选择器；换 engine 调用者零改造（差异吞在适配器内）。
var websearchEngines = []string{websearchEngineSerpAPI}

// serpAPIZeroResultRe 零结果=合法空集（对齐 search-relay：不能当 key 失败）。
var serpAPIZeroResultRe = regexp.MustCompile(`hasn't returned any results`)

// websearchPassthroughParams 允许透传给 SerpAPI 的 engine 侧参数（白名单——
// 防调用者覆盖 api_key/output 等控制参数）。
var websearchPassthroughParams = []string{
	"gl", "hl", "location", "uule", "google_domain", "safe",
}

// WebsearchAdapter 搜索车道适配器：自有统一协议（D3）——请求
// `{engine, q, num, ...透传}`，响应统一信封 `{meta, results[], usage}`；
// 上游差异（SerpAPI 的 google_light/organic_results）全吞在这一层。
type WebsearchAdapter struct {
	client *http.Client
}

func NewWebsearchAdapter() *WebsearchAdapter {
	return &WebsearchAdapter{client: &http.Client{Timeout: 30 * time.Second}}
}

type websearchResult struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Position int    `json:"position"`
}

type websearchEnvelope struct {
	Meta struct {
		Engine    string `json:"engine"`
		LatencyMs int64  `json:"latency_ms"`
	} `json:"meta"`
	Results []websearchResult `json:"results"`
	Usage   struct {
		Requests int `json:"requests"`
	} `json:"usage"`
}

func unknownWebsearchEngine(c *gin.Context, engine string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{
			"type":              "lane_invalid_engine",
			"message":           fmt.Sprintf("unknown search engine %q; available engines: %s", engine, strings.Join(websearchEngines, ", ")),
			"lane":              LaneWebsearch,
			"available_engines": websearchEngines,
		},
	})
}

// websearchNum 解析并夹取 num（默认 10，界 1..20）。
func websearchNum(raw any) (int, bool) {
	if raw == nil {
		return websearchDefaultNum, true
	}
	f, ok := raw.(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	n := int(f)
	if n < websearchMinNum {
		n = websearchMinNum
	}
	if n > websearchMaxNum {
		n = websearchMaxNum
	}
	return n, true
}

func (a *WebsearchAdapter) Serve(c *gin.Context, apiKey *service.APIKey, account *service.Account, meter *Meter) {
	started := time.Now()
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		laneBadRequest(c, LaneWebsearch, "failed to read request body")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		laneBadRequest(c, LaneWebsearch, "request body must be a JSON object ({\"engine\",\"q\",\"num\"})")
		return
	}
	engine, _ := req["engine"].(string)
	engine = strings.ToLower(strings.TrimSpace(engine))
	if engine == "" {
		// v1 单引擎：未提供时默认 serpapi（集合扩容后仍兼容显式传参调用者）。
		engine = websearchEngineSerpAPI
	}
	if engine != websearchEngineSerpAPI {
		unknownWebsearchEngine(c, engine)
		return
	}
	q, _ := req["q"].(string)
	if strings.TrimSpace(q) == "" {
		laneBadRequest(c, LaneWebsearch, "field `q` is required and must be a non-empty string")
		return
	}
	num, ok := websearchNum(req["num"])
	if !ok {
		laneBadRequest(c, LaneWebsearch, "field `num` must be an integer")
		return
	}

	apiKeyStr, _ := account.Credentials["api_key"].(string)
	if apiKeyStr == "" {
		// 池里进了无凭据账号：车道侧配置错误（502、不计费、不暴露账号细节）。
		laneUpstreamMisconfigured(c, LaneWebsearch)
		return
	}
	baseURL, _ := account.Credentials["base_url"].(string)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = serpAPIDefaultBaseURL
	}

	params := url.Values{}
	params.Set("engine", serpAPILightEngine)
	params.Set("q", q)
	params.Set("num", strconv.Itoa(num))
	params.Set("api_key", apiKeyStr)
	for _, key := range websearchPassthroughParams {
		if v, exists := req[key]; exists {
			params.Set(key, fmt.Sprint(v))
		}
	}
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, strings.TrimRight(baseURL, "/")+serpAPISearchPath+"?"+params.Encode(), nil)
	if err != nil {
		laneUpstreamError(c, LaneWebsearch, 0)
		return
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		laneUpstreamError(c, LaneWebsearch, 0)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		laneUpstreamHTTPError(c, LaneWebsearch, resp.StatusCode)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		laneUpstreamHTTPError(c, LaneWebsearch, resp.StatusCode)
		return
	}
	var parsed struct {
		Error          string `json:"error"`
		OrganicResults []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic_results"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		laneUpstreamHTTPError(c, LaneWebsearch, resp.StatusCode)
		return
	}
	if parsed.Error != "" && !serpAPIZeroResultRe.MatchString(parsed.Error) {
		// 上游业务错误（无效 key/额度耗尽等）：不外泄错误文本（可能含账户信息）。
		laneUpstreamHTTPError(c, LaneWebsearch, resp.StatusCode)
		return
	}
	// 零结果=合法空集（对齐 search-relay 口径），照常成 200 并计费（上游额度已消耗）。

	var env websearchEnvelope
	env.Meta.Engine = engine
	env.Meta.LatencyMs = time.Since(started).Milliseconds()
	env.Results = make([]websearchResult, 0, num)
	for i, r := range parsed.OrganicResults {
		if i >= num {
			break
		}
		env.Results = append(env.Results, websearchResult{
			Title:    r.Title,
			URL:      r.Link,
			Snippet:  r.Snippet,
			Position: i + 1,
		})
	}
	env.Usage.Requests = 1
	c.JSON(http.StatusOK, env)

	if meter != nil {
		meter.WriteUsage(c, apiKey, UsageInput{
			Lane:             LaneWebsearch,
			Model:            "websearch:" + engine,
			UpstreamModel:    "serpapi:" + serpAPILightEngine,
			RequestID:        laneRequestID(c),
			Account:          account,
			WebSearchCalls:   1,
			Duration:         time.Since(started),
			UpstreamEndpoint: serpAPISearchPath,
		})
	}
}
