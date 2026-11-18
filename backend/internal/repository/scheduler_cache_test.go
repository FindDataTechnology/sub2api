package repository

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestFilterSchedulerCredentialsKeepsSubscriptionPlanType(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"plan_type":     "plus",
		"access_token":  "secret-access-token",
		"refresh_token": "secret-refresh-token",
	})

	require.Equal(t, "plus", filtered["plan_type"])
	require.NotContains(t, filtered, "access_token")
	require.NotContains(t, filtered, "refresh_token")
}

func TestFilterSchedulerCredentialsKeepsGeminiExplicitCacheKeys(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"api_key":              "k",
		"base_url":             "https://gw.example.com",
		"auth_mode":            "apikey",
		"service_account_json": "secret",
		service.GeminiExplicitCacheUpstreamCredentialKey:       true,
		service.GeminiExplicitCacheUpstreamMaxTTLCredentialKey: 3600,
	})

	require.Equal(t, "https://gw.example.com", filtered["base_url"])
	require.Equal(t, "apikey", filtered["auth_mode"])
	require.Equal(t, true, filtered[service.GeminiExplicitCacheUpstreamCredentialKey])
	require.Equal(t, 3600, filtered[service.GeminiExplicitCacheUpstreamMaxTTLCredentialKey])
	require.NotContains(t, filtered, "service_account_json")
}

