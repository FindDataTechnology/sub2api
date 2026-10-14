package lanes

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// 车道统一错误面（tasks 3.4 / 规格「上游故障不外泄且如实标注」）：三车道共用
// 同一组错误构造函数，形状一致（error.type + message + lane + engine），且
// 一律满足：
//   - 不外泄：上游响应体、上游错误文本（可能含账号信息）、上游 URL/凭据一律不入响应
//   - 不计费：错误路径不调 Meter.WriteUsage（计量只在成功路径）
//   - 可归因：lane 恒在；engine（上游供应商标识）随车道带上
//
// 错误码约定（调用者可按 type 分类）：
//
//	invalid_request_error          400 调用者请求形状/内容问题
//	lane_payload_too_large         413 上传超限（audio）
//	lane_upstream_unavailable      503 车道暂无可用上游账号（池空/全停用）
//	lane_upstream_error            502 上游侧故障（含 401/403/429/5xx、断连、超时）
//	lane_not_implemented           501 车道/能力未落地（v1 后仅剩未知路径兜底）
type laneError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Lane    string `json:"lane"`
	Engine  string `json:"engine,omitempty"`
}

func writeLaneError(c *gin.Context, status int, err laneError) {
	c.JSON(status, gin.H{"error": err})
}

// laneUnauthorized 缺 key（正常由 apiKeyAuth 中间件先拦，这里是裸挂载兜底）。
func laneUnauthorized(c *gin.Context, lane string) {
	writeLaneError(c, http.StatusUnauthorized, laneError{
		Type:    "unauthorized",
		Message: "missing api key",
		Lane:    lane,
		Engine:  LaneEngine(lane),
	})
}

// laneBadRequest 调用者侧问题（形状/内容/参数）。
func laneBadRequest(c *gin.Context, lane, message string) {
	writeLaneError(c, http.StatusBadRequest, laneError{
		Type:    "invalid_request_error",
		Message: message,
		Lane:    lane,
		Engine:  LaneEngine(lane),
	})
}

// laneRequestTooLarge 上传超车道上限（audio 专属上限，见 audioMaxUploadBytes）。
func laneRequestTooLarge(c *gin.Context, lane string, limit int64) {
	writeLaneError(c, http.StatusRequestEntityTooLarge, laneError{
		Type:    "lane_payload_too_large",
		Message: fmt.Sprintf("%s lane upload exceeds %d bytes", lane, limit),
		Lane:    lane,
		Engine:  LaneEngine(lane),
	})
}

// laneUpstreamUnavailable 池空/全停用：车道暂无可用上游（与 502「上游故障」
// 可区分——前者是配置问题，后者是运行问题）。
func laneUpstreamUnavailable(c *gin.Context, lane string) {
	writeLaneError(c, http.StatusServiceUnavailable, laneError{
		Type:    "lane_upstream_unavailable",
		Message: lane + " lane has no available upstream account (configure one in admin accounts)",
		Lane:    lane,
		Engine:  LaneEngine(lane),
	})
}

// laneUpstreamError 上游侧故障：断连/超时/上游非 2xx（status=0 表示未能取得
// 上游状态码，如连接失败或超时）。统一 502，且不外泄上游任何细节。
func laneUpstreamError(c *gin.Context, lane string, upstreamStatus int) {
	writeLaneError(c, http.StatusBadGateway, laneError{
		Type:    "lane_upstream_error",
		Message: fmt.Sprintf("%s lane upstream request failed (status %d)", lane, upstreamStatus),
		Lane:    lane,
		Engine:  LaneEngine(lane),
	})
}

// laneUpstreamHTTPError 上游非 2xx 的统一分级（三车道同一判据）：
// 400/422=上游认为请求内容/参数有问题（多半是调用者上传内容，如不支持的音频
// 编码、超长文本）→ 400 给调用者；其余（401/403/429/5xx=上游侧故障或额度）
// → 502。两者都不外泄上游错误文本、都不计费。
func laneUpstreamHTTPError(c *gin.Context, lane string, upstreamStatus int) {
	if upstreamStatus == http.StatusBadRequest || upstreamStatus == http.StatusUnprocessableEntity {
		laneBadRequest(c, lane, fmt.Sprintf("%s lane upstream rejected the request content (status %d)", lane, upstreamStatus))
		return
	}
	laneUpstreamError(c, lane, upstreamStatus)
}

// laneUpstreamMisconfigured 车道侧配置错误：池里取到无凭据账号（管理员配了
// 账号却没填 api_key）。对调用者是运行问题→502；不暴露账号细节。
func laneUpstreamMisconfigured(c *gin.Context, lane string) {
	writeLaneError(c, http.StatusBadGateway, laneError{
		Type:    "lane_upstream_error",
		Message: lane + " lane upstream account is missing credentials (check admin account settings)",
		Lane:    lane,
		Engine:  LaneEngine(lane),
	})
}

// notImplemented 车道/能力未落地（v1 后仅剩未知路径兜底）。
func notImplemented(c *gin.Context, lane string) {
	writeLaneError(c, http.StatusNotImplemented, laneError{
		Type:    "lane_not_implemented",
		Message: lane + " lane is not implemented yet (ciyuan lanes skeleton)",
		Lane:    lane,
	})
}

// LaneEngine 车道 v1 的上游供应商标识（错误体如实标注，与用量行同源）。
// 后续一车道多上游时按实际选中的适配器/账号覆盖。
func LaneEngine(lane string) string {
	switch lane {
	case LaneRerank, LaneAudio:
		return laneEngineSiliconFlow
	case LaneWebsearch:
		return websearchEngineSerpAPI
	default:
		return ""
	}
}

// laneEngineSiliconFlow 与 LanePlatform 同义登记（避免 pool 包常量反向依赖）。
const laneEngineSiliconFlow = "siliconflow"
