package lanes

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	pkgerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RecordUsageFunc 生产实现 = OpenAIGatewayService.RecordUsage（原生账本写入）。
// 以函数注入：车道测试可捕获入参形状而不必装配整套网关服务——账本写入本体
// （成本计算/扣费/用量行）是上游自身已测机器，车道只负责正确接线。
type RecordUsageFunc func(ctx context.Context, input *service.OpenAIRecordUsageInput) error

// Meter 车道计量通道（tasks 2.3）：计费准入（余额/订阅/窗口/配额/RPM）+ 一次
// 成功车道请求的用量写回原生账本。分组倍率/消费窗口/面板由上游机器自然生效。
type Meter struct {
	billing     *service.BillingCacheService
	apiKeyQuota service.APIKeyQuotaUpdater
	recordUsage RecordUsageFunc
	pool        *service.UsageRecordWorkerPool
}

// NewMeter 装配计量通道。billing 为 nil 时 Meter 退化为纯骨架（不准入、不计量）。
func NewMeter(billing *service.BillingCacheService, apiKeyQuota service.APIKeyQuotaUpdater, recordUsage RecordUsageFunc, cfg *config.Config) *Meter {
	return &Meter{
		billing:     billing,
		apiKeyQuota: apiKeyQuota,
		recordUsage: recordUsage,
		pool:        service.NewUsageRecordWorkerPool(cfg),
	}
}

// Stop 停用量记录池（进程退出时调用；未停则随进程回收）。
func (m *Meter) Stop() {
	if m == nil || m.pool == nil {
		return
	}
	m.pool.Stop()
}

// CheckEligibility 与 chat/grok 音频网关同口径的计费准入：订阅/余额、
// user×平台配额、API key 货币窗口、RPM。窗口耗尽在此拒绝，不触上游。
func (m *Meter) CheckEligibility(c *gin.Context, apiKey *service.APIKey) error {
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	return m.billing.CheckBillingEligibility(
		c.Request.Context(), apiKey.User, apiKey, apiKey.Group,
		subscription, service.QuotaPlatform(c.Request.Context(), apiKey),
	)
}

// UsageInput 一次车道用量的计量入参。v1 单位口径（D4）：websearch=次数
// （WebSearchCalls，复用分组 WebSearchPricePerCall 按次价）；audio=AudioUsage
// {Mode, DurationOrUnits}（复用分组音频三价）；rerank=按请求（独立价目字段
// 3.1 落地，落地前该车道不产生真实计费——无适配器即不达 WriteUsage）。
type UsageInput struct {
	Lane             string
	Model            string // 面板显示的模型名（车道侧命名，如 websearch:serpapi）
	UpstreamModel    string
	RequestID        string
	Account          *service.Account // 车道上游池账号（2.4 落地；测试可注入桩）
	WebSearchCalls   int
	RerankRequests   int // rerank 车道成功请求数（按请求计费，单价=分组 rerank_price_per_request）
	Audio            *service.AudioUsage
	Duration         time.Duration
	UpstreamEndpoint string
}

// WriteUsage 把一次成功车道请求的用量写回原生账本。与 AlphaSearch 参考实现
// 同构：mandatory 池提交，池满/停池时同步兜底执行，保证扣费不丢；池内使用
// 与请求取消脱钩的上下文（值保留，取消不传导）。
func (m *Meter) WriteUsage(c *gin.Context, apiKey *service.APIKey, in UsageInput) {
	if m == nil || m.recordUsage == nil || apiKey == nil {
		return
	}
	detached := context.WithoutCancel(c.Request.Context())
	input := &service.OpenAIRecordUsageInput{
		Result: &service.OpenAIForwardResult{
			RequestID:      in.RequestID,
			Model:          in.Model,
			UpstreamModel:  in.UpstreamModel,
			WebSearchCalls: in.WebSearchCalls,
			RerankRequests: in.RerankRequests,
			AudioUsage:     in.Audio,
			Duration:       in.Duration,
		},
		APIKey:           apiKey,
		User:             apiKey.User,
		Account:          in.Account,
		InboundEndpoint:  c.FullPath(),
		UpstreamEndpoint: in.UpstreamEndpoint,
		UserAgent:        c.GetHeader("User-Agent"),
		IPAddress:        ip.GetClientIP(c),
		SessionID:        service.ExtractClientSessionID(c),
		APIKeyService:    m.apiKeyQuota,
		QuotaPlatform:    service.QuotaPlatform(detached, apiKey),
	}
	log := logger.L().With(
		zap.String("component", "lanes."+in.Lane),
		zap.String("lane", in.Lane),
		zap.Int64("api_key_id", apiKey.ID),
		zap.String("model", in.Model),
	)
	task := func(ctx context.Context) {
		if err := m.recordUsage(ctx, input); err != nil {
			log.Error("lanes.record_usage_failed", zap.Error(err))
		}
	}
	if m.pool != nil {
		if mode := m.pool.Submit(task); !mode.Dropped() {
			return
		}
		log.Warn("lanes.usage_record_task_mandatory_sync_fallback")
	}
	runLaneUsageTask(detached, task)
}

