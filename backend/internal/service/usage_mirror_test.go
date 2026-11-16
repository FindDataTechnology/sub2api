package service

import (
	"context"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 镜像端点的服务层单测。
//
// 替身策略：嵌入接口再把要用的方法覆盖掉。UsageLogRepository / UserRepository 各有
// 二十来个方法，逐条实现只会让这个文件淹没在无关代码里；嵌入接口后，未覆盖的方法
// 在 nil 上解引用会 panic —— 这恰好证明「本测试只走镜像路径」。

type mirrorRepoStub struct {
	UsageLogRepository
	rows   map[string]*UsageMirror
	nextID int64
	// creates 记录真正发生插入的次数（重复提交不该增加它）
	creates int
}

func newMirrorRepoStub() *mirrorRepoStub {
	return &mirrorRepoStub{rows: map[string]*UsageMirror{}, nextID: 1}
}

func (r *mirrorRepoStub) CreateMirror(_ context.Context, req UsageMirrorCreate) (*UsageMirror, bool, error) {
	if existing, ok := r.rows[req.ExternalID]; ok {
		// 唯一索引命中：返回首次那行，inserted=false
		copied := *existing
		return &copied, false, nil
	}
	r.creates++
	row := &UsageMirror{
		ID:         r.nextID,
		ExternalID: req.ExternalID,
		UserID:     req.UserID,
		Minutes:    req.Minutes,
		Note:       req.Note,
		RecordedAt: req.RecordedAt,
	}
	r.nextID++
	r.rows[req.ExternalID] = row
	copied := *row
	return &copied, true, nil
}

func (r *mirrorRepoStub) ListMirrors(_ context.Context, userID int64, limit int) ([]UsageMirror, error) {
	out := make([]UsageMirror, 0, len(r.rows))
	for _, row := range r.rows {
		if row.UserID == userID && len(out) < limit {
			out = append(out, *row)
		}
	}
	return out, nil
}

// mirrorUserRepoStub 只实现 GetByID；known 里的 id 视为存在。
type mirrorUserRepoStub struct {
	UserRepository
	known map[int64]bool
}

func (r *mirrorUserRepoStub) GetByID(_ context.Context, id int64) (*User, error) {
	if !r.known[id] {
		return nil, ErrUserNotFound
	}
	return &User{ID: id}, nil
}

func newMirrorService(repo UsageLogRepository, users map[int64]bool) *UsageService {
	return &UsageService{usageRepo: repo, userRepo: &mirrorUserRepoStub{known: users}}
}

func TestRecordMirrorInsertsOnce(t *testing.T) {
	repo := newMirrorRepoStub()
	svc := newMirrorService(repo, map[int64]bool{7: true})

	mirror, duplicate, err := svc.RecordMirror(context.Background(), UsageMirrorCreate{
		ExternalID: "wu_abc123",
		UserID:     7,
		Minutes:    3,
		Note:       "wanxing plan wp_ab12cd (agent packs-p1-heal)",
		RecordedAt: time.UnixMilli(1760000000000),
	})
	if err != nil {
		t.Fatalf("RecordMirror: %v", err)
	}
	if duplicate {
		t.Fatalf("首次写入不该被报成重复")
	}
	if mirror == nil || mirror.ID != 1 {
		t.Fatalf("首次写入应返回 usage_id=1，实得 %+v", mirror)
	}
	if repo.creates != 1 {
		t.Fatalf("应只插入一次，实得 %d", repo.creates)
	}
}

// 这一条是契约的核心：重复提交返回**首次**结果，不双记。
func TestRecordMirrorReplayReturnsFirstResult(t *testing.T) {
	repo := newMirrorRepoStub()
	svc := newMirrorService(repo, map[int64]bool{7: true})
	req := UsageMirrorCreate{ExternalID: "wu_replay", UserID: 7, Minutes: 3}

	first, dupFirst, err := svc.RecordMirror(context.Background(), req)
	if err != nil || dupFirst {
		t.Fatalf("首次写入应成功且非重复：err=%v dup=%v", err, dupFirst)
	}

	// 同一 external_id 再来一次，且 payload 故意不同：仍必须回首次那行。
	req.Minutes = 999
	second, dupSecond, err := svc.RecordMirror(context.Background(), req)
	if err != nil {
		t.Fatalf("重复提交不该报错：%v", err)
	}
	if !dupSecond {
		t.Fatalf("重复提交必须 duplicate=true")
	}
	if second.ID != first.ID {
		t.Fatalf("重复提交必须返回首次 id=%d，实得 %d", first.ID, second.ID)
	}
	if second.Minutes != first.Minutes {
		t.Fatalf("重复提交必须返回首次的 minutes=%d，实得 %d", first.Minutes, second.Minutes)
	}
	if repo.creates != 1 {
		t.Fatalf("重复提交不该插入第二行，creates=%d", repo.creates)
	}
}

func TestRecordMirrorValidation(t *testing.T) {
	cases := []struct {
		name     string
		req      UsageMirrorCreate
		wantCode string
	}{
		{
			name:     "external_id 空白",
			req:      UsageMirrorCreate{ExternalID: "   ", UserID: 7},
			wantCode: "USAGE_MIRROR_EXTERNAL_ID_REQUIRED",
		},
		{
			name:     "external_id 含控制字符",
			req:      UsageMirrorCreate{ExternalID: "wu_\x00bad", UserID: 7},
			wantCode: "USAGE_MIRROR_EXTERNAL_ID_INVALID",
		},
		{
			name:     "external_id 超长",
			req:      UsageMirrorCreate{ExternalID: repeatAscii(129), UserID: 7},
			wantCode: "USAGE_MIRROR_EXTERNAL_ID_INVALID",
		},
		{
			name:     "缺 user_id",
			req:      UsageMirrorCreate{ExternalID: "wu_x"},
			wantCode: "USAGE_MIRROR_USER_REQUIRED",
		},
		{
			name:     "minutes 为负",
			req:      UsageMirrorCreate{ExternalID: "wu_x", UserID: 7, Minutes: -1},
			wantCode: "USAGE_MIRROR_MINUTES_INVALID",
		},
		{
			name:     "用户不存在",
			req:      UsageMirrorCreate{ExternalID: "wu_x", UserID: 999},
			wantCode: "USAGE_MIRROR_USER_NOT_FOUND",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMirrorRepoStub()
			svc := newMirrorService(repo, map[int64]bool{7: true})
			_, _, err := svc.RecordMirror(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("期望报错 %s，实得 nil", tc.wantCode)
			}
			if got := infraerrors.Code(err); got != tc.wantCode {
				t.Fatalf("期望错误码 %s，实得 %s（err=%v）", tc.wantCode, got, err)
			}
			if repo.creates != 0 {
				t.Fatalf("校验失败不该落库，creates=%d", repo.creates)
			}
		})
	}
}

