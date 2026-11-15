package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetLineStats 返回窗口内**按线**聚合的用量，连同路由键读数与可判读覆盖率。
//
// 归并口径的唯一真源是 tools/line-aliases.json（生成到
// usagestats/line_aliases_gen.go）；本方法只负责「取原始段 → 归并」两步，
// 不再定义第二次别名表。
//
// 维度声明（必须随读面一起给出）：本统计的维度是**计费主体**——谁持 key 在调。
// key 名同时编码了「谁在调」与「替哪条线调」（`wanxing-lex-console-copilot`
// 首段是萬星，实际替识律调）。所以看到「萬星占 X%」不等于「萬星的业务量占 X%」。
//
// 为什么返回聚合体而不是 `[]LineStat`：声明为路由键的那些段（`特价` / `openrouter`）
// 落进「自有平台」只是键名归属登记的副作用。只给逐线数组时，读面必然把那一行渲染成
// 最大的产品线——报表与告警已经改口（route_keys + 可判读占比），控制台必须同口径。
func (s *DashboardService) GetLineStats(ctx context.Context, startTime, endTime time.Time) (usagestats.LineAggregate, error) {
	// 用可选接口断言而不是扩大 UsageLogRepository：后者会让所有 mock 都要跟着改，
	// 而这条读面只在真实仓库上成立（同 GetModelStatsWithFiltersBySource 的做法）。
	type lineStatsRepo interface {
		GetLineStatsRows(context.Context, time.Time, time.Time) ([]usagestats.LineStatRow, error)
	}
	repo, ok := s.usageRepo.(lineStatsRepo)
	if !ok {
		return usagestats.LineAggregate{}, fmt.Errorf("usage repo 不支持按线聚合（缺 GetLineStatsRows）")
	}
	rows, err := repo.GetLineStatsRows(ctx, startTime, endTime)
	if err != nil {
		return usagestats.LineAggregate{}, fmt.Errorf("get line stats rows: %w", err)
	}
	return usagestats.AggregateLinesDetailed(rows), nil
}
