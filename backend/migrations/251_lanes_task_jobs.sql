-- 251: 任务车道·任务表（add-ciyuan-task-lane S2）
--
-- 为什么需要持久化：mode=defer 的任务可能等 4 小时到低谷，进程重启不能丢；
-- 且「没有任务会静默越过 deadline」这条不变量只有在库里可查时才可验（I3）。
--
-- 幂等键 external_id 与 api_key_id 组合唯一（同 key 下不重复），
-- 与 usage_logs 的 (request_id, api_key_id) 同一纪律；NULL 允许重复（不传 external_id 就不去重）。
--
-- 估价与实扣分两列且**互不覆盖**：estimated_cost_usd 是提交时按 max_tokens 上界算的
-- 上界估价，actual_cost_usd 来自 RecordUsage 的实扣。两者同时可读，不允许把估价当账单。
--
-- 迁移编号说明：本 change 设计时计划用 250，但 250 已被 add-key-capacity-contract
-- 占用（2026-11-11 已上产），故顺延到 251/252。

CREATE TABLE IF NOT EXISTS lanes_task_jobs (
    id                  BIGSERIAL PRIMARY KEY,
    external_id         TEXT,
    api_key_id          BIGINT       NOT NULL,
    user_id             BIGINT       NOT NULL,
    group_id            BIGINT,
    platform            TEXT         NOT NULL DEFAULT '',
    model               TEXT         NOT NULL DEFAULT '',
    mode                TEXT         NOT NULL,
    status              TEXT         NOT NULL,
    -- 提交时的客户端 IP：重放必须带同一个来源，否则带 IP 白名单的 key
    -- 会在执行时被自己的鉴权链拒掉（任务失败而调用方看起来完全合规）。
    client_ip           TEXT         NOT NULL DEFAULT '',
    -- 客户端原始请求体（不含认证头，不含 key）。服务端执行时原样重放。
    request_body        JSONB        NOT NULL,
    deadline_at         TIMESTAMPTZ,
    deferred_until      TIMESTAMPTZ,
    next_attempt_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    attempts            INTEGER      NOT NULL DEFAULT 0,
    max_attempts        INTEGER      NOT NULL DEFAULT 6,
    -- 峰谷快照：提交时刻的倍率与推迟决策原因（可观测，不静默）。
    peak_multiplier     NUMERIC(6,3),
    defer_reason        TEXT,
    estimated_cost_usd  NUMERIC(18,8),
    actual_cost_usd     NUMERIC(18,8),
    result_status       INTEGER,
    result_body         JSONB,
    result_truncated    BOOLEAN      NOT NULL DEFAULT false,
    reason              TEXT,
    last_request_id     TEXT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ
);

-- status: pending | running | completed | failed
ALTER TABLE lanes_task_jobs
    DROP CONSTRAINT IF EXISTS lanes_task_jobs_mode_check,
    ADD CONSTRAINT lanes_task_jobs_mode_check CHECK (mode IN ('now', 'defer'));

ALTER TABLE lanes_task_jobs
    DROP CONSTRAINT IF EXISTS lanes_task_jobs_status_check,
    ADD CONSTRAINT lanes_task_jobs_status_check
        CHECK (status IN ('pending', 'running', 'completed', 'failed'));

-- 幂等：同一 key 下 external_id 唯一。NULL 不参与唯一（Postgres 唯一索引对 NULL 天然放行）。
CREATE UNIQUE INDEX IF NOT EXISTS uq_lanes_task_jobs_external
    ON lanes_task_jobs (api_key_id, external_id)
    WHERE external_id IS NOT NULL;

-- 调度扫描：只扫 pending，按 next_attempt_at 就绪排序。
CREATE INDEX IF NOT EXISTS idx_lanes_task_jobs_ready
    ON lanes_task_jobs (next_attempt_at)
    WHERE status = 'pending';

-- deadline 淘汰扫描：I3（pending 且 deadline < now 的行数恒为 0）靠这个索引的扫描保证。
CREATE INDEX IF NOT EXISTS idx_lanes_task_jobs_deadline
    ON lanes_task_jobs (deadline_at)
    WHERE status = 'pending';

-- 单 key 的活跃任务数（队列上界判定）+ 用户维度统计。
CREATE INDEX IF NOT EXISTS idx_lanes_task_jobs_key_active
    ON lanes_task_jobs (api_key_id, status);
