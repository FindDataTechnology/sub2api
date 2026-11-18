package admin

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetLineStats 返回窗口内按线聚合的用量（词元控制台的「按线」视图）。
//
// GET /api/v1/admin/dashboard/lines?start_date=&end_date=
//
// 只给运维/站长看（admin 组）：按线视图会暴露其它线的名字，
// 对客户是否可见属产品决定，尚未裁定。
//
// 归并口径：线身份 = key 名首段，别名表是 tools/line-aliases.json（生成到
// usagestats 包）。无法归因的键落 unknown 桶并带上原始段名——**不并入任何线**，
// 因为并进去就是编数字（`特价` 一把键占全系统花费八成以上，并到哪条线都是错的）。
//
// 路由键（`route:true`，当前是 `特价` / `openrouter`）是第三种东西：**已知是谁、但不可拆**。
// 它们单列成 route_keys，并给出 product_readable_share——只给 lines 时读面必然把
// 「自有平台」那一行画成最大的产品线。
func (h *DashboardHandler) GetLineStats(c *gin.Context) {
	startTime, endTime := parseTimeRange(c)

	agg, err := h.dashboardService.GetLineStats(c.Request.Context(), startTime, endTime)
	if err != nil {
		response.Error(c, 500, "Failed to get line statistics")
		return
	}

	response.Success(c, gin.H{
		"lines":           agg.Lines,
		"route_keys":      agg.RouteKeys,
		"route_key_usd":   agg.RouteKeyUSD,
		"route_key_share": agg.RouteKeyShare,
		// nil 会序列化成 null：窗口内没有花费时，这不是「100% 可判读」。
		"product_readable_share": agg.ProductReadableShare,
		"total_usd":              agg.TotalUSD,
		"start_date":             startTime.Format("2006-01-02"),
		"end_date":               endTime.Add(-24 * time.Hour).Format("2006-01-02"),
		// 维度声明随数据一起给：读面最容易犯的错是把它当成"受益方"口径。
		"dimension": "billing_subject",
		"note": "维度=计费主体（谁持 key 调用），不是受益方；" +
			"`wanxing-lex-console-copilot` 之类键的首段是调用方，实际可能替别的线调。" +
			"unknown 桶是未能归因的键，已单列而未被并入任何线。" +
			"route_keys 是**已知但不可拆**的第三类：键名编码上游折扣路由、不含线信息，" +
			"归在某条线下只是键名归属登记的副作用——product_readable_share 已把这两类都扣掉，" +
			"它才是真正能逐线判读的份额。",
	})
}
