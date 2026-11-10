package lanes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 能力快照（ciyuan-capability-endpoint）单测：两谓词分离、未知一律 null、
// 只读不写、错误面与车道同形。

func newCapabilitiesTestContext(t *testing.T, apiKey *service.APIKey) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/ciyuan/capabilities", nil)
	if apiKey != nil {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	}
	return c, rec
}

func decodeCapabilities(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

// ── 鉴权与装配退化 ─────────────────────────────────────────────

func TestServeCapabilities_RequiresKey(t *testing.T) {
	c, rec := newCapabilitiesTestContext(t, nil)
	ServeCapabilities(c, &Lanes{})

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	body := decodeCapabilities(t, rec)
	errBody, ok := body["error"].(map[string]any)
	require.True(t, ok, "错误体应为 {error:{...}} 形状")
	require.Equal(t, capabilityLane, errBody["lane"], "缺 key 也要可归因")
}

func TestServeCapabilities_NotWiredIs503(t *testing.T) {
	// models 为 nil（网关服务未装配）：503 + 车道同形错误体，且不返回半截结构。
	c, rec := newCapabilitiesTestContext(t, &service.APIKey{Name: "k"})
	ServeCapabilities(c, &Lanes{})

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body := decodeCapabilities(t, rec)
	_, hasModels := body["models"]
	require.False(t, hasModels, "503 不得返回不完整的能力结构")
	errBody := body["error"].(map[string]any)
	require.Equal(t, "lane_upstream_unavailable", errBody["type"])
	require.Equal(t, capabilityLane, errBody["lane"])
}

// ── 两谓词 ────────────────────────────────────────────────────

func TestServeCapabilities_ServingVsPermittedOnly(t *testing.T) {
	apiKey := &service.APIKey{
		Name:   "yizuo-ab12cd34-20261110",
		Status: service.StatusActive,
		Group: &service.Group{
			ID:       3,
			Name:     "core",
			Platform: service.PlatformOpenAI,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"deepseek-v4.1-flash", "glm-5.3-flash"},
			},
		},
	}
	app := &Lanes{models: func(_ context.Context, _ *service.APIKey) ([]string, []string) {
		// 账号只能服务 deepseek；glm 在白名单里但没有可调度账号 → permitted_only。
		return []string{"deepseek-v4.1-flash"},
			[]string{"deepseek-v4.1-flash", "glm-5.3-flash"}
	}}

	c, rec := newCapabilitiesTestContext(t, apiKey)
	ServeCapabilities(c, app)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp capabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, capabilitiesSchema, resp.Schema)
	require.Equal(t, "schedulable_account_config", resp.Basis)
	require.False(t, resp.Probed, "非探测口径必须显式标注")
	require.True(t, resp.Key.AllowlistEnabled)

	require.Len(t, resp.Models.Serving, 1)
	require.Equal(t, "deepseek-v4.1-flash", resp.Models.Serving[0].ID)
	require.Empty(t, resp.Models.Serving[0].Basis, "serving 项不带 basis")

	require.Len(t, resp.Models.PermittedOnly, 1)
	require.Equal(t, "glm-5.3-flash", resp.Models.PermittedOnly[0].ID)
	require.Equal(t, "allowlist", resp.Models.PermittedOnly[0].Basis)
	require.Equal(t, 1, resp.Models.Counts.Serving)
	require.Equal(t, 1, resp.Models.Counts.PermittedOnly)
}

func TestBuildCapabilitiesModels_DefaultBasisWhenAllowlistOff(t *testing.T) {
	apiKey := &service.APIKey{
		Group: &service.Group{ID: 7, Platform: service.PlatformOpenAI},
	}
	// 无映射：available 空，source 由平台默认目录回落。
	models := buildCapabilitiesModels(apiKey, nil, []string{"gpt-5.4-mini"})

	require.Empty(t, models.Serving)
	require.Len(t, models.PermittedOnly, 1)
	require.Equal(t, "platform_default", models.PermittedOnly[0].Basis)
}

func TestBuildCapabilitiesModels_DedupesAndTrims(t *testing.T) {
	apiKey := &service.APIKey{Group: &service.Group{ID: 1}}
	models := buildCapabilitiesModels(apiKey,
		[]string{"m1", " M1 ", ""},
		[]string{"m1", "m1", "m2 "})

	require.Len(t, models.Serving, 1)
	require.Len(t, models.PermittedOnly, 1)
	require.Equal(t, "m2", models.PermittedOnly[0].ID)
}

// ── kind 只标可确定的 ─────────────────────────────────────────

func TestModelKind(t *testing.T) {
	require.Equal(t, "unknown", modelKind("deepseek-v4.1-flash"))
	// embedding 用既有判定，不猜 chat/image/video。
	require.Equal(t, "unknown", modelKind("gpt-5.4-mini"))
}

// ── 车道：未知用 null ─────────────────────────────────────────

func TestCapabilityLanes_PoolNotWiredIsNull(t *testing.T) {
	lanes := capabilityLanesOf(&Lanes{})
	raw, err := json.Marshal(lanes)
	require.NoError(t, err)
	// 断言字面量 null——false 会被序列化成 false，两者必须可分。
	require.Contains(t, string(raw), `"available":null`)
	require.Contains(t, string(raw), `"upstream_accounts":null`)
	require.NotContains(t, string(raw), `"available":false`)

	for _, lane := range []string{LaneRerank, LaneAudio, LaneWebsearch} {
		require.Nil(t, lanes[lane].Available)
		require.Nil(t, lanes[lane].UpstreamAccounts)
	}
}

