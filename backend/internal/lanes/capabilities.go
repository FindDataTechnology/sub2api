package lanes

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 能力快照（ciyuan-capability-endpoint）：把词元在鉴权时已经算出来的能力事实开一个
// key-scoped 只读出口。设计要点（见 openspec/changes/add-ciyuan-capability-endpoint/design.md）：
//
//   - 只读：不计费、不扣窗口、不写用量行、不向上游探测。
//   - 两个谓词分开：「可服务」（serving，来自可调度账号映射）与「只是被允许」
//     （permitted_only，来自分组白名单/平台默认目录）。今天静默失败的正是两者之差。
//   - 未知一律 null：池未装配、视频无配置、窗口用量不可得，都给 null 而不是 false/0/[]。
//   - 不给单价与倍率：价目属管理端；这里只给额度与限额的配置值。
const capabilitiesSchema = "ciyuan.capabilities/v1"

// capabilitiesKeyContext 与模型推导同源，由 handler 只读 getter 返回。
type capabilitiesModelEntry struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Basis string `json:"basis,omitempty"`
}

type capabilitiesKey struct {
	Name             string `json:"name"`
	GroupID          *int64 `json:"group_id"`
	Group            string `json:"group,omitempty"`
	Platform         string `json:"platform,omitempty"`
	Status           string `json:"status,omitempty"`
	AllowlistEnabled bool   `json:"allowlist_enabled"`
}

type capabilitiesModels struct {
	Serving       []capabilitiesModelEntry `json:"serving"`
	PermittedOnly []capabilitiesModelEntry `json:"permitted_only"`
	Counts        capabilitiesCounts       `json:"counts"`
}

type capabilitiesCounts struct {
	Serving       int `json:"serving"`
	PermittedOnly int `json:"permitted_only"`
}

type capabilitiesLane struct {
	Available        *bool `json:"available"`
	UpstreamAccounts *int  `json:"upstream_accounts"`
}

type capabilitiesMediaFlag struct {
	Allowed bool `json:"allowed"`
}

type capabilitiesVideo struct {
	Models []string `json:"models"`
}

type capabilitiesMedia struct {
	Image      capabilitiesMediaFlag `json:"image"`
	BatchImage capabilitiesMediaFlag `json:"batch_image"`
	Video      capabilitiesVideo     `json:"video"`
}

type capabilitiesLimits struct {
	KeyQuotaUSD         float64  `json:"key_quota_usd"`
	QuotaUnlimited      bool     `json:"quota_unlimited"`
	KeyQuotaUsedUSD     float64  `json:"key_quota_used_usd"`
	BalanceUSD          float64  `json:"balance_usd"`
	RateLimit5hUSD      float64  `json:"rate_limit_5h_usd"`
	RateLimit1dUSD      float64  `json:"rate_limit_1d_usd"`
	RateLimit7dUSD      float64  `json:"rate_limit_7d_usd"`
	GroupDailyLimitUSD  *float64 `json:"group_daily_limit_usd"`
	GroupWeeklyLimitUSD *float64 `json:"group_weekly_limit_usd"`
	GroupMonthlyLimit   *float64 `json:"group_monthly_limit_usd"`
	WindowUsage         *float64 `json:"window_usage"`

	// ===== 容量契约（add-key-capacity-contract）=====
	// 有效上限必须如实暴露：key 列为 0（不限）但 user 层限 5 时，报 5 并说明来源。
	// **报"不限"而实际受 user 约束就是欺骗**——调用方会按"不限"规划并发，然后撞 429。
	ConcurrencyRequest int `json:"concurrency_request"` // 保底（额度保留，不保证饱和吞吐）
	// EffectiveConcurrencyLimit 是本 key 实际生效的并发上限；0 表示确实不限。
	EffectiveConcurrencyLimit int    `json:"effective_concurrency_limit"`
	EffectiveRPMLimit         int    `json:"effective_rpm_limit"`
	LimitSource               string `json:"limit_source"` // key | user | unlimited
	// EffectiveAt 未到时刻时请求会被拒（KEY_NOT_YET_EFFECTIVE），与 expires_at 相对。
	EffectiveAt *string `json:"effective_at"`
}

type capabilitiesResponse struct {
	Schema      string                      `json:"schema"`
	GeneratedAt string                      `json:"generated_at"`
	Basis       string                      `json:"basis"`
	Probed      bool                        `json:"probed"`
	Key         capabilitiesKey             `json:"key"`
	Models      capabilitiesModels          `json:"models"`
	Lanes       map[string]capabilitiesLane `json:"lanes"`
	Media       capabilitiesMedia           `json:"media"`
	Limits      capabilitiesLimits          `json:"limits"`
}

