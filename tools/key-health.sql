-- 词元按 key 健康（只读）：成功率 / p95 / 限流占比 / 活跃时段
--
-- 回答运维最常问的一句：「这把 key 怎么样？」
--
-- 用法（只读，不写任何东西）：
--   kubectl --context cheap -n sub2api exec -i sub2api-postgres-0 -- \
--     psql -U sub2api -d sub2api -v days=7 -f - < tools/key-health.sql
--
-- 口径与 /api/v1/keys/{id}/stability 端点**同源**（add-key-capacity-contract 5.3），
-- 三条必须同时成立，否则报表与端点会各说各话：
--   1. 成功率分母 = 成功 + 失败。只用 usage_logs 会恒得 100%（那里只有成功计费行）。
--   2. p95 只用成功请求。失败的 duration 反映的是「失败得快」，混进来会把 p95 拉低，
--      看起来更健康——典型的自证陷阱。
--   3. 无数据给 NULL，不给 0 也不给 1。「没调用过」不等于「很稳定」。
--
-- 为什么失败数与 429 取自 ops_error_logs 而不是 usage_logs：
--   usage_logs 只登记成功计费行；429/403 这类拒绝根本不会产生用量行。
--   2026-11-26 之前容量拒绝也不入 ops_error_logs，lookback 会看到 throttle=0 ——
--   那正是「最该看的一类永远看不到」，已在同批修掉（LoggableIngressReject）。

\if :{?days}
\else
  \set days 7
\endif
\if :{?min_calls}
\else
  \set min_calls 1
\endif

\echo '══════ 1. 按 key 健康总表（近' :days '天，调用≥' :min_calls '次）══════'
WITH succ AS (
    SELECT api_key_id,
           count(*)                                        AS ok_n,
           percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95_ms,
           max(created_at)                                 AS last_ok_at
    FROM usage_logs
    WHERE created_at > now() - make_interval(days => :days)
    GROUP BY api_key_id
),
fail AS (
    SELECT api_key_id,
           count(*)                                     AS err_n,
           count(*) FILTER (WHERE status_code = 429)    AS throttle_n
    FROM ops_error_logs
    WHERE created_at > now() - make_interval(days => :days)
      AND api_key_id IS NOT NULL
    GROUP BY api_key_id
)
SELECT k.id                                                       AS key_id,
       left(coalesce(k.name, ''), 28)                              AS name,
       k.user_id,
       coalesce(s.ok_n, 0)                                         AS ok,
       coalesce(f.err_n, 0)                                        AS err,
       coalesce(f.throttle_n, 0)                                   AS throttled,
       -- 分母含失败；total=0 时整列为 NULL（不是 0、不是 1）
       CASE WHEN coalesce(s.ok_n,0) + coalesce(f.err_n,0) = 0 THEN NULL
            ELSE round(coalesce(s.ok_n,0)::numeric
                       / (coalesce(s.ok_n,0) + coalesce(f.err_n,0)), 4)
       END                                                         AS success_ratio,
       CASE WHEN coalesce(s.ok_n,0) + coalesce(f.err_n,0) = 0 THEN NULL
            ELSE round(coalesce(f.throttle_n,0)::numeric
                       / (coalesce(s.ok_n,0) + coalesce(f.err_n,0)), 4)
       END                                                         AS throttle_ratio,
       round(s.p95_ms::numeric, 1)                                 AS p95_ms,
       s.last_ok_at
FROM api_keys k
LEFT JOIN succ s ON s.api_key_id = k.id
LEFT JOIN fail f ON f.api_key_id = k.id
WHERE k.deleted_at IS NULL
  AND coalesce(s.ok_n,0) + coalesce(f.err_n,0) >= :min_calls
ORDER BY coalesce(s.ok_n,0) + coalesce(f.err_n,0) DESC;

\echo '══════ 2. 活跃时段（UTC 小时，近' :days '天，只看有调用的 key）══════'
WITH hours AS (
    SELECT api_key_id,
           extract(hour FROM created_at AT TIME ZONE 'UTC')::int AS h
    FROM usage_logs
    WHERE created_at > now() - make_interval(days => :days)
    GROUP BY api_key_id, h
)
SELECT h.api_key_id,
       left(coalesce(k.name, ''), 28)                    AS name,
       string_agg(h.h::text, ',' ORDER BY h.h)           AS active_hours_utc,
       count(*)                                          AS distinct_hours
