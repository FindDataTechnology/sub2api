package admin

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
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
// 因为并进去就是编数字（`特价` 一把键占全系统花费 31%，并到哪条线都是错的）。
func (h *DashboardHandler) GetLineStats(c *gin.Context) {
	startTime, endTime := parseTimeRange(c)

	lines, err := h.dashboardService.GetLineStats(c.Request.Context(), startTime, endTime)
	if err != nil {
		response.Error(c, 500, "Failed to get line statistics")
		return
	}

	response.Success(c, gin.H{
		"lines":      lines,
		"start_date": startTime.Format("2006-01-02"),
		"end_date":   endTime.Add(-24 * time.Hour).Format("2006-01-02"),
		// 维度声明随数据一起给：读面最容易犯的错是把它当成"受益方"口径。
		"dimension": "billing_subject",
		"note": "维度=计费主体（谁持 key 调用），不是受益方；" +
			"`wanxing-lex-console-copilot` 之类键的首段是调用方，实际可能替别的线调。" +
			"unknown 桶是未能归因的键，已单列而未被并入任何线。",
	})
}
