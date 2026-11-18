package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 车道骨架契约（tasks 2.2）：四条车道路由挂同一 apiKeyAuth 链——
// 无 key 401 / 有 key 501 占位（带 lane 标识）/ 未注册方法 404（与 /v1 面一致）。

var lanesSkeletonRoutes = []struct {
	method string
	path   string
	lane   string
}{
	{http.MethodPost, "/v1/rerank", "rerank"},
	{http.MethodPost, "/v1/search", "websearch"},
	{http.MethodPost, "/v1/audio/transcriptions", "audio"},
	{http.MethodPost, "/v1/audio/speech", "audio"},
}

// lanesRouteAPIKeyRepo 在 keyBilling 测试 fake 之上补 UpdateLastUsed：车道路径
// 非 billingInfoRequest，apiKeyAuth 成功后会 TouchLastUsed（防抖写 last_used_at）。
type lanesRouteAPIKeyRepo struct {
	service.APIKeyRepository
	apiKey *service.APIKey
}

func (r *lanesRouteAPIKeyRepo) GetByKeyForAuth(_ context.Context, key string) (*service.APIKey, error) {
	if r.apiKey == nil || key != r.apiKey.Key {
		return nil, service.ErrAPIKeyNotFound
	}
	clone := *r.apiKey
	return &clone, nil
}

func (r *lanesRouteAPIKeyRepo) UpdateLastUsed(context.Context, int64, time.Time) error { return nil }

type lanesRouteRateRepo struct {
	service.UserGroupRateRepository
}

func (r *lanesRouteRateRepo) GetByUserAndGroup(context.Context, int64, int64) (*float64, error) {
	return nil, nil
}

func (r *lanesRouteRateRepo) GetRPMOverrideByUserAndGroup(context.Context, int64, int64) (*int, error) {
	return nil, nil
}

func newLanesRouteTestRouter() (*gin.Engine, string) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{
		ID:               42,
		Status:           service.StatusActive,
		Hydrated:         true,
		Platform:         service.PlatformOpenAI,
		SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier:   0.75,
	}
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}
	groupID := group.ID
	apiKey := &service.APIKey{
		ID:      100,
		UserID:  user.ID,
		Key:     "lanes-route-test-key",
		Status:  service.StatusActive,
		User:    user,
		GroupID: &groupID,
		Group:   group,
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	rateRepo := &lanesRouteRateRepo{}
	apiKeyService := service.NewAPIKeyService(
		&lanesRouteAPIKeyRepo{apiKey: apiKey}, nil, nil, nil, rateRepo, nil, cfg,
	)
	gatewayService := service.NewGatewayService(
		nil, nil, nil, nil, nil, nil, rateRepo, nil, cfg, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	openAIGatewayService := service.NewOpenAIGatewayService(
		nil, nil, nil, nil, nil, rateRepo, nil, cfg, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	gatewayHandler := handler.NewGatewayHandler(
		gatewayService, openAIGatewayService, nil, nil, nil, nil, nil, nil,
		apiKeyService, nil, nil, nil, nil, cfg, nil,
	)

	router := gin.New()
	RegisterGatewayRoutes(
		router,
		&handler.Handlers{Gateway: gatewayHandler, OpenAIGateway: &handler.OpenAIGatewayHandler{}},
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

func TestLanesSkeletonRoutesRegistered(t *testing.T) {
	router, _ := newLanesRouteTestRouter()

	registered := map[string]bool{}
	for _, r := range router.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	for _, r := range lanesSkeletonRoutes {
		require.Truef(t, registered[r.method+" "+r.path], "%s %s should be registered", r.method, r.path)
	}
}

func TestLanesSkeletonAuthChain(t *testing.T) {
	router, key := newLanesRouteTestRouter()

	t.Run("missing key 401 json", func(t *testing.T) {
		for _, r := range lanesSkeletonRoutes {
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(r.method, r.path, nil))

			require.Equalf(t, http.StatusUnauthorized, w.Code, "%s %s", r.method, r.path)
			require.Containsf(t, w.Header().Get("Content-Type"), "application/json", r.path)
		}
	})

	t.Run("valid key 501 lane body", func(t *testing.T) {
		for _, r := range lanesSkeletonRoutes {
			req := httptest.NewRequest(r.method, r.path, nil)
			req.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			require.Equalf(t, http.StatusNotImplemented, w.Code, "%s %s", r.method, r.path)

			var body struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
					Lane    string `json:"lane"`
				} `json:"error"`
			}
			require.NoErrorf(t, json.Unmarshal(w.Body.Bytes(), &body), r.path)
			require.Equalf(t, "lane_not_implemented", body.Error.Type, r.path)
			require.Equalf(t, r.lane, body.Error.Lane, r.path)
			require.NotEmptyf(t, body.Error.Message, r.path)
		}
	})

	t.Run("unregistered method 404 consistent with gateway", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/rerank", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}
