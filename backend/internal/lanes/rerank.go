package lanes

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// 经 Meter 计量。3.x 每车道一个实现；未实现的车道保持 501 占位。
type LaneAdapter interface {
	Serve(c *gin.Context, apiKey *service.APIKey, account *service.Account, meter *Meter)
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

// laneUpstreamError 上游失败统一 5xx（3.4 全家桶的雏形）：lane 标识、不泄露
// 上游响应体、不计费（WriteUsage 只在成功路径调用）。
func laneUpstreamError(c *gin.Context, lane string, upstreamStatus int) {
	c.JSON(http.StatusBadGateway, gin.H{
		"error": gin.H{
			"type":    "lane_upstream_error",
			"message": fmt.Sprintf("%s lane upstream request failed (status %d)", lane, upstreamStatus),
			"lane":    lane,
		},
	})
}

func laneBadRequest(c *gin.Context, lane, message string) {
	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{
			"type":    "invalid_request_error",
			"message": message,
			"lane":    lane,
		},
	})
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

func (a *RerankAdapter) Serve(c *gin.Context, apiKey *service.APIKey, account *service.Account, meter *Meter) {
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

	apiKeyStr, _ := account.Credentials["api_key"].(string)
	if apiKeyStr == "" {
		// 池里进了无凭据账号：视为车道侧配置错误（5xx 家族、不计费）。
		laneUpstreamError(c, LaneRerank, 0)
		return
	}
	baseURL, _ := account.Credentials["base_url"].(string)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = siliconflowDefaultBaseURL
	}

	upstreamBody, err := json.Marshal(req)
	if err != nil {
		laneBadRequest(c, LaneRerank, "failed to serialize request")
		return
	}
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, strings.TrimRight(baseURL, "/")+siliconflowRerankPath, bytes.NewReader(upstreamBody))
	if err != nil {
		laneUpstreamError(c, LaneRerank, 0)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKeyStr)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		laneUpstreamError(c, LaneRerank, 0)
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		laneUpstreamError(c, LaneRerank, resp.StatusCode)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		laneUpstreamError(c, LaneRerank, resp.StatusCode)
		return
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
}