// 存储取不到镜像能力时必须明确报 store unavailable —— 不许静默当成功。
func TestRecordMirrorStoreUnavailable(t *testing.T) {
	type plainRepo struct{ UsageLogRepository } // 只有基础接口，没有镜像方法
	svc := newMirrorService(&plainRepo{}, map[int64]bool{7: true})

	_, _, err := svc.RecordMirror(context.Background(), UsageMirrorCreate{ExternalID: "wu_x", UserID: 7})
	if err == nil {
		t.Fatal("期望 store unavailable，实得 nil")
	}
	if got := infraerrors.Code(err); got != "USAGE_MIRROR_STORE_UNAVAILABLE" {
		t.Fatalf("期望 USAGE_MIRROR_STORE_UNAVAILABLE，实得 %s", got)
	}
}

func TestListMirrorsClampsLimit(t *testing.T) {
	repo := newMirrorRepoStub()
	svc := newMirrorService(repo, map[int64]bool{7: true})
	for _, id := range []string{"wu_1", "wu_2", "wu_3"} {
		if _, _, err := svc.RecordMirror(context.Background(), UsageMirrorCreate{ExternalID: id, UserID: 7}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// limit=0 → 默认值；不该报错，也不该返回空
	rows, err := svc.ListMirrors(context.Background(), 7, 0)
	if err != nil {
		t.Fatalf("ListMirrors: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("默认 limit 应取到 3 行，实得 %d", len(rows))
	}

	// 超大 limit 被夹到上限，但结果集不受影响（3 行）
	rows, err = svc.ListMirrors(context.Background(), 7, 100000)
	if err != nil {
		t.Fatalf("ListMirrors(大 limit): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("夹上限不该丢行，实得 %d", len(rows))
	}

	if _, err := svc.ListMirrors(context.Background(), 0, 10); infraerrors.Code(err) != "USAGE_MIRROR_USER_REQUIRED" {
		t.Fatalf("缺 user_id 应报 USER_REQUIRED，实得 %v", err)
	}
}

func repeatAscii(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}
