package lanes

import (
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 任务车道的错误面与请求号（add-ciyuan-task-lane）。
//
// 与三车道共用同一个 laneError 形状（error.type + message + lane），
// 好处是调用方只需按 type 分类、不必为任务车道再写一套解析。

// LaneTask 是任务车道的 lane 标识。
const LaneTask = "tasks"

var (
	errDeadlineFormat = errors.New("deadline must be RFC3339, e.g. 2026-11-12T02:00:00Z")
	errDeadlinePast   = errors.New("deadline must be in the future")
)

// taskUnavailable 任务车道未装配（无 DB）。**必须 503 而不是假装成功**：
// 「没跑」与「跑了没走到」在调用方眼里必须可分。
func taskUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
		"type": "lane_unavailable", "lane": LaneTask, "code": ReasonLaneUnavailable,
		"message": "task lane is not configured (database not wired)",
	}})
}

func taskUnauthorized(c *gin.Context) {
	c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{
		"type": "unauthorized", "lane": LaneTask, "message": "missing api key",
	}})
}

func taskBadRequest(c *gin.Context, typ, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"type": typ, "lane": LaneTask, "message": message,
	}})
}

// taskReject 是「当场拒」的统一形状：带机器可读 code，便于调用方程序化处理。
func taskReject(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{
		"type": "request_rejected", "lane": LaneTask, "code": code, "message": message,
	}})
}

func taskStoreError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrLanesTaskStoreUnavailable) {
		taskUnavailable(c)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
		"type": "lane_storage_error", "lane": LaneTask, "message": err.Error(),
	}})
}

var taskNowSeq atomic.Int64

// taskNowRequestID 为 mode=now 的一次性执行生成 request_id。
//
// 字符集只用 [a-z0-9-]：middleware.ClientRequestID 会对上下文里的值做
// normalizeCorrelationID，含冒号等字符会被改写成新 uuid，
// 那样 usage_logs 里的 id 就与我们记录的不一致，血缘断掉。
func taskNowRequestID() string {
	return "task-now-" + itoa(time.Now().UnixNano()) + "-" + itoa(taskNowSeq.Add(1))
}