func TestCapabilityLanes_CountsSchedulableAccounts(t *testing.T) {
	lister := &fakeLaneAccountLister{accounts: []service.Account{
		{ID: 1, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive},
		{ID: 2, Platform: domain.PlatformSiliconFlow, Status: service.StatusActive},
		{ID: 3, Platform: domain.PlatformSiliconFlow, Status: service.StatusDisabled},
	}}
	lanes := capabilityLanesOf(&Lanes{Pool: NewPool(lister, nil)})

	rerank := lanes[LaneRerank]
	require.NotNil(t, rerank.Available)
	require.True(t, *rerank.Available)
	require.NotNil(t, rerank.UpstreamAccounts)
	require.Equal(t, 2, *rerank.UpstreamAccounts, "停用账号不算")

	// 无该平台账号的车道 = 明确 false（知道，且确实没有），与 null 不同。
	audio := lanes[LaneAudio]
	require.NotNil(t, audio.Available)
	require.False(t, *audio.Available)
}

func TestCapabilityLanes_ListErrorIsNullNotEmpty(t *testing.T) {
	lister := &fakeLaneAccountLister{err: context.DeadlineExceeded}
	lanes := capabilityLanesOf(&Lanes{Pool: NewPool(lister, nil)})

	// 取数失败＝不知道：null，不得退化成 false（那是「知道没有」）。
	require.Nil(t, lanes[LaneRerank].Available)
	require.Nil(t, lanes[LaneRerank].UpstreamAccounts)
}

// capabilityLanesOf 便于测试直接读车道段（生产入口是 ServeCapabilities）。
func capabilityLanesOf(app *Lanes) map[string]capabilitiesLane {
	saved := gin.DefaultWriter
	gin.DefaultWriter = httptest.NewRecorder()
	defer func() { gin.DefaultWriter = saved }()
	return app.capabilityLanes(context.Background())
}

// ── 媒体与额度 ───────────────────────────────────────────────

func TestBuildCapabilitiesMedia(t *testing.T) {
	media := buildCapabilitiesMedia(&service.Group{
		AllowImageGeneration:      true,
		AllowBatchImageGeneration: false,
		VideoModelPrices: map[string]map[string]float64{
			"seedance-pro": {"720p": 0.1},
			"seedance-lite": {"480p": 0.05},
		},
	})
	require.True(t, media.Image.Allowed)
	require.False(t, media.BatchImage.Allowed)
	require.Equal(t, []string{"seedance-lite", "seedance-pro"}, media.Video.Models)

	// 无视频配置 → null（未知），不是空数组（空数组会被读成「不支持」）。
	raw, err := json.Marshal(buildCapabilitiesMedia(&service.Group{}))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"models":null`)
}

func TestBuildCapabilitiesLimits_QuotaUnlimitedIsExplicit(t *testing.T) {
	unlimited := buildCapabilitiesLimits(&service.APIKey{Quota: 0, QuotaUsed: 1.5})
	require.True(t, unlimited.QuotaUnlimited, "quota=0 必须显式标注为不限")

	capped := buildCapabilitiesLimits(&service.APIKey{Quota: 10, QuotaUsed: 1.5})
	require.False(t, capped.QuotaUnlimited)

	daily := 5.0
	withGroup := buildCapabilitiesLimits(&service.APIKey{
		User:  &service.User{Balance: 42},
		Group: &service.Group{DailyLimitUSD: &daily},
	})
	require.Equal(t, 42.0, withGroup.BalanceUSD)
	require.NotNil(t, withGroup.GroupDailyLimitUSD)
	require.Equal(t, 5.0, *withGroup.GroupDailyLimitUSD)
	require.Nil(t, withGroup.GroupMonthlyLimit)
	require.Nil(t, withGroup.WindowUsage, "窗口用量恒为 null（快照只带限额）")
}

// ── 只读性与不泄露价目 ───────────────────────────────────────

func TestServeCapabilities_NoPriceOrMultiplierKeys(t *testing.T) {
	apiKey := &service.APIKey{
		Name: "k",
		Group: &service.Group{
			ID:             1,
			Platform:       service.PlatformOpenAI,
			RateMultiplier: 0.8,
			ImagePrice1K:   float64Ptr(0.02),
		},
	}
	app := &Lanes{models: func(_ context.Context, _ *service.APIKey) ([]string, []string) {
		return []string{"m1"}, []string{"m1"}
	}}
	c, rec := newCapabilitiesTestContext(t, apiKey)
	ServeCapabilities(c, app)
	require.Equal(t, http.StatusOK, rec.Code)

	raw := rec.Body.String()
	for _, forbidden := range []string{"rate_multiplier", "price", "image_price", "usd_per"} {
		require.NotContains(t, strings.ToLower(raw), forbidden,
			"能力快照不得含价目/倍率（%s）", forbidden)
	}
}

func float64Ptr(v float64) *float64 { return &v }
