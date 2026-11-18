package lanes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// ── 请求级 BYOK 覆盖（ciyuan-lane-pool-failover 4.1）─────────────────────────
// 调用者可在请求头带自己的 SerpAPI key（X-Serpapi-Key）：优先直连上游（不经
// 池），失败按 hash8 进程内冷却并回退池；来源三态经 x-search-source 如实标注
// （语义对齐 search-relay add-search-byok）。key 值绝不入日志/响应/错误文本。
const (
	websearchBYOKHeader  = "X-Serpapi-Key"
	websearchBYOKMarker  = "byok:" // 用量行归因前缀（UpstreamModel=byok:<hash8>）
	websearchSourceUser  = "user"
	websearchSourcePool  = "pool"
	websearchSourcePoolAfterUserFailure = "pool-after-user-failure"
)

var (
	byokMu       sync.Mutex
	byokCooldown = map[string]time.Time{} // hash8 -> 冷却截止
)

func byokTagOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4]) // 前 8 个 hex 字符
}

func byokCooling(tag string) bool {
	byokMu.Lock()
	defer byokMu.Unlock()
	until, ok := byokCooldown[tag]
	return ok && time.Now().Before(until)
}

func byokSetCooldown(tag string, d time.Duration) {
	byokMu.Lock()
	defer byokMu.Unlock()
	byokCooldown[tag] = time.Now().Add(d)
}

func (a *WebsearchAdapter) Serve(c *gin.Context, apiKey *service.APIKey, accounts []*service.Account, pool *Pool, meter *Meter) {
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
	if len(accounts) == 0 {
		// 防御：serveLane 已拦空池；BYOK 路径要取 accounts[0] 端点，先挡。
		laneUpstreamUnavailable(c, LaneWebsearch)
		return
	}

	// BYOK 优先（4.1）：用户 key 直连成功即应答（0 费计量）；失败按分级冷却
	// 该 hash8 并回退池（来源如实标 pool-after-user-failure，不静默）。
	// BYOK 端点=平台所配 SerpAPI 端点（首候选 base_url，缺省 serpapi.com）——
	// 平台自定义端点/代理对用户 key 同样生效，测试也因此可注入。
	source := websearchSourcePool
	if userKey := strings.TrimSpace(c.GetHeader(websearchBYOKHeader)); userKey != "" {
		byokBaseURL := serpAPIDefaultBaseURL
		if b, _ := accounts[0].Credentials["base_url"].(string); strings.TrimSpace(b) != "" {
			byokBaseURL = b
		}
		tag := byokTagOf(userKey)
		if !byokCooling(tag) {
			results, failure := a.serpAPISearch(c, byokBaseURL, userKey, q, num, req)
			if failure == nil {
				a.respond(c, engine, results, websearchSourceUser, started)
				if meter != nil {
					// BYOK 笔计量 0 费（4.2，成本分支=台账缝点「分组价目」）：
					// 用量行保留归因（UpstreamModel=byok:<hash8>）。账号位锚定池
					// 首号——usage_logs.account_id 是 NOT NULL+外键（v0.2.16-5 线上
					// 实证：合成 ID=0 账号被 fkey 拒、行丢失）；归因真相在前缀字段，
					// 费率为 0 故账号侧统计零影响。
					meter.WriteUsage(c, apiKey, UsageInput{
						Lane:             LaneWebsearch,
						Model:            "websearch:" + engine,
						UpstreamModel:    websearchBYOKMarker + tag,
						RequestID:        laneRequestID(c),
						Account:          accounts[0],
						WebSearchCalls:   1,
						Duration:         time.Since(started),
						UpstreamEndpoint: serpAPISearchPath,
					})
				}
				return
			}
			d := laneCooldownTransient
			if failure.misconfigured || classifyLaneUpstreamFailure(failure.status, failure.bodyExcerpt) {
				d = laneCooldownExhausting
			}
			byokSetCooldown(tag, d)
		}
		source = websearchSourcePoolAfterUserFailure
	}

	// 池轮转（failover.go）：单账号失败分级冷却+换下一候选；成功照 v1 计费。
	serveWithFailover(c, LaneWebsearch, pool, accounts, func(account *service.Account) (bool, *upstreamFailure) {
		apiKeyStr, _ := account.Credentials["api_key"].(string)
		if apiKeyStr == "" {
			return false, &upstreamFailure{misconfigured: true}
		}
		baseURL, _ := account.Credentials["base_url"].(string)
		if strings.TrimSpace(baseURL) == "" {
			baseURL = serpAPIDefaultBaseURL
		}
		results, failure := a.serpAPISearch(c, baseURL, apiKeyStr, q, num, req)
		if failure != nil {
			return false, failure
		}
		a.respond(c, engine, results, source, started)
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
		return true, nil
	})
}

// serpAPISearch 执行一次上游搜索（池账号与 BYOK key 共用）：归一化结果或
// 失败事实。零结果=合法空集（对齐 v1/search-relay 口径，照常计费）。
func (a *WebsearchAdapter) serpAPISearch(c *gin.Context, baseURL, apiKeyStr, q string, num int, req map[string]any) ([]websearchResult, *upstreamFailure) {
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
		return nil, &upstreamFailure{status: 0}
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, &upstreamFailure{status: 0}
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &upstreamFailure{status: resp.StatusCode}
		if readErr == nil {
			failure.bodyExcerpt = string(respBody)
		}
		return nil, failure
	}
	if readErr != nil {
		return nil, &upstreamFailure{status: resp.StatusCode}
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
		return nil, &upstreamFailure{status: resp.StatusCode}
	}
	if parsed.Error != "" && !serpAPIZeroResultRe.MatchString(parsed.Error) {
		// 上游业务错误（无效 key/额度耗尽等）：错误文本只进分级判据，不外泄。
		return nil, &upstreamFailure{status: resp.StatusCode, bodyExcerpt: parsed.Error}
	}
	results := make([]websearchResult, 0, num)
	for i, r := range parsed.OrganicResults {
		if i >= num {
			break
		}
		results = append(results, websearchResult{
			Title:    r.Title,
			URL:      r.Link,
			Snippet:  r.Snippet,
			Position: i + 1,
		})
	}
	return results, nil
}

// respond 统一信封应答（含来源标注头；BYOK 与池路径共用）。
func (a *WebsearchAdapter) respond(c *gin.Context, engine string, results []websearchResult, source string, started time.Time) {
	var env websearchEnvelope
	env.Meta.Engine = engine
	env.Meta.LatencyMs = time.Since(started).Milliseconds()
	env.Results = results
	env.Usage.Requests = 1
	c.Header("x-search-source", source)
	c.JSON(http.StatusOK, env)
}
