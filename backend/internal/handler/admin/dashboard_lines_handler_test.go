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
		{Seg: "wanxing", Requests: 100, ActualCost: 30, ListCost: 30},
		{Seg: "wx", Requests: 50, ActualCost: 10, ListCost: 10},
		{Seg: "特价", Requests: 20, ActualCost: 60, ListCost: 60},
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
	require.Len(t, lines, 2, "wanxing+wx 应归并成一条，特价 落 unknown：共 2 条")

	// 实扣最高的应是 unknown（60 > 40）
	require.Equal(t, usagestats.UnknownLine, lines[0].Line)
	require.Equal(t, []string{"特价"}, lines[0].UnknownSegs,
		"无法归因的键必须带着原始段名可见，否则读者无法判断是没人归并还是键名脏")
	require.Equal(t, "萬星 wanxing", lines[1].Line)
	require.Equal(t, int64(150), lines[1].Requests)

	// 维度声明必须随数据出去：读面最容易被当成"受益方"口径
	require.Equal(t, "billing_subject", body.Data.Dim)
	require.Contains(t, body.Data.Note, "计费主体")
}
