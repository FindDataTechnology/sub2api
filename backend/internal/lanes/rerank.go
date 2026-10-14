package lanes

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 硅基流动（SiliconFlow）rerank 常量（tasks 3.1）。
const (
	siliconflowDefaultBaseURL  = "https://api.siliconflow.cn"
	siliconflowRerankPath      = "/v1/rerank"
	rerankDefaultUpstreamModel = "BAAI/bge-reranker-v2-m3"
)

// LaneAdapter 车道适配器：准入与池取号之后的业务位——调上游、回响应、
// 经 Meter 计量。v1.1 起接收全候选列表，适配器内跑轮转循环（failover.go）。
type LaneAdapter interface {
	Serve(c *gin.Context, apiKey *service.APIKey, accounts []*service.Account, pool *Pool, meter *Meter)
}

// RerankAdapter 硅基 /v1/rerank（Cohere/Jina 形状透传：query+documents+top_n，
// 响应 results[{index,relevance_score}]原样回传）。v1 直连（不走账号代理，
// 随 3.x 需要再接 ProxyID）；credentials: api_key 必填、base_url 可覆盖
// （测试/自建网关用）。
type RerankAdapter struct {
	client *http.Client
}

func NewRerankAdapter() *RerankAdapter {
	return &RerankAdapter{client: &http.Client{Timeout: 30 * time.Second}}
}

// laneRequestID 用量行关联：优先客户端 X-Request-Id，缺省生成。
func laneRequestID(c *gin.Context) string {
	if id := strings.TrimSpace(c.GetHeader("X-Request-Id")); id != "" {
		return id
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "lane-" + hex.EncodeToString(buf)
}

func (a *RerankAdapter) Serve(c *gin.Context, apiKey *service.APIKey, accounts []*service.Account, pool *Pool, meter *Meter) {
	started := time.Now()
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		laneBadRequest(c, LaneRerank, "failed to read request body")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		laneBadRequest(c, LaneRerank, "request body must be a JSON object (Cohere/Jina rerank shape)")
		return
	}
	// 形状校验（Cohere/Jina）：query 必填字符串、documents 必填非空字符串数组。
	query, _ := req["query"].(string)
	if strings.TrimSpace(query) == "" {
		laneBadRequest(c, LaneRerank, "field `query` is required and must be a non-empty string")
		return
	}
	docs, ok := req["documents"].([]any)
	if !ok || len(docs) == 0 {
		laneBadRequest(c, LaneRerank, "field `documents` is required and must be a non-empty array of strings")
		return
	}
	for _, d := range docs {
		if _, isStr := d.(string); !isStr {
			laneBadRequest(c, LaneRerank, "field `documents` must be an array of strings (object form is not supported in v1)")
			return
		}
	}
	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = rerankDefaultUpstreamModel
		req["model"] = model
	}

	upstreamBody, err := json.Marshal(req)
	if err != nil {
		laneBadRequest(c, LaneRerank, "failed to serialize request")
		return
	}

	// 轮转循环（failover.go）：单账号失败分级冷却+换下一候选；成功路径与
	// v1 完全一致（原样透传+计量）。
	serveWithFailover(c, LaneRerank, pool, accounts, func(account *service.Account) (bool, *upstreamFailure) {
		apiKeyStr, _ := account.Credentials["api_key"].(string)
		if apiKeyStr == "" {
			// 池里进了无凭据账号：车道侧配置错误（换号+长冷却，全尽才 502）。
			return false, &upstreamFailure{misconfigured: true}
		}
		baseURL, _ := account.Credentials["base_url"].(string)
		if strings.TrimSpace(baseURL) == "" {
			baseURL = siliconflowDefaultBaseURL
		}
		httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, strings.TrimRight(baseURL, "/")+siliconflowRerankPath, bytes.NewReader(upstreamBody))
		if err != nil {
			return false, &upstreamFailure{status: 0}
		}
		httpReq.Header.Set("Authorization", "Bearer "+apiKeyStr)
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := a.client.Do(httpReq)
		if err != nil {
			return false, &upstreamFailure{status: 0}
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// 400/422=调用者内容问题（换号无意义）→ 直接终态；其余进轮转。
			if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
				laneUpstreamHTTPError(c, LaneRerank, resp.StatusCode)
				return true, nil
			}
			failure := &upstreamFailure{status: resp.StatusCode}
			if readErr == nil {
				failure.bodyExcerpt = string(respBody)
			}
			return false, failure
		}
		if readErr != nil {
			return false, &upstreamFailure{status: resp.StatusCode}
		}

		c.Data(http.StatusOK, "application/json", respBody)

		if meter != nil {
			meter.WriteUsage(c, apiKey, UsageInput{
				Lane:             LaneRerank,
				Model:            model,
				UpstreamModel:    model,
				RequestID:        laneRequestID(c),
				Account:          account,
				RerankRequests:   1,
				Duration:         time.Since(started),
				UpstreamEndpoint: siliconflowRerankPath,
			})
		}
		return true, nil
	})
}
