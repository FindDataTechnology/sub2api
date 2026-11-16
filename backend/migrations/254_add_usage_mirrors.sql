-- 254_add_usage_mirrors.sql
-- 套餐/外部结算的虚拟用量镜像（add-serving-plans D6）。
--
-- 为什么不写进 usage_logs
-- ──────────────────────
-- usage_logs 的 api_key_id / account_id 都是 NOT NULL 且带外键
-- （usage_logs_api_key_id_fkey / usage_logs_account_id_fkey，2026-11-16 在线上
-- sub2api 库用 pg_constraint 复核过），而一次「套餐结算」既没有上游账号，也不属于
-- 某个 API Key 的逐次调用。硬塞就只能借一个真实 api_key/account 的 id 冒充出处
-- ——那是在计费台账里写假 provenance，比没有这张表更糟。
--
-- 所以镜像有自己的表：只增、不改、不移动余额、不触发配额窗口。
-- 幂等由 external_id 的唯一索引保证（不是内存表、不是 TTL 缓存）：
-- 萬星重放同一条结算，落点仍然只有一行，且返回首次那行的 id。
--
-- 2026-11-17 已在生产库（sub2api@cheap，事务内 ROLLBACK）用同构 DDL 实跑验证：
-- 首次 INSERT … ON CONFLICT DO NOTHING RETURNING 返回 1 行；同 external_id 第二次
-- 返回 **0 行**（＝仓储层 inserted=false ⇒ duplicate=true）；随后按 external_id
-- 读回的是**首次**那行（minutes 仍是第一次的值，不是第二次的），行数恰为 1。
-- 注意：迁移文件一旦上产就受 schema_migrations 的 checksum 守卫，之后不再可改。
CREATE TABLE IF NOT EXISTS usage_mirrors (
    id          BIGSERIAL PRIMARY KEY,
    external_id VARCHAR(128) NOT NULL,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    minutes     INT NOT NULL DEFAULT 0,
    note        VARCHAR(512) NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE usage_mirrors IS '套餐/外部结算的虚拟用量镜像：只增展示行，不动余额（add-serving-plans D6）';
COMMENT ON COLUMN usage_mirrors.external_id IS '幂等键：上游结算单号，重复提交不再写第二行';
COMMENT ON COLUMN usage_mirrors.recorded_at IS '事件发生时刻（上游给的 recorded_at，缺省为服务端收到时刻）';

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_mirrors_external_id_unique
    ON usage_mirrors (external_id);

CREATE INDEX IF NOT EXISTS idx_usage_mirrors_user_time
    ON usage_mirrors (user_id, recorded_at DESC);
