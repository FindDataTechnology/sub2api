//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// effective_at 语义（add-key-capacity-contract）：真跑中间件，而不是断言我自己的表达式。
//
// 一个只断言 `future.After(now)` 的测试是**同义反复**：它测的是测试自己写的表达式，
// 守卫接错地方、接在不可达分支里，它照样绿。所以这里走既有 harness
// （stubApiKeyRepo + NewAPIKeyService），从 HTTP 入口驱动整条鉴权链。

func setClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = prev })
}

func runAuthWithKey(t *testing.T, apiKey *service.APIKey) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repo := &stubApiKeyRepo{
		getByKey: func(_ context.Context, key string) (*service.APIKey, error) {
			if key != apiKey.Key {
				return nil, service.ErrAPIKeyNotFound
			}
			clone := *apiKey
			return &clone, nil
		},
		updateLastUsed: func(context.Context, int64, time.Time) error { return nil },
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
	router.GET("/t", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	req.Header.Set("x-api-key", apiKey.Key)
	router.ServeHTTP(w, req)
	return w
}

func baseEffectiveAtKey() *service.APIKey {
	group := &service.Group{
		ID: 101, Name: "g1", Status: service.StatusActive,
		Platform: service.PlatformAnthropic, Hydrated: true,
	}
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive, Balance: 10, Concurrency: 3}
	k := &service.APIKey{
		ID: 100, UserID: user.ID, Key: "test-key", Status: service.StatusActive,
		User: user, Group: group,
	}
	k.GroupID = &group.ID
	return k
}

func TestEffectiveAt_FutureKeyIsRejectedWith403(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	setClock(t, now)

	k := baseEffectiveAtKey()
	future := now.Add(2 * time.Hour)
	k.EffectiveAt = &future

	w := runAuthWithKey(t, k)

	require.Equal(t, http.StatusForbidden, w.Code, "未生效必须是 403，不能是 401")
	require.Contains(t, w.Body.String(), "KEY_NOT_YET_EFFECTIVE")
	require.Contains(t, w.Body.String(), "2026-11-11T12:00:00Z", "必须回传生效时刻，便于调用方等待")
}

func TestEffectiveAt_PastKeyIsAllowed(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	setClock(t, now)

	k := baseEffectiveAtKey()
	past := now.Add(-time.Minute)
	k.EffectiveAt = &past

	w := runAuthWithKey(t, k)
	require.Equal(t, http.StatusOK, w.Code, "已到点必须放行（不重启即生效）")
}

func TestEffectiveAt_ExactlyNowIsAllowed(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	setClock(t, now)

	k := baseEffectiveAtKey()
	at := now
	k.EffectiveAt = &at

	w := runAuthWithKey(t, k)
	require.Equal(t, http.StatusOK, w.Code, "恰好等于生效时刻应放行")
}

func TestEffectiveAt_NilIsAllowedAndUnchanged(t *testing.T) {
	// nil = 创建即生效：存量 key 全在此，行为必须与改动前一致
	k := baseEffectiveAtKey()
	w := runAuthWithKey(t, k)
	require.Equal(t, http.StatusOK, w.Code)
}
