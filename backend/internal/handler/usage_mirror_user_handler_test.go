package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// mirrorRepoCapture 只实现镜像读面需要的那两个方法：
// 嵌 service.UsageLogRepository（nil）取其余方法，是仓内 handler 单测的既有范式
// （见 usage_handler_request_type_test.go 的 userUsageRepoCapture）。
type mirrorRepoCapture struct {
	service.UsageLogRepository
	calls     int
	seenUser  int64
	seenLimit int
	rows      []service.UsageMirror
}

func (s *mirrorRepoCapture) ListMirrors(_ context.Context, userID int64, limit int) ([]service.UsageMirror, error) {
	s.calls++
	s.seenUser = userID
	s.seenLimit = limit
	return s.rows, nil
}

// newMirrorRouter 的 subject 用 withSubject 控制是否塞登录主体；
// 塞的时候显式给 userID，好覆盖「主体在场但 UserID=0」这一档。
func newMirrorRouter(repo *mirrorRepoCapture, withSubject bool, userID int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	usageSvc := service.NewUsageService(repo, nil, nil, nil)
	handler := NewUsageHandler(usageSvc, nil, nil, nil)
	router := gin.New()
	if withSubject {
		router.Use(func(c *gin.Context) {
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID})
			c.Next()
		})
	}
	router.GET("/usage/mirrors", handler.ListMyMirrors)
	return router
}

// 载荷臂：query 里带别人的 user_id 也必须按**登录主体**查。
// 这条是安全边界（越权读别人的套餐消费），其余臂都只是形状。
func TestListMyMirrorsIgnoresQueryUserID(t *testing.T) {
	repo := &mirrorRepoCapture{}
	router := newMirrorRouter(repo, true, 42)

	req := httptest.NewRequest(http.MethodGet, "/usage/mirrors?user_id=99&limit=5", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(42), repo.seenUser, "user_id 必须来自登录主体，不能被 query 覆盖")
	require.Equal(t, 5, repo.seenLimit)
}

func TestListMyMirrorsReturnsOwnRows(t *testing.T) {
	repo := &mirrorRepoCapture{rows: []service.UsageMirror{{
		ID:         7,
		ExternalID: "wu_abc123",
		UserID:     42,
		Minutes:    3,
		Note:       "wanxing plan wp_1 (agent packs-p1-heal)",
		RecordedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}}}
	router := newMirrorRouter(repo, true, 42)

	req := httptest.NewRequest(http.MethodGet, "/usage/mirrors", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Data struct {
			Items []service.UsageMirror `json:"items"`
			Total int                   `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Data.Items, 1)
	require.Equal(t, "wu_abc123", body.Data.Items[0].ExternalID)
	require.Equal(t, 3, body.Data.Items[0].Minutes)
	require.Equal(t, 1, body.Data.Total)
	require.Equal(t, 0, repo.seenLimit, "不带 limit 时交给 service 的缺省值（0）")
}

// 未认证与「主体在但 UserID=0」都必须是 401，且不许触到仓储。
func TestListMyMirrorsRequiresSubjectWithUserID(t *testing.T) {
	for _, tc := range []struct {
		name        string
		withSubject bool
		userID      int64
	}{
		{name: "no subject", withSubject: false},
		{name: "zero user id", withSubject: true, userID: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &mirrorRepoCapture{}
			router := newMirrorRouter(repo, tc.withSubject, tc.userID)

			req := httptest.NewRequest(http.MethodGet, "/usage/mirrors", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			require.Zero(t, repo.calls, "未认证时不许查库")
		})
	}
}

func TestListMyMirrorsRejectsNonIntegerLimit(t *testing.T) {
	repo := &mirrorRepoCapture{}
	router := newMirrorRouter(repo, true, 42)

	req := httptest.NewRequest(http.MethodGet, "/usage/mirrors?limit=abc", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, repo.calls)
}