FROM hours h
JOIN api_keys k ON k.id = h.api_key_id AND k.deleted_at IS NULL
GROUP BY h.api_key_id, k.name
ORDER BY count(*) DESC;

\echo '══════ 3. 被限流最多的 key（近' :days '天）══════'
SELECT e.api_key_id,
       left(coalesce(k.name, ''), 28) AS name,
       count(*)                       AS n_429,
       count(DISTINCT e.request_path) AS distinct_paths,
       min(e.created_at)              AS first_seen,
       max(e.created_at)              AS last_seen
FROM ops_error_logs e
LEFT JOIN api_keys k ON k.id = e.api_key_id
WHERE e.created_at > now() - make_interval(days => :days)
  AND e.status_code = 429
  AND e.api_key_id IS NOT NULL
GROUP BY e.api_key_id, k.name
ORDER BY n_429 DESC
LIMIT 20;

\echo '══════ 4. 口径自检：有失败的 key 成功率必须 < 1 ══════'
-- 没有这一行就说明口径坏了：成功数与失败数至少一边没取到，或分母漏了失败。
-- 判据只用 **err > 0 的行** 求 max(success_ratio)：那样才等价于「有失败的 key 里，
-- 有没有哪个被算成了 100%」。不这么写的话 CASE/END 与 FULL OUTER JOIN 的 NULL 行
-- 会把两侧都计入分母，报出假的 wrongly_perfect（2026-11-26 首版就踩了这个）。
WITH succ AS (
    SELECT api_key_id, count(*) AS ok_n
    FROM usage_logs
    WHERE created_at > now() - make_interval(days => :days)
    GROUP BY api_key_id
),
fail AS (
    SELECT api_key_id, count(*) AS err_n
    FROM ops_error_logs
    WHERE created_at > now() - make_interval(days => :days) AND api_key_id IS NOT NULL
    GROUP BY api_key_id
),
joined AS (
    SELECT coalesce(s.api_key_id, f.api_key_id) AS key_id,
           coalesce(s.ok_n, 0)                  AS ok,
           coalesce(f.err_n, 0)                 AS err
    FROM succ s
    FULL OUTER JOIN fail f ON f.api_key_id = s.api_key_id
)
SELECT count(*) FILTER (WHERE err > 0)                       AS keys_with_failures,
       max(ok::numeric / nullif(ok + err, 0)) FILTER (WHERE err > 0) AS max_ratio_among_failing,
       'max_ratio_among_failing 必须 < 1（=1 说明分母漏了失败）'      AS note
FROM joined
WHERE ok + err > 0;
\echo '══════ 5. key 级 RPM 的 fail-open 可见性（2.3）══════'
-- fail-open 本身是对的：计数后端（Redis）故障时放行，不让限流器把全站变成 429。
-- 但**静默失效比故障更危险**——限流一旦不生效，外部只会看到"额度突然不管用了"。
--
-- 口径与限制（必须一起读）：
--   · 计数在**日志**里，不在 DB：`Warning: rpm increment (key=%d) failed for user=%d`
--     （internal/service/billing_cache_service.go）。所以本段查不到任何东西**不代表没发生**，
--     只代表 `kubectl logs` 里没有这一行。
--   · 该日志写在 pod stdout，容器重启即失。要长期曲线必须先把日志收进聚合器；
--     我们没有，故这里只给「当前 pod 生命周期内」的读法。
--   · 改那条日志的文案会让本段失效——改前先同步本文件。
\echo '  查询命令（本段故意不写 SQL，因为它不在 DB 里）：'
\echo '    kubectl --context cheap -n sub2api logs -l app=sub2api --since=24h | grep -c "rpm increment (key="'
\echo '  判读：> 0 说明有过 fail-open（限流静默失效）；配合 --since 看首次出现时间定位 Redis 故障窗口。'