func TestSchedulerMetadataAccountKeepsOpenAISubscriptionIdentity(t *testing.T) {
	account := service.Account{
		ID:       24,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Credentials: map[string]any{
			"plan_type":    "plus",
			"access_token": "secret-access-token",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)

	require.True(t, metadata.IsOpenAIChatGPTSubscription())
	require.Empty(t, metadata.GetCredential("access_token"))
}

func TestSchedulerMetadataAccountKeepsProArgonQuotaIsolation(t *testing.T) {
	account := service.Account{
		ID: 25, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"plan_type": "Pro", "access_token": "secret-access-token", "refresh_token": "secret-refresh-token",
		},
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"antigravity:gemini:argon": map[string]any{
					"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	_, payload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)
	var metadata service.Account
	require.NoError(t, json.Unmarshal([]byte(payload), &metadata))
	require.Equal(t, "Pro", metadata.GetCredential("plan_type"))
	require.Empty(t, metadata.GetCredential("access_token"))
	require.Empty(t, metadata.GetCredential("refresh_token"))
	require.False(t, metadata.IsSchedulableForModel("gemini-4-argon-low"))
	require.Positive(t, metadata.GetRateLimitRemainingTime("gemini-4-argon-low"))
	require.True(t, metadata.IsSchedulableForModel("gemini-3.8-flash"))
	require.Zero(t, metadata.GetRateLimitRemainingTime("gemini-3.8-flash"))
}

func TestSchedulerMetadataAccountProjectsUpstreamBillingProbe(t *testing.T) {
	lastError := strings.Repeat("upstream diagnostic ", 512)
	probe := map[string]any{
		"status": "ok",
		"data": map[string]any{
			"billing_scope":             "token",
			"resolved_rate_multiplier":  0.03,
			"peak_rate_enabled":         true,
			"peak_start":                "09:00",
			"peak_end":                  "18:00",
			"peak_rate_multiplier":      2.0,
			"timezone":                  "Asia/Shanghai",
			"effective_rate_multiplier": 0.03,
			"remote_diagnostic":         lastError,
		},
		"received_at":   "2026-07-13T10:00:00Z",
		"fresh_until":   "2026-07-13T11:00:00Z",
		"next_probe_at": "2026-07-13T10:30:00Z",
		"http_status":   502,
		"last_error":    lastError,
	}
	account := service.Account{
		ID: 42,
		Extra: map[string]any{
			"upstream_billing_probe": probe,
			"unused_large_field":     "drop-me",
		},
	}

	metadata := buildSchedulerMetadataAccount(account)
	fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	filtered, ok := metadata.Extra["upstream_billing_probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok", filtered["status"])
	require.Equal(t, "2026-07-13T10:00:00Z", filtered["received_at"])
	require.Equal(t, "2026-07-13T11:00:00Z", filtered["fresh_until"])
	require.Equal(t, "2026-07-13T10:30:00Z", filtered["next_probe_at"])
	require.NotContains(t, filtered, "http_status")
	require.NotContains(t, filtered, "last_error")
	filteredData, ok := filtered["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "token", filteredData["billing_scope"])
	require.Equal(t, 0.03, filteredData["resolved_rate_multiplier"])
	require.Equal(t, true, filteredData["peak_rate_enabled"])
	require.Equal(t, "09:00", filteredData["peak_start"])
	require.Equal(t, "18:00", filteredData["peak_end"])
	require.Equal(t, 2.0, filteredData["peak_rate_multiplier"])
	require.Equal(t, "Asia/Shanghai", filteredData["timezone"])
	require.NotContains(t, filteredData, "effective_rate_multiplier")
	require.NotContains(t, filteredData, "remote_diagnostic")
	require.NotContains(t, metadata.Extra, "unused_large_field")
	require.Contains(t, string(fullPayload), lastError)
	require.NotContains(t, string(metaPayload), "last_error")
	require.Less(t, len(metaPayload)*4, len(fullPayload))
}

func TestSchedulerMetadataAccountDropsInvalidUpstreamBillingProbe(t *testing.T) {
	for _, probe := range []any{
		"invalid",
		map[string]any{},
		map[string]any{"status": ""},
	} {
		metadata := buildSchedulerMetadataAccount(service.Account{
			Extra: map[string]any{service.UpstreamBillingProbeExtraKey: probe},
		})

		require.NotContains(t, metadata.Extra, service.UpstreamBillingProbeExtraKey)
	}
}

// 调度投影必须保留 OpenAI 端点能力集（openai_capabilities）。
//
// 候选过滤 isAccountRequestCompatibleReason 用 SupportsOpenAIEndpointCapability
// 判定能力（capability_mismatch），该判定读 credentials["openai_capabilities"]，
// 且缺键时对「显式配置过能力集的账号」返回 false。投影裁掉该键会让 Seedance
// 视频账号在候选阶段被恒久排除（生产实证 2026-10-15：sched:acc 全量快照带
// seedance、sched:meta 投影为空 → 每次请求 503 seedance_no_eligible_account）。
func TestFilterSchedulerCredentialsKeepsOpenAIEndpointCapabilities(t *testing.T) {
	filtered := filterSchedulerCredentials(map[string]any{
		"api_key":  "k",
		"base_url": "https://api.openai-next.com/seedance",
		service.OpenAIEndpointCapabilitiesCredentialKey: []any{"seedance"},
		// 投影仍不得携带任何未登记键。
		"service_account_json": "secret",
	})

	require.Equal(t, []any{"seedance"}, filtered[service.OpenAIEndpointCapabilitiesCredentialKey],
		"投影裁掉能力集会让显式配置能力的账号被误判 capability_mismatch")
	require.NotContains(t, filtered, "service_account_json")
}

// 走完整序列化路径复证：投影往返后账号仍应被判定支持 seedance。
func TestBuildSchedulerMetadataAccount_KeepsSeedanceCapability(t *testing.T) {
	account := service.Account{
		ID:       4401,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "k",
			"base_url": "https://api.openai-next.com/seedance",
			service.OpenAIEndpointCapabilitiesCredentialKey: []any{"seedance"},
		},
	}
	require.True(t, account.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilitySeedance),
		"前置条件：源账号声明了 seedance 能力")

	// 走一遍真实的序列化/反序列化路径（写入 sched:meta 再由 decodeCachedAccount 读回）。
	payload, err := json.Marshal(buildSchedulerMetadataAccount(account))
	require.NoError(t, err)
	var restored service.Account
	require.NoError(t, json.Unmarshal(payload, &restored))

	require.True(t, restored.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilitySeedance),
		"投影裁掉能力集会让 Seedance 账号在候选过滤阶段被恒久排除")
	// 能力集是白名单语义：未声明的能力仍应被拒（防止把视频账号调度去接 chat）。
	require.False(t, restored.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilityChatCompletions))
}