// ServeCapabilities 处理 GET /v1/ciyuan/capabilities。
func ServeCapabilities(c *gin.Context, app *Lanes) {
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		laneUnauthorized(c, capabilityLane)
		return
	}
	if app == nil || !app.capabilitiesReady() {
		writeLaneError(c, http.StatusServiceUnavailable, laneError{
			Type:    "lane_upstream_unavailable",
			Message: "capability snapshot is unavailable: gateway services are not wired",
			Lane:    capabilityLane,
		})
		return
	}

	ctx := c.Request.Context()
	available, source := app.capabilityModels(ctx, apiKey)
	resp := capabilitiesResponse{
		Schema:      capabilitiesSchema,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Basis:       "schedulable_account_config",
		Probed:      false,
		Key:         buildCapabilitiesKey(apiKey),
		Models:      buildCapabilitiesModels(apiKey, available, source),
		Lanes:       app.capabilityLanes(ctx),
		Media:       buildCapabilitiesMedia(apiKey.Group),
		Limits:      buildCapabilitiesLimits(apiKey),
	}
	c.JSON(http.StatusOK, resp)
}

// capabilityLane 是本端点在错误体里的 lane 标识（不是三车道之一；用于可归因）。
const capabilityLane = "capabilities"

// capabilitiesReady 报告模型推导所需的网关服务是否装配。
func (l *Lanes) capabilitiesReady() bool {
	return l != nil && l.models != nil
}

// capabilityModels 走 handler 只读 getter；未装配时返回空（由 capabilitiesReady 先拦）。
func (l *Lanes) capabilityModels(ctx context.Context, apiKey *service.APIKey) ([]string, []string) {
	if l == nil || l.models == nil {
		return nil, nil
	}
	return l.models(ctx, apiKey)
}

// capabilityLanes 逐车道取上游候选账号数。池未装配或取数失败 → null（不是 false）。
func (l *Lanes) capabilityLanes(ctx context.Context) map[string]capabilitiesLane {
	out := map[string]capabilitiesLane{}
	for _, lane := range []string{LaneRerank, LaneAudio, LaneWebsearch} {
		out[lane] = capabilitiesLane{}
	}
	if l == nil || l.Pool == nil {
		return out
	}
	for _, lane := range []string{LaneRerank, LaneAudio, LaneWebsearch} {
		platforms := LanePlatforms(lane)
		var (
			accounts []*service.Account
			err      error
		)
		if len(platforms) > 1 {
			accounts, err = l.Pool.CandidatesForPlatforms(ctx, platforms)
		} else {
			accounts, err = l.Pool.Candidates(ctx, LanePlatform(lane))
		}
		if err != nil {
			// 空池（ErrNoLaneAccount）＝「知道，确实没有」→ available=false + 0；
			// 取数失败＝「不知道」→ null。两者混同会让上游故障被读成能力缺失（反之亦然），
			// 这正是本仓反复栽过的「读不到当成不存在」，故在契约层分开。
			if errors.Is(err, ErrNoLaneAccount) {
				empty := 0
				none := false
				out[lane] = capabilitiesLane{Available: &none, UpstreamAccounts: &empty}
				continue
			}
			out[lane] = capabilitiesLane{}
			continue
		}
		n := len(accounts)
		available := n > 0
		out[lane] = capabilitiesLane{Available: &available, UpstreamAccounts: &n}
	}
	return out
}

func buildCapabilitiesKey(apiKey *service.APIKey) capabilitiesKey {
	key := capabilitiesKey{
		Name:   apiKey.Name,
		Status: apiKey.Status,
	}
	if apiKey.Group != nil {
		groupID := apiKey.Group.ID
		key.GroupID = &groupID
		key.Group = apiKey.Group.Name
		key.Platform = apiKey.Group.Platform
		key.AllowlistEnabled = apiKey.Group.ModelAllowlistEnabled()
	}
	return key
}

// buildCapabilitiesModels 生成两个集合：
//
//	serving        = 可调度账号映射集合（available）
//	permitted_only = 可见集合（source，经白名单过滤后）− serving
//
// basis 标明「它为什么可见」：白名单开启 → allowlist；否则 → platform_default。
func buildCapabilitiesModels(apiKey *service.APIKey, available, source []string) capabilitiesModels {
	listed := source
	allowlistEnabled := apiKey != nil && apiKey.Group != nil && apiKey.Group.ModelAllowlistEnabled()
	if allowlistEnabled {
		listed = apiKey.Group.ModelAllowlist.FilterForListing(source)
	}
	basis := "platform_default"
	if allowlistEnabled {
		basis = "allowlist"
	}

	servingSet := make(map[string]struct{}, len(available))
	serving := make([]capabilitiesModelEntry, 0, len(available))
	for _, id := range normalizeModelIDs(available) {
		servingSet[strings.ToLower(id)] = struct{}{}
		serving = append(serving, capabilitiesModelEntry{ID: id, Kind: modelKind(id)})
	}

	permittedOnly := make([]capabilitiesModelEntry, 0, len(listed))
	for _, id := range normalizeModelIDs(listed) {
		if _, ok := servingSet[strings.ToLower(id)]; ok {
			continue
		}
		permittedOnly = append(permittedOnly, capabilitiesModelEntry{ID: id, Kind: modelKind(id), Basis: basis})
	}

	return capabilitiesModels{
		Serving:       serving,
		PermittedOnly: permittedOnly,
		Counts: capabilitiesCounts{
			Serving:       len(serving),
			PermittedOnly: len(permittedOnly),
		},
	}
}

