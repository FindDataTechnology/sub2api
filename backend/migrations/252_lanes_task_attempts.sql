-- 252: 任务车道·尝试表（add-ciyuan-task-lane S3.3）
--
-- 每次执行尝试一行。为什么必须单独一张表：`task_jobs.attempts` 只能记次数，
-- 记不了「哪一次是被 429 挡回的、哪一次是上游 502、每次花了多少钱」。
-- 排障时唯一能回答「这个任务到底试了什么」的就是这张表。
--
-- 关键血缘约束：request_id 每次尝试**必须不同**。
-- usage_logs 上有 (request_id, api_key_id) 唯一索引（迁移 027），
-- 同一任务重试若复用同一个 request_id，第二次的用量行会撞唯一索引 →
-- 计费行写不进去，而任务却显示成功 —— 账单与账面直接分叉。
-- 故 request_id 形如 task-<job_id>-<attempt_no>，attempt_no 进唯一键。

CREATE TABLE IF NOT EXISTS lanes_task_attempts (
    id            BIGSERIAL PRIMARY KEY,
    job_id        BIGINT      NOT NULL REFERENCES lanes_task_jobs(id) ON DELETE CASCADE,
    attempt_no    INTEGER     NOT NULL,
    request_id    TEXT        NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    http_status   INTEGER,
    -- completed | retry_queue_full | retry_transient | failed_client | failed_deadline
    outcome       TEXT,
    error_message TEXT,
    cost_usd      NUMERIC(18,8),
    UNIQUE (job_id, attempt_no)
);

CREATE INDEX IF NOT EXISTS idx_lanes_task_attempts_job
    ON lanes_task_attempts (job_id);

-- 观测用：近窗完成速率（class 容量观测与队列等待估算都读它，不读账号配置）。
CREATE INDEX IF NOT EXISTS idx_lanes_task_attempts_finished
    ON lanes_task_attempts (finished_at)
    WHERE outcome IS NOT NULL;
