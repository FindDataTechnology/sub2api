package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// usage_mirrors 的读写落地。挂在 usageLogRepository 上而不是新开一个 repository 类型，
// 是为了不引入新的构造参数（构造参数变化要重跑 wire 代码生成）。
//
// 为什么与 usage_logs 分开见 migrations/254_add_usage_mirrors.sql 的注释：
// 那张表的 api_key_id / account_id 是 NOT NULL 且带外键，套餐结算两者都没有。

const usageMirrorColumns = `id, external_id, user_id, minutes, note, recorded_at`

// CreateMirror 写入一条镜像行；external_id 已存在时不写第二行，返回首次那行。
// inserted=false 即「这次是重复提交」——契约里 duplicate: true 就是它。
func (r *usageLogRepository) CreateMirror(ctx context.Context, req service.UsageMirrorCreate) (*service.UsageMirror, bool, error) {
	sqlq, err := r.mirrorExecutor()
	if err != nil {
		return nil, false, err
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, false, ctx.Err()
	}

	insert := `
		INSERT INTO usage_mirrors (external_id, user_id, minutes, note, recorded_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (external_id) DO NOTHING
		RETURNING ` + usageMirrorColumns

	mirror := &service.UsageMirror{}
	err = scanSingleRow(ctx, sqlq, insert,
		[]any{req.ExternalID, req.UserID, req.Minutes, req.Note, req.RecordedAt},
		&mirror.ID, &mirror.ExternalID, &mirror.UserID, &mirror.Minutes, &mirror.Note, &mirror.RecordedAt)
	if err == nil {
		return mirror, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("insert usage mirror: %w", err)
	}

	// 唯一索引命中：读回首次那行。「重复提交返回首次结果」不是覆盖，也不是报错。
	first, err := r.getMirrorByExternalID(ctx, sqlq, req.ExternalID)
	if err != nil {
		return nil, false, err
	}
	return first, false, nil
}

// GetMirrorByExternalID 读一条镜像行；不存在时返回 (nil, nil)。
func (r *usageLogRepository) GetMirrorByExternalID(ctx context.Context, externalID string) (*service.UsageMirror, error) {
	sqlq, err := r.mirrorExecutor()
	if err != nil {
		return nil, err
	}
	mirror, err := r.getMirrorByExternalID(ctx, sqlq, externalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return mirror, err
}

// ListMirrors 按用户列最近的镜像行（新的在前）。
func (r *usageLogRepository) ListMirrors(ctx context.Context, userID int64, limit int) ([]service.UsageMirror, error) {
	sqlq, err := r.mirrorExecutor()
	if err != nil {
		return nil, err
	}
	query := `
		SELECT ` + usageMirrorColumns + `
		FROM usage_mirrors
		WHERE user_id = $1
		ORDER BY recorded_at DESC, id DESC
		LIMIT $2`

	rows, err := sqlq.QueryContext(ctx, query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list usage mirrors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]service.UsageMirror, 0, limit)
	for rows.Next() {
		var m service.UsageMirror
		if err := rows.Scan(&m.ID, &m.ExternalID, &m.UserID, &m.Minutes, &m.Note, &m.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan usage mirror: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage mirrors: %w", err)
	}
	return out, nil
}

func (r *usageLogRepository) getMirrorByExternalID(ctx context.Context, sqlq sqlExecutor, externalID string) (*service.UsageMirror, error) {
	query := `SELECT ` + usageMirrorColumns + ` FROM usage_mirrors WHERE external_id = $1`
	mirror := &service.UsageMirror{}
	err := scanSingleRow(ctx, sqlq, query, []any{externalID},
		&mirror.ID, &mirror.ExternalID, &mirror.UserID, &mirror.Minutes, &mirror.Note, &mirror.RecordedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("get usage mirror: %w", err)
	}
	return mirror, nil
}

// mirrorExecutor 取可用执行器：优先事务执行器（r.sql），否则退回裸库句柄。
func (r *usageLogRepository) mirrorExecutor() (sqlExecutor, error) {
	if r.sql != nil {
		return r.sql, nil
	}
	if r.db != nil {
		return r.db, nil
	}
	return nil, errors.New("usage mirror store unavailable: no sql executor")
}
