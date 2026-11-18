-- 阶段 5.2 收尾（二）：三个 cache 失效函数仍引用已下线的明文列。
--
-- 253 只修了 `enqueue_api_key_auth_cache_invalidation`（api_keys 触发器本体），
-- 184 里另外三个函数漏了，它们都在 SELECT 里引用 `k.key`：
--
--   enqueue_user_auth_cache_invalidation            （users 触发器调用）
--   enqueue_group_auth_cache_invalidation           （groups 触发器调用，193 覆盖过）
--   enqueue_allowed_group_auth_cache_invalidation   （user_allowed_groups 触发器调用）
--
-- 251 drop 掉 `api_keys.key` 之后，任何触发它们的路径都抛
--   `pq: column k.key does not exist`
-- 实测：`GroupRepository.Delete()` 直接失败，`TestGroupRepoSuite` 16 个用例红。
-- 与 253 同族——不是少写计数，是整笔事务回滚。
--
-- 修法与 253 一致：`key_hash` 列本身就是 `sha256(明文 key)` 的十六进制，
-- 直接取它，不再对明文做哈希（明文此时已不存在）。
-- 过滤条件 `k.key <> ''` 等价改为 `k.key_hash <> ''`（空串仍排除）。

-- ---------------------------------------------------------------- users
CREATE OR REPLACE FUNCTION enqueue_user_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_user_id BIGINT;
BEGIN
    target_user_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.role IS NOT DISTINCT FROM NEW.role
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT k.key_hash
    FROM api_keys AS k
    WHERE k.user_id = target_user_id
      AND k.deleted_at IS NULL
      AND k.key_hash IS NOT NULL
      AND k.key_hash <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

-- --------------------------------------------------------------- groups
-- 与 193 的函数体保持一致，只把 key 的取法换成 key_hash。
CREATE OR REPLACE FUNCTION enqueue_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_group_id BIGINT;
BEGIN
    target_group_id := OLD.id;
    IF TG_OP = 'UPDATE'
       AND OLD.status IS NOT DISTINCT FROM NEW.status
       AND OLD.is_exclusive IS NOT DISTINCT FROM NEW.is_exclusive
       AND OLD.allow_image_generation IS NOT DISTINCT FROM NEW.allow_image_generation
       AND OLD.platform IS NOT DISTINCT FROM NEW.platform
       AND OLD.subscription_type IS NOT DISTINCT FROM NEW.subscription_type
       AND OLD.rate_multiplier IS NOT DISTINCT FROM NEW.rate_multiplier
       AND OLD.peak_rate_enabled IS NOT DISTINCT FROM NEW.peak_rate_enabled
       AND OLD.peak_start IS NOT DISTINCT FROM NEW.peak_start
       AND OLD.peak_end IS NOT DISTINCT FROM NEW.peak_end
       AND OLD.peak_rate_multiplier IS NOT DISTINCT FROM NEW.peak_rate_multiplier
       AND OLD.profit_control_enabled IS NOT DISTINCT FROM NEW.profit_control_enabled
       AND OLD.profit_min_margin IS NOT DISTINCT FROM NEW.profit_min_margin
       AND OLD.profit_safety_buffer IS NOT DISTINCT FROM NEW.profit_safety_buffer
       AND OLD.deleted_at IS NOT DISTINCT FROM NEW.deleted_at THEN
        RETURN NEW;
    END IF;

    INSERT INTO auth_cache_invalidation_outbox (cache_key)
    SELECT k.key_hash
    FROM api_keys AS k
    WHERE k.group_id = target_group_id
      AND k.deleted_at IS NULL
      AND k.key_hash IS NOT NULL
      AND k.key_hash <> '';
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

-- ------------------------------------------------- user_allowed_groups
CREATE OR REPLACE FUNCTION enqueue_allowed_group_auth_cache_invalidation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_user_id BIGINT;
    target_group_id BIGINT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND (OLD.user_id IS DISTINCT FROM NEW.user_id
            OR OLD.group_id IS DISTINCT FROM NEW.group_id) THEN
        IF EXISTS (
            SELECT 1 FROM groups g
            WHERE g.id = OLD.group_id AND g.is_exclusive = TRUE
        ) THEN
            INSERT INTO auth_cache_invalidation_outbox (cache_key)
            SELECT k.key_hash
            FROM api_keys AS k
            WHERE k.user_id = OLD.user_id
              AND k.group_id = OLD.group_id
              AND k.deleted_at IS NULL
              AND k.key_hash IS NOT NULL
              AND k.key_hash <> '';
        END IF;
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSIF TG_OP = 'UPDATE' THEN
        RETURN NEW;
    ELSIF TG_OP = 'INSERT' THEN
        target_user_id := NEW.user_id;
        target_group_id := NEW.group_id;
    ELSE
        target_user_id := OLD.user_id;
        target_group_id := OLD.group_id;
    END IF;

    IF EXISTS (
        SELECT 1 FROM groups g
        WHERE g.id = target_group_id AND g.is_exclusive = TRUE
    ) THEN
        INSERT INTO auth_cache_invalidation_outbox (cache_key)
        SELECT k.key_hash
        FROM api_keys AS k
        WHERE k.user_id = target_user_id
          AND k.group_id = target_group_id
          AND k.deleted_at IS NULL
          AND k.key_hash IS NOT NULL
          AND k.key_hash <> '';
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
