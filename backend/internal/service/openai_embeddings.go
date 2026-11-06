package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func (s *OpenAIGatewayService) ForwardEmbeddings(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	originalModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if originalModel == "" {
		writeOpenAIEmbeddingsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}

	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	SetOpsUpstreamModel(c, upstreamModel)
	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}
	// 账号级 provider 钉扎：聚合渠道多 provider 时保证向量一致性（只增）。
	// 覆盖调用方自带的同名字段；未配置则不改写 body。
	upstreamBody = applyEmbeddingProviderPin(upstreamBody, account)

	logger.L().Debug("openai embeddings: forwarding",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Strings("provider_pin", embeddingProviderOnlyList(account)),
	)

	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	// 协议感知：Anthropic 协议账号的凭证 base_url 指向 /anthropic 端点，
	// embeddings 需使用 OpenAI 格式 base。
	baseURL := account.GetOpenAIFormatBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url: %w", err)
	}
	targetURL := buildOpenAIEmbeddingsURL(validatedURL)

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	upstreamReq.Header.Set("Accept", "application/json")
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if openaiCCRawAllowedHeaders[lowerKey] {
			for _, v := range values {
				upstreamReq.Header.Add(key, v)
			}
		}
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		upstreamReq.Header.Set("user-agent", customUA)
	}

	// 账号级请求头覆写（仅 openai api_key 账号启用时生效）
	account.ApplyHeaderOverrides(upstreamReq.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIUpstream(upstreamReq, proxyURL, account)
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Kind:               "request_error",
			Message:            safeErr,
		})
		writeOpenAIEmbeddingsError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeUpstreamErrorMessage(upstreamMsg)
		if s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
			retryableOnSameAccount := !shouldDisable && account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode)
			if account.IsOpenAIOAuth() && resp.StatusCode == http.StatusTooManyRequests {
				return nil, s.newOpenAIAccountFailoverError(account, resp.StatusCode, resp.Header, respBody, upstreamMsg, shouldDisable, retryableOnSameAccount)
			}
			if isOpenAIHTTPUpstreamAccessStateError(resp.StatusCode, upstreamMsg, respBody) {
				return nil, newOpenAIUpstreamFailoverError(resp.StatusCode, resp.Header, respBody, upstreamMsg, retryableOnSameAccount)
			}
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBody, RetryableOnSameAccount: retryableOnSameAccount}
		}
		writeOpenAIEmbeddingsUpstreamResponse(c, resp, respBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if !errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			writeOpenAIEmbeddingsError(c, http.StatusBadGateway, "api_error", "Failed to read upstream response")
		}
		return nil, fmt.Errorf("read upstream body: %w", err)
	}

	// Dimension drift guard (ciyuan-embedding-lane): a successful response
	// whose vectors disagree with the caller-declared `dimensions`, or whose
	// batch mixes lengths, is a corrupt vector space — reject it as an
	// upstream anomaly instead of writing bad vectors into the consumer's
	// store. Returning a plain error lets the handler's non-failover path
	// answer 502 without billing the caller for the bad response.
	if err := validateOpenAIEmbeddingsResponseDimensions(body, respBody); err != nil {
		return nil, fmt.Errorf("validate embeddings dimensions: %w", err)
	}

	writeOpenAIEmbeddingsUpstreamResponse(c, resp, respBody, s.responseHeaderFilter)

	return &OpenAIForwardResult{
		RequestID:       firstNonEmptyString(resp.Header.Get("x-request-id"), resp.Header.Get("request-id")),
		UpstreamHeaders: resp.Header,
		Usage:           extractOpenAIEmbeddingsUsage(respBody),
		Model:           originalModel,
		BillingModel:    billingModel,
		UpstreamModel:   upstreamModel,
		Stream:          false,
		Duration:        time.Since(startTime),
	}, nil
}

func writeOpenAIEmbeddingsUpstreamResponse(c *gin.Context, resp *http.Response, body []byte, filter *responseheaders.CompiledHeaderFilter) {
	if c == nil || resp == nil {
		return
	}
	if c.Writer.Written() {
		return
	}
	if resp.Header != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, filter)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Writer.Header().Set("Content-Type", ct)
	} else {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(body)
}

func writeOpenAIEmbeddingsError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func extractOpenAIEmbeddingsUsage(body []byte) OpenAIUsage {
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() || !usage.IsObject() {
		return OpenAIUsage{}
	}
	inputTokens := firstPositiveGJSONInt(
		usage.Get("prompt_tokens"),
		usage.Get("input_tokens"),
		usage.Get("total_tokens"),
	)
	outputTokens := firstPositiveGJSONInt(
		usage.Get("completion_tokens"),
		usage.Get("output_tokens"),
	)
	cacheReadTokens := openAICacheReadTokensFromUsage(usage)
	cacheCreationTokens := openAICacheCreationTokensFromUsage(usage)
	// 多模态 embedding（如 doubao-embedding-vision）回传图文 token 拆分，
	// 用于图文不同价计费；纯文本 embedding 该字段为 0，行为不变。
	imageInputTokens := firstPositiveGJSONInt(
		usage.Get("prompt_tokens_details.image_tokens"),
		usage.Get("input_tokens_details.image_tokens"),
	)
	return OpenAIUsage{
		InputTokens:              inputTokens,
		ImageInputTokens:         imageInputTokens,
		OutputTokens:             outputTokens,
		CacheReadInputTokens:     cacheReadTokens,
		CacheCreationInputTokens: cacheCreationTokens,
	}
}

func firstPositiveGJSONInt(values ...gjson.Result) int {
	for _, value := range values {
		if !value.Exists() {
			continue
		}
		n := int(value.Int())
		if n > 0 {
			return n
		}
	}
	return 0
}

func buildOpenAIEmbeddingsURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/embeddings")
}

// validateOpenAIEmbeddingsResponseDimensions rejects the silent dimension
// drift the ciyuan embedding contract forbids. Rules (all deliberately narrow
// so other embedding models keep their passthrough behaviour):
//   - a request carrying `dimensions=N` expects every vector to have N values;
//   - a batch whose vectors have different lengths is malformed regardless;
//   - `encoding_format=base64` (string embeddings) is not decodable here and
//     is left to the caller; a response without a `data` array is not ours to
//     judge (e.g. an upstream error shape).
func validateOpenAIEmbeddingsResponseDimensions(requestBody, responseBody []byte) error {
	data := gjson.GetBytes(responseBody, "data")
	if !data.IsArray() {
		return nil
	}

	want := int64(0)
	if d := gjson.GetBytes(requestBody, "dimensions"); d.Exists() && d.Type == gjson.Number && d.Int() > 0 {
		want = d.Int()
	}

	batchDim := int64(0)
	for _, item := range data.Array() {
		vec := item.Get("embedding")
		if !vec.IsArray() { // base64 / other encoding
			continue
		}
		n := int64(len(vec.Array()))
		if n == 0 {
			continue
		}
		if batchDim == 0 {
			batchDim = n
		}
		if n != batchDim {
			return fmt.Errorf("upstream embeddings mix vector dimensions (%d vs %d)", batchDim, n)
		}
		if want > 0 && n != want {
			return fmt.Errorf("upstream embeddings dimension %d does not match requested dimensions=%d", n, want)
		}
	}
	return nil
}
