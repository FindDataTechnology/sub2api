package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetLineStats 返回窗口内**按线**聚合的用量。
//
// 归并口径的唯一真源是 tools/line-aliases.json（生成到
// usagestats/line_aliases_gen.go）；本方法只负责「取原始段 → 归并」两步，
// 不再定义第二次别名表。
//
// 维度声明（必须随读面一起给出）：本统计的维度是**计费主体**——谁持 key 在调。
// key 名同时编码了「谁在调」与「替哪条线调」（`wanxing-lex-console-copilot`
// 首段是萬星，实际替识律调）。所以看到「萬星占 X%」不等于「萬星的业务量占 X%」。
func (s *DashboardService) GetLineStats(ctx context.Context, startTime, endTime time.Time) ([]usagestats.LineStat, error) {
	// 用可选接口断言而不是扩大 UsageLogRepository：后者会让所有 mock 都要跟着改，
	// 而这条读面只在真实仓库上成立（同 GetModelStatsWithFiltersBySource 的做法）。
	type lineStatsRepo interface {
		GetLineStatsRows(context.Context, time.Time, time.Time) ([]usagestats.LineStatRow, error)
	}
	repo, ok := s.usageRepo.(lineStatsRepo)
	if !ok {
		return nil, fmt.Errorf("usage repo 不支持按线聚合（缺 GetLineStatsRows）")
	}
	rows, err := repo.GetLineStatsRows(ctx, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("get line stats rows: %w", err)
	}
	return usagestats.AggregateLines(rows), nil
}
