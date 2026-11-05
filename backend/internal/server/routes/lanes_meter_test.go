package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 车道计量通道（tasks 2.3）全引擎契约：装配真 Meter（真 BillingCacheService +
// OpenAI 网关 handler）后，车道路由先过计费准入——余额不足 403 拒付（不达
// 适配器占位=不触上游）、余额健康 501（过准入、停在无适配器占位）、无 key 401。

// lanesMeterBillingCache 只实现准入路径用到的余额读；其余嵌 nil 接口
// （本测试不触达）。RPM/配额面在 BillingCacheService 以 nil 依赖装配时按
// 既有测试口径自然放行。
type lanesMeterBillingCache struct {
	service.BillingCache
	balance float64
}

func (c *lanesMeterBillingCache) GetUserBalance(context.Context, int64) (float64, error) {
	return c.balance, nil
}

// lanesMeterAccountRepo 池取数面（第 0 参注入 NewOpenAIGatewayService）：
// seedAccounts 空 → 池空 → 车道 503；有账号 → 过池停 501 适配器占位。
type lanesMeterAccountRepo struct {
	service.AccountRepository
	seedAccounts []service.Account
}

func (r *lanesMeterAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	var out []service.Account
	for _, a := range r.seedAccounts {
		if a.Platform == platform {
			out = append(out, a)
		}
	}
	return out, nil
}

// ListSchedulableByPlatforms: 池接口已改复数批量取数（internal/lanes pool.go），
// 不实现则嵌入的 nil 接口提升出自动生成方法，池一调即 panic。
func (r *lanesMeterAccountRepo) ListSchedulableByPlatforms(_ context.Context, platforms []string) ([]service.Account, error) {
	var out []service.Account
	for _, a := range r.seedAccounts {
		for _, p := range platforms {
			if a.Platform == p {
				out = append(out, a)
				break
			}
		}
	}
	return out, nil
}

func lanePoolSeedAccounts() []service.Account {
	return []service.Account{
		{ID: 11, Platform: "siliconflow", Status: service.StatusActive, Priority: 1},
		{ID: 13, Platform: "openainext", Status: service.StatusActive, Priority: 1},
		{ID: 12, Platform: "serpapi", Status: service.StatusActive, Priority: 1},
	}
}

func newLanesMeterRouteTestRouter(t *testing.T, minBalanceReserve float64, balance float64, seedAccounts []service.Account) (*gin.Engine, string) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{
		ID:               42,
		Status:           service.StatusActive,
		Hydrated:         true,
		Platform:         service.PlatformOpenAI,
		SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier:   1,
	}
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: balance}
	groupID := group.ID
	apiKey := &service.APIKey{
		ID:      100,
		UserID:  user.ID,
		Key:     "lanes-meter-route-test-key",
		Status:  service.StatusActive,
		User:    user,
		GroupID: &groupID,
		Group:   group,
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Billing.MinimumBalanceReserve = minBalanceReserve
	rateRepo := &lanesRouteRateRepo{}
	apiKeyService := service.NewAPIKeyService(
		&lanesRouteAPIKeyRepo{apiKey: apiKey}, nil, nil, nil, rateRepo, nil, cfg,
	)
	gatewayService := service.NewGatewayService(
		nil, nil, nil, nil, nil, nil, rateRepo, nil, cfg, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	openAIGatewayService := service.NewOpenAIGatewayService(
		&lanesMeterAccountRepo{seedAccounts: seedAccounts}, nil, nil, nil, nil, rateRepo, nil, cfg, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(
		&lanesMeterBillingCache{balance: balance}, nil, nil, nil, nil, nil, cfg, nil,
	)
	t.Cleanup(billing.Stop)
	openAIHandler := handler.NewOpenAIGatewayHandler(
		openAIGatewayService, nil, billing, apiKeyService, nil, nil, nil, nil, cfg,
	)
	gatewayHandler := handler.NewGatewayHandler(
		gatewayService, openAIGatewayService, nil, nil, nil, nil, nil, nil,
		apiKeyService, nil, nil, nil, nil, cfg, nil,
	)

	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{Gateway: gatewayHandler, OpenAIGateway: openAIHandler},
		servermiddleware.NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg),
		apiKeyService,
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	return router, apiKey.Key
}

func TestLanesMeterEligibilityGate(t *testing.T) {
	t.Run("insufficient balance rejected before lane adapter slot", func(t *testing.T) {
		router, key := newLanesMeterRouteTestRouter(t, 0.01, 0.005, lanePoolSeedAccounts())
		req := httptest.NewRequest(http.MethodPost, "/v1/search", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		// 403 计费拒付 ≠ 501 占位 ≠ 503 池空：证明请求未越过准入层（未触上游）。
		require.Equal(t, http.StatusForbidden, w.Code)
		var body struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Lane    string `json:"lane"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "billing_error", body.Error.Type)
		require.Equal(t, "websearch", body.Error.Lane)
		require.NotEmpty(t, body.Error.Message)
	})

	t.Run("healthy balance passes gates; all four lane routes reach their adapters", func(t *testing.T) {
		router, key := newLanesMeterRouteTestRouter(t, 0.01, 10, lanePoolSeedAccounts())
		// 三车道 v1 全接（3.1 rerank / 3.2 audio / 3.3 websearch）：501 占位位不再
		// 出现于任一路经——nil body 命中各适配器形状校验 400
		//（契约见 lanes/rerank_test.go、websearch_test.go、audio_test.go）。
		for _, path := range []string{"/v1/rerank", "/v1/search", "/v1/audio/transcriptions", "/v1/audio/speech"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equalf(t, http.StatusBadRequest, w.Code, path)
			require.Containsf(t, w.Body.String(), "invalid_request_error", path)
		}
	})

	t.Run("empty upstream pool yields 503 distinct from adapter slot", func(t *testing.T) {
		router, key := newLanesMeterRouteTestRouter(t, 0.01, 10, nil)
		// 池空先于适配器：四条路由全部 503（不与 400/501 混淆）。
		for _, path := range []string{"/v1/rerank", "/v1/search", "/v1/audio/transcriptions", "/v1/audio/speech"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equalf(t, http.StatusServiceUnavailable, w.Code, path)
			var body struct {
				Error struct {
					Type string `json:"type"`
					Lane string `json:"lane"`
				} `json:"error"`
			}
			require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &body), path)
			require.Equalf(t, "lane_upstream_unavailable", body.Error.Type, path)
		}
	})

	t.Run("missing key still 401 before billing", func(t *testing.T) {
		router, _ := newLanesMeterRouteTestRouter(t, 0.01, 0.005, lanePoolSeedAccounts())
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/search", nil))
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})
}