// runLaneUsageTask 同步兜底（同 handler.submitMandatoryUsageRecordTask 语义）：
// 限时执行 + panic 兜底，计费任务不因单笔异常丢失进程。
func runLaneUsageTask(parent context.Context, task service.UsageRecordTask) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "lanes.usage"),
				zap.Any("panic", recovered),
			).Error("lanes.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

// laneBillingError 把计费准入失败映射为车道错误体（镜像 handler.billingErrorDetails
// 的映射口径——该函数未导出；此处外加 lane 标识，与 3.4 统一错误体同构）。
func laneBillingError(c *gin.Context, lane string, err error) {
	status, code, message, retryAfter := laneBillingErrorDetails(err)
	if retryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    code,
			"message": message,
			"lane":    lane,
		},
	})
}

func laneBillingErrorDetails(err error) (status int, code, message string, retryAfter int) {
	if errors.Is(err, service.ErrBillingServiceUnavailable) {
		msg := pkgerrors.Message(err)
		if msg == "" {
			msg = "Billing service temporarily unavailable. Please retry later."
		}
		return http.StatusServiceUnavailable, "billing_service_error", msg, 0
	}
	if errors.Is(err, service.ErrAPIKeyRateLimit5hExceeded) ||
		errors.Is(err, service.ErrAPIKeyRateLimit1dExceeded) ||
		errors.Is(err, service.ErrAPIKeyRateLimit7dExceeded) {
		return http.StatusTooManyRequests, "rate_limit_exceeded", pkgerrors.Message(err), 0
	}
	if errors.Is(err, service.ErrGroupRPMExceeded) || errors.Is(err, service.ErrUserRPMExceeded) {
		retrySeconds := 60 - int(time.Now().Unix()%60)
		return http.StatusTooManyRequests, "rate_limit_exceeded", pkgerrors.Message(err), retrySeconds
	}
	if errors.Is(err, service.ErrUserPlatformDailyQuotaExhausted) ||
		errors.Is(err, service.ErrUserPlatformWeeklyQuotaExhausted) ||
		errors.Is(err, service.ErrUserPlatformMonthlyQuotaExhausted) {
		return http.StatusTooManyRequests, "rate_limit_exceeded", pkgerrors.Message(err), laneQuotaResetSeconds(err)
	}
	msg := pkgerrors.Message(err)
	if msg == "" {
		logger.L().With(
			zap.String("component", "lanes.billing"),
			zap.Error(err),
		).Warn("lanes.billing_error_missing_message")
		msg = "Billing error"
	}
	return http.StatusForbidden, "billing_error", msg, 0
}

// laneQuotaResetSeconds 从平台配额错误的 window_resets_at 元数据取退避秒数
// （镜像 handler.extractQuotaResetSeconds；解析失败按 60s 常规退避）。
func laneQuotaResetSeconds(err error) int {
	const fallback = 60
	appErr := pkgerrors.FromError(err)
	if appErr == nil {
		return fallback
	}
	raw, ok := appErr.Metadata["window_resets_at"]
	if !ok || raw == "" {
		return fallback
	}
	resetAt, parseErr := time.Parse(time.RFC3339, raw)
	if parseErr != nil {
		return fallback
	}
	secs := time.Until(resetAt).Seconds()
	if secs <= 0 {
		return fallback
	}
	return int(math.Ceil(secs))
}
