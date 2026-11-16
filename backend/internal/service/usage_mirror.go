package service

import (
	"context"
	"errors"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 套餐/外部结算的虚拟用量镜像（add-serving-plans D6）。
// 契约正本：fd-wanxing/docs/sub2api-usage-mirror.md。
//
// 语义三条，缺一条就不是镜像：
// 1. 只增展示行——不动余额、不触发配额窗口、不写 usage_logs；
// 2. external_id 永久幂等——重复提交返回首次结果，而不是报错或追加元；
// 3. 上游（萬星）失败不阻塞它自己的结算——本端点的失败只影响「面板看不看得到」。

const (
	// UsageMirrorExternalIDMaxLen 与 usage_mirrors.external_id VARCHAR(128) 对齐。
	UsageMirrorExternalIDMaxLen = 128
	// UsageMirrorNoteMaxLen 与 usage_mirrors.note VARCHAR(512) 对齐。
	UsageMirrorNoteMaxLen = 512
	// UsageMirrorDefaultListLimit / UsageMirrorMaxListLimit 列表默认与上限。
	UsageMirrorDefaultListLimit = 50
	UsageMirrorMaxListLimit     = 200
)

var (
	ErrUsageMirrorExternalIDRequired = infraerrors.BadRequest("USAGE_MIRROR_EXTERNAL_ID_REQUIRED", "external_id is required")
	ErrUsageMirrorExternalIDInvalid  = infraerrors.BadRequest("USAGE_MIRROR_EXTERNAL_ID_INVALID", "external_id must be 1-128 printable ASCII characters")
	ErrUsageMirrorUserRequired       = infraerrors.BadRequest("USAGE_MIRROR_USER_REQUIRED", "user_id is required")
	ErrUsageMirrorMinutesInvalid     = infraerrors.BadRequest("USAGE_MIRROR_MINUTES_INVALID", "minutes must be >= 0")
	ErrUsageMirrorNoteTooLong        = infraerrors.BadRequest("USAGE_MIRROR_NOTE_TOO_LONG", "note must be at most 512 bytes")
	ErrUsageMirrorUserNotFound       = infraerrors.NotFound("USAGE_MIRROR_USER_NOT_FOUND", "user not found")
	ErrUsageMirrorStoreUnavailable   = infraerrors.ServiceUnavailable("USAGE_MIRROR_STORE_UNAVAILABLE", "usage mirror store unavailable")
)

// UsageMirrorCreate 是一次镜像写入请求（已通过校验的形态）。
type UsageMirrorCreate struct {
	ExternalID string
	UserID     int64
	Minutes    int
	Note       string
	RecordedAt time.Time
}

// UsageMirror 是一条已落库的镜像行。
type UsageMirror struct {
	ID         int64     `json:"id"`
	ExternalID string    `json:"external_id"`
	UserID     int64     `json:"user_id"`
	Minutes    int       `json:"minutes"`
	Note       string    `json:"note"`
	RecordedAt time.Time `json:"recorded_at"`
}

// usageMirrorRepository 是镜像的写读口。
//
// 刻意**不**并进 UsageLogRepository：那个接口在 handler/service 的单测里有十几个替身，
// 加方法会让它们全线失效；而且镜像行与逐次调用日志是两件事（见 migrations/254）。
// 生产里 concrete *usageLogRepository 同时满足两个接口，单测里替身按需只实现这一个。
type usageMirrorRepository interface {
	CreateMirror(ctx context.Context, req UsageMirrorCreate) (*UsageMirror, bool, error)
	ListMirrors(ctx context.Context, userID int64, limit int) ([]UsageMirror, error)
}

// mirrorRepository 取镜像存储；取不到时明确报 store unavailable，绝不静默降级成「写成功」。
func (s *UsageService) mirrorRepository() (usageMirrorRepository, error) {
	repo, ok := s.usageRepo.(usageMirrorRepository)
	if !ok || repo == nil {
		return nil, ErrUsageMirrorStoreUnavailable
	}
	return repo, nil
}

// RecordMirror 写入一条镜像行，返回落库行与「这次是不是重复提交」。
// duplicate=true 时返回的是**首次**那行（id 也是首次的），契约与 usage_logs 的
// (request_id, api_key_id) 冲突语义一致。
func (s *UsageService) RecordMirror(ctx context.Context, req UsageMirrorCreate) (*UsageMirror, bool, error) {
	externalID, err := normalizeUsageMirrorExternalID(req.ExternalID)
	if err != nil {
		return nil, false, err
	}
	if req.UserID <= 0 {
		return nil, false, ErrUsageMirrorUserRequired
	}
	if req.Minutes < 0 {
		return nil, false, ErrUsageMirrorMinutesInvalid
	}
	if len(req.Note) > UsageMirrorNoteMaxLen {
		return nil, false, ErrUsageMirrorNoteTooLong
	}

	// 用户必须存在：镜像挂在具体用户名下，凭空写一行等于把账记到没人的名下。
	if _, err := s.userRepo.GetByID(ctx, req.UserID); err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, false, ErrUsageMirrorUserNotFound
		}
		return nil, false, err
	}

	repo, err := s.mirrorRepository()
	if err != nil {
		return nil, false, err
	}

	recordedAt := req.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now()
	}

	mirror, inserted, err := repo.CreateMirror(ctx, UsageMirrorCreate{
		ExternalID: externalID,
		UserID:     req.UserID,
		Minutes:    req.Minutes,
		Note:       req.Note,
		RecordedAt: recordedAt,
	})
	if err != nil {
		return nil, false, err
	}
	return mirror, !inserted, nil
}

// ListMirrors 按用户列最近的镜像行。
func (s *UsageService) ListMirrors(ctx context.Context, userID int64, limit int) ([]UsageMirror, error) {
	if userID <= 0 {
		return nil, ErrUsageMirrorUserRequired
	}
	if limit <= 0 {
		limit = UsageMirrorDefaultListLimit
	}
	if limit > UsageMirrorMaxListLimit {
		limit = UsageMirrorMaxListLimit
	}
	repo, err := s.mirrorRepository()
	if err != nil {
		return nil, err
	}
	return repo.ListMirrors(ctx, userID, limit)
}

// normalizeUsageMirrorExternalID 校验并归一幂等键。
// 只收可见 ASCII：它会进 URL 查询与日志，空白/控制字符在多一层编码后是另一个键，
// 「重放同一条结算」就会变成两行记录。
func normalizeUsageMirrorExternalID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", ErrUsageMirrorExternalIDRequired
	}
	if len(id) > UsageMirrorExternalIDMaxLen {
		return "", ErrUsageMirrorExternalIDInvalid
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 33 || id[i] > 126 {
			return "", ErrUsageMirrorExternalIDInvalid
		}
	}
	return id, nil
}
