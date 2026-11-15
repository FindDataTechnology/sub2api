package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 读面要钉住的不是聚合算法（那在 usagestats 包里测过），而是**这条线真的通**：
// 路由级调用能落到 repo、response 里带着数据与那句维度声明。
// 本仓反复栽过的形态是「函数写了、没人调用」——所以这里从 handler 进去。

type lineStatsRepo struct {
	service.UsageLogRepository // 未实现的方法保持 nil，只覆盖被测的那个
	rows                       []usagestats.LineStatRow
	calledStart, calledEnd     time.Time
}

func (r *lineStatsRepo) GetLineStatsRows(_ context.Context, start, end time.Time) ([]usagestats.LineStatRow, error) {
	r.calledStart, r.calledEnd = start, end
	return r.rows, nil
}

func TestGetLineStatsReturnsMergedLinesAndDimensionNote(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &lineStatsRepo{rows: []usagestats.LineStatRow{
		{KeyName: "wanxing-a", Requests: 100, ActualCost: 30, ListCost: 30},
		{KeyName: "wx-a", Requests: 50, ActualCost: 10, ListCost: 10},
		{KeyName: "413621549-chat", Requests: 20, ActualCost: 60, ListCost: 60},
	}}
	handler := NewDashboardHandler(service.NewDashboardService(repo, nil, nil, nil), nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/dashboard/lines", nil)
	handler.GetLineStats(ctx)

	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, repo.calledStart.IsZero(), "必须真的查了仓库层，不能返回空壳")

	var body struct {
		Data struct {
			Lines []usagestats.LineStat `json:"lines"`
			Note  string                `json:"note"`
			Dim   string                `json:"dimension"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	lines := body.Data.Lines
	require.Len(t, lines, 2, "wanxing+wx 应归并成一条，413621549 落 unknown：共 2 条")

	// 实扣最高的应是 unknown（60 > 40）
	require.Equal(t, usagestats.UnknownLine, lines[0].Line)
	require.Equal(t, []string{"413621549"}, lines[0].UnknownSegs,
		"无法归因的键必须带着原始段名可见，否则读者无法判断是没人归并还是键名脏")
	require.Equal(t, "萬星 wanxing", lines[1].Line)
	require.Equal(t, int64(150), lines[1].Requests)

	// 维度声明必须随数据出去：读面最容易被当成"受益方"口径
	require.Equal(t, "billing_subject", body.Data.Dim)
	require.Contains(t, body.Data.Note, "计费主体")
}

// TestGetLineStatsCarriesRouteKeysAndReadableShare 钉住读面的**诚实性**：
// 只给 lines 时，含路由键的那一行（自有平台）会被渲染成最大的产品线。
// 所以响应必须同时带 route_keys 与 product_readable_share，且 note 要说清第三类。
func TestGetLineStatsCarriesRouteKeysAndReadableShare(t *testing.T) {
	gin.SetMode(gin.TestMode)

	repo := &lineStatsRepo{rows: []usagestats.LineStatRow{
		{KeyName: "特价-a", Requests: 100, ActualCost: 200, ListCost: 200},
		{KeyName: "lex-a", Requests: 10, ActualCost: 20, ListCost: 20},
	}}
	handler := NewDashboardHandler(service.NewDashboardService(repo, nil, nil, nil), nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/dashboard/lines", nil)
	handler.GetLineStats(ctx)

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Data struct {
			Lines           []usagestats.LineStat     `json:"lines"`
			RouteKeys       []usagestats.RouteKeyStat `json:"route_keys"`
			RouteKeyUSD     float64                   `json:"route_key_usd"`
			RouteKeyShare   float64                   `json:"route_key_share"`
			ProductReadable *float64                  `json:"product_readable_share"`
			TotalUSD        float64                   `json:"total_usd"`
			Note            string                    `json:"note"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	require.Len(t, body.Data.RouteKeys, 1)
	require.Equal(t, "特价", body.Data.RouteKeys[0].Seg)
	require.Equal(t, 200.0, body.Data.RouteKeys[0].ActualCost)
	require.Equal(t, 200.0, body.Data.RouteKeyUSD)
	require.Equal(t, 0.9091, body.Data.RouteKeyShare)
	require.Equal(t, 220.0, body.Data.TotalUSD)

	// 可判读 = 1 − 0(未归因) − 200/220
	require.NotNil(t, body.Data.ProductReadable)
	require.Equal(t, 0.0909, *body.Data.ProductReadable)

	// 含路由键的那一行必须自己说明，不能只靠外部读数
	var own *usagestats.LineStat
	for i := range body.Data.Lines {
		if body.Data.Lines[i].Line == "自有平台" {
			own = &body.Data.Lines[i]
		}
	}
	require.NotNil(t, own, "路由键仍归在自有平台（归属不变）")
	require.Equal(t, []string{"特价"}, own.RouteKeys)

	// note 要说出第三类，否则读的人只有「产品线」与「未归因」两档心智模型
	require.Contains(t, body.Data.Note, "route_keys")
	require.Contains(t, body.Data.Note, "product_readable_share")
}
