-- 阶段 5.2 的收尾修复：cache 失效触发器不能继续引用已下线的明文列。
--
-- 背景：251 把 `api_keys.key` drop 掉了，但 `trg_api_keys_auth_cache_invalidation`
-- 调用的 `enqueue_api_key_auth_cache_invalidation()` 仍然写着 `OLD.key` / `NEW.key`。
-- 从 251 生效那一刻起，`api_keys` 上**任何** UPDATE 都会抛
--   `pq: record "old" has no field "key"`
-- 后果不是「少写了几个计数」而是整笔结算事务回滚：
--   * `quota_used` 永不累加 → key 配额永远无法耗尽 → key 级限额形同虚设；
--   * `last_used_at` / `usage_5h|1d|7d` 永不更新；
--   * `DELETE /api/v1/keys/:id` 直接 500（DELETE 分支同样走 OLD.key）。
-- 与 `openai_gateway_usage.go` 的错误分支配合，表现成「调用成功、`actual_cost` 被置 0」，
-- 也就是这笔钱既没进余额、也没进订阅——静默漏计费。
--
-- 修法要点：**不能继续调用 `enqueue_auth_cache_invalidation()`**。
-- 那个函数会对入参再做一次 `sha256()`，而 `key_hash` 列本身就是 `sha256(明文 key)`
-- （见 249 的列注释与 repository 侧 `apiKeyPlaintextFromCiphertext` 的口径），
-- 传进去会得到双重哈希，缓存永远失效不到点上。所以这里直接按 `key_hash` 写入 outbox。
--
-- 监听列集合与修复前保持一致（key_hash / status / deleted_at / user_id / group_id /
-- ip_whitelist / ip_blacklist / expires_at），只是把 key 换成 key_hash；
-- `usage_*`、`quota_used`、`last_used_at` 这些高频计数**刻意不在**其中——
-- 它们不影响鉴权，纳入会让每次计费都产生一条无用的失效投递。
--
-- 幂等：`CREATE OR REPLACE` 重复执行安全。

CREATE OR REPLACE FUNCTION public.enqueue_api_key_auth_cache_invalidation()
 RETURNS trigger
 LANGUAGE plpgsql
AS $function$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.key_hash IS NOT NULL AND OLD.key_hash <> '' THEN
            INSERT INTO auth_cache_invalidation_outbox (cache_key) VALUES (OLD.key_hash);
        END IF;
        RETURN OLD;
    END IF;

    IF OLD.key_hash IS DISTINCT FROM NEW.key_hash
       OR OLD.status IS DISTINCT FROM NEW.status
       OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
       OR OLD.user_id IS DISTINCT FROM NEW.user_id
       OR OLD.group_id IS DISTINCT FROM NEW.group_id
       OR OLD.ip_whitelist IS DISTINCT FROM NEW.ip_whitelist
       OR OLD.ip_blacklist IS DISTINCT FROM NEW.ip_blacklist
       OR OLD.expires_at IS DISTINCT FROM NEW.expires_at THEN
        IF OLD.key_hash IS NOT NULL AND OLD.key_hash <> '' THEN
            INSERT INTO auth_cache_invalidation_outbox (cache_key) VALUES (OLD.key_hash);
        END IF;
        IF NEW.deleted_at IS NULL
           AND NEW.key_hash IS DISTINCT FROM OLD.key_hash
           AND NEW.key_hash IS NOT NULL AND NEW.key_hash <> '' THEN
            INSERT INTO auth_cache_invalidation_outbox (cache_key) VALUES (NEW.key_hash);
        END IF;
    END IF;
    RETURN NEW;
END;
$function$;