// modelKind 只标能确定的：embedding 用既有判定，其余 unknown——不猜 chat/image/video。
func modelKind(id string) string {
	if service.IsEmbeddingModelID(id) {
		return "embedding"
	}
	return "unknown"
}

// normalizeModelIDs 去空白、按小写去重保序。
func normalizeModelIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	return out
}

// buildCapabilitiesMedia 媒体开关取自认证分组快照。video 无配置 → nil（未知，不是不支持）。
func buildCapabilitiesMedia(group *service.Group) capabilitiesMedia {
	media := capabilitiesMedia{}
	if group == nil {
		return media
	}
	media.Image.Allowed = group.AllowImageGeneration
	media.BatchImage.Allowed = group.AllowBatchImageGeneration
	if len(group.VideoModelPrices) > 0 {
		models := make([]string, 0, len(group.VideoModelPrices))
		for id := range group.VideoModelPrices {
			models = append(models, id)
		}
		sort.Strings(models)
		media.Video.Models = models
	}
	return media
}

// buildCapabilitiesLimits 只给配置值。window_usage 恒为 null（认证快照只带限额，
// 用量在检查时另读）；key_quota_usd=0 用 quota_unlimited 消歧，避免被读成零额度。
//
// 容量契约部分算的是**有效上限**（取 key 与 user 两层中更严的那个，0 视为不限），
// 并给出 limit_source 说明谁在生效——只报 key 列会骗人。
func buildCapabilitiesLimits(apiKey *service.APIKey) capabilitiesLimits {
	limits := capabilitiesLimits{
		KeyQuotaUSD:     apiKey.Quota,
		QuotaUnlimited:  apiKey.Quota == 0,
		KeyQuotaUsedUSD: apiKey.QuotaUsed,
		RateLimit5hUSD:  apiKey.RateLimit5h,
		RateLimit1dUSD:  apiKey.RateLimit1d,
		RateLimit7dUSD:  apiKey.RateLimit7d,
	}
	if apiKey.User != nil {
		limits.BalanceUSD = apiKey.User.Balance
	}
	if group := apiKey.Group; group != nil {
		limits.GroupDailyLimitUSD = group.DailyLimitUSD
		limits.GroupWeeklyLimitUSD = group.WeeklyLimitUSD
		limits.GroupMonthlyLimit = group.MonthlyLimitUSD
	}

	// 容量契约：有效上限 = min(key 值, user 池值)（0 视为不限）
	limits.ConcurrencyRequest = apiKey.ConcurrencyRequest
	poolCap := 0
	if apiKey.User != nil {
		poolCap = apiKey.User.Concurrency
	}
	limits.EffectiveConcurrencyLimit, limits.LimitSource = effectiveLimit(apiKey.ConcurrencyLimit, poolCap)

	// RPM 是速率不是池：只取两层中更严的硬速率（不做 Σ 校验，见 design）。
	userRPM := 0
	if apiKey.User != nil {
		// 用户级 RPM 在快照里是 User.RPMLimit。
		userRPM = apiKey.User.RPMLimit
	}
	limits.EffectiveRPMLimit, _ = effectiveLimit(apiKey.RPMLimit, userRPM)
	if apiKey.EffectiveAt != nil {
		ts := apiKey.EffectiveAt.UTC().Format(time.RFC3339)
		limits.EffectiveAt = &ts
	}
	return limits
}

// effectiveLimit 取两层中更严的上限，并说明生效来源。
// 0 视为不限（沿用本仓 quota=0 语义）。返回 (有效值, 来源)。
func effectiveLimit(keyLimit, poolLimit int) (int, string) {
	switch {
	case keyLimit > 0 && poolLimit > 0:
		if keyLimit <= poolLimit {
			return keyLimit, "key"
		}
		return poolLimit, "user"
	case keyLimit > 0:
		return keyLimit, "key"
	case poolLimit > 0:
		return poolLimit, "user"
	default:
		return 0, "unlimited"
	}
}
