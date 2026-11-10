-- 250: API key 容量契约（add-key-capacity-contract）
--
-- 模型：user 是池（复用既有 users.concurrency），key 是池内席位。
--   concurrency_request = 保底（额度保留，不保证饱和吞吐）
--   concurrency_limit   = 该 key 硬顶（0 = 不额外限制，可借支到池上限）
--   rpm_limit           = 每分钟请求上限（0 = 不限）
--   effective_at        = 生效时刻（NULL = 创建即生效）
--
-- 不变量（写路径事务内持锁校验，不在 DB 层强制——因为它是聚合约束，跨行）：
--   Σ concurrency_request（同一 user 的活跃 key） <= users.concurrency   （池 > 0 时）
--   concurrency_limit <= users.concurrency                               （池 > 0 且硬顶 > 0 时）
--   concurrency_request <= concurrency_limit                              （硬顶 > 0 时）
--
-- 不做数据回填：存量 key 全部 0/0/0/NULL，语义 = 「不占席位、可借支到池上限」，
-- 与本次改动前行为逐字一致。
--
-- 迁移编号说明：main 上现有最大号 248；249 由 harden/credential-at-rest 分支占用
-- （尚未合并），故本迁移用 250，给先落地的一方留出 249。

ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS concurrency_request integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS concurrency_limit   integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS rpm_limit           integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS effective_at        timestamptz NULL;

-- 非负约束：负值没有语义，且会让准入计算出现负数余量。
ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_concurrency_request_nonneg,
    ADD CONSTRAINT api_keys_concurrency_request_nonneg CHECK (concurrency_request >= 0);

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_concurrency_limit_nonneg,
    ADD CONSTRAINT api_keys_concurrency_limit_nonneg CHECK (concurrency_limit >= 0);

ALTER TABLE api_keys
    DROP CONSTRAINT IF EXISTS api_keys_rpm_limit_nonneg,
    ADD CONSTRAINT api_keys_rpm_limit_nonneg CHECK (rpm_limit >= 0);

-- 按 user 聚合「Σ活跃 key 的 request」是写路径的热查询，故给 (user_id, status) 一个部分索引。
-- 只索引未删除行：deleted_at 非空的行不参与额度计算。
CREATE INDEX IF NOT EXISTS idx_api_keys_user_active_capacity
    ON api_keys (user_id)
    WHERE deleted_at IS NULL;
