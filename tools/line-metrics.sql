-- 词元按线指标（只读）：花费 / 错误 / 延迟，一个别名表
--
-- 为什么是一个文件而不是三个：别名表（`lex`+`lexkg`、`wanxing`+`wx`…）只能有一份。
-- 拆成多个文件后，改了一处忘了另一处，同一条线在两个报表里就会是两个数——这是本仓
-- 反复出现的漂移形态。改口径只改这个文件，留下 diff。
--
-- 用法（只读，不写任何东西）：
--   kubectl --context cheap-ts -n sub2api exec -i sub2api-postgres-0 -- \
--     psql -U sub2api -d sub2api -v days=30 -f - < tools/line-metrics.sql
--
-- 口径声明（报表标题必须一起带上）：
--   **维度 = 计费主体（谁持 key 调用），不是受益方。**
--   `wanxing-lex-console-copilot` 的首段是「萬星在调」，但它替**识律**调。
--   按首段归因只会把花费记到萬星头上。要做受益方口径需要另一张键→受益方映射，
--   目前没有，也不靠猜。
--
-- 三条硬纪律：
--   1. 无法归因的键**单列 unknown**，不并入任何一条线（`特价` 占 26.9%，并入哪条都是编数字）。
--   2. 别名只允许「同一实体」的写法归一；不确定就留 unknown。
--   3. 花费同时给 `actual_cost`（实扣）与 `total_cost`（标价）；错误必须按状态码分列。
--
-- ── 已知噪声键（2026-11-10 实查，明确排除出错误率，**不删 key**）────────────────
--   `wx-wizard-verify`（api_keys id=53，group_id=3 `core`，2026-10-12 建）：
--   7 天 15,392 次 403（`No active subscription found for this group`），
--   占全系统错误行 50%，14 天每日 699～3,722 次，至今仍在打。
--   它挂的分组没有有效订阅，所以每次必 403——不是履约故障，是验证 key 在空转。
--   排除理由：不排除的话「错误率」看板看到的是这把 key 的心跳。
--   处置（禁用/换分组）属运营动作，本文件只做**统计排除**，不动数据。

\if :{?days}
\else
  \set days 30
\endif
\if :{?err_days}
\else
  \set err_days 7
\endif

\echo '══════ 1. 花费（按计费主体，近' :days '天）══════'
WITH usage AS (
    SELECT u.api_key_id,
           COALESCE(u.actual_cost, 0)::numeric AS actual_usd,
           COALESCE(u.total_cost, 0)::numeric  AS list_usd
    FROM usage_logs u
    WHERE u.created_at >= now() - make_interval(days => :days)
),
aliases(seg, line, why) AS (
    VALUES
        ('lex',      '识律 lex',       '主键名'),
        ('lexkg',    '识律 lex',       '同一产品的知识图谱键，2026-11-10 归并'),
        ('wanxing',  '萬星 wanxing',   '主键名'),
        ('wx',       '萬星 wanxing',   '萬星简写，2026-11-10 归并'),
        ('law',      'law-bench',      'law-bench 早期短名'),
        ('lawbench', 'law-bench',      'law-bench 无连字符写法'),
        ('yizuo',    '壹座 yizuo',     '平台自身承载的调用'),
        ('platform', '壹座 platform',  '平台内部/演示 key')
),
attributed AS (
    SELECT COALESCE(a.line, 'unknown') AS line,
           CASE WHEN a.line IS NULL THEN split_part(COALESCE(k.name, '(deleted key)'), '-', 1) END AS raw_seg,
           us.actual_usd, us.list_usd
    FROM usage us
    LEFT JOIN api_keys k ON k.id = us.api_key_id
    LEFT JOIN aliases  a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
)
SELECT line,
       COALESCE(raw_seg, '')                                  AS raw_prefix_if_unknown,
       count(*)                                               AS requests,
       round(sum(actual_usd), 2)                              AS actual_usd,
       round(sum(list_usd), 2)                                AS list_usd,
       round(100 * sum(actual_usd) / NULLIF((SELECT sum(actual_usd) FROM attributed), 0), 1) AS pct
FROM attributed
GROUP BY line, raw_seg
ORDER BY actual_usd DESC;

\echo ''
\echo '══════ 2. 错误（按计费主体 × 状态码，近' :err_days '天；已排除噪声键）══════'
WITH errs AS (
    SELECT e.api_key_id, e.status_code
    FROM ops_error_logs e
    JOIN api_keys k ON k.id = e.api_key_id
    WHERE e.created_at >= now() - make_interval(days => :err_days)
      AND k.name <> 'wx-wizard-verify'          -- 噪声键：见文件头，统计排除而非删数据
),
aliases(seg, line) AS (
    VALUES
        ('lex','识律 lex'), ('lexkg','识律 lex'),
        ('wanxing','萬星 wanxing'), ('wx','萬星 wanxing'),
        ('law','law-bench'), ('lawbench','law-bench'),
        ('yizuo','壹座 yizuo'), ('platform','壹座 platform')
),
tagged AS (
    SELECT COALESCE(a.line, 'unknown') AS line,
           CASE WHEN a.line IS NULL THEN split_part(COALESCE(k.name,'(deleted key)'), '-', 1) END AS raw_seg,
           er.status_code
    FROM errs er
    JOIN api_keys k ON k.id = er.api_key_id
    LEFT JOIN aliases a ON a.seg = split_part(COALESCE(k.name,''), '-', 1)
)
SELECT line,
       COALESCE(raw_seg, '')                                AS raw_prefix_if_unknown,
       count(*)                                             AS errors,
       count(*) FILTER (WHERE status_code = 403)            AS s403,
       count(*) FILTER (WHERE status_code = 429)            AS s429,
       count(*) FILTER (WHERE status_code BETWEEN 500 AND 599) AS s5xx
FROM tagged
GROUP BY line, raw_seg
ORDER BY errors DESC;

\echo ''
\echo '══════ 3. 延迟（按计费主体 × 前 2 模型，近' :days '天）══════'
WITH usage AS (
    SELECT u.api_key_id, lower(u.model) AS model_lower, u.duration_ms, u.actual_cost
    FROM usage_logs u
    WHERE u.created_at >= now() - make_interval(days => :days)
      AND u.duration_ms IS NOT NULL
),
aliases(seg, line) AS (
    VALUES
        ('lex','识律 lex'), ('lexkg','识律 lex'),
        ('wanxing','萬星 wanxing'), ('wx','萬星 wanxing'),
        ('law','law-bench'), ('lawbench','law-bench'),
        ('yizuo','壹座 yizuo'), ('platform','壹座 platform')
),
tagged AS (
    SELECT COALESCE(a.line, 'unknown') AS line, us.model_lower, us.duration_ms, us.actual_cost
    FROM usage us
    LEFT JOIN api_keys k ON k.id = us.api_key_id
    LEFT JOIN aliases  a ON a.seg = split_part(COALESCE(k.name,''), '-', 1)
),
agg AS (
    SELECT line, model_lower, count(*) reqs,
           round(sum(actual_cost)::numeric, 2) usd,
           round(avg(duration_ms)) avg_ms,
           round(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms)) p95_ms,
           row_number() OVER (PARTITION BY line ORDER BY count(*) DESC) rn
    FROM tagged
    GROUP BY line, model_lower
)
SELECT line, model_lower, reqs, usd, avg_ms, p95_ms
FROM agg WHERE rn <= 2 ORDER BY line, rn;

\echo ''
\echo '══════ 4. 花费异常（周环比 >=2 倍，或近 3 日均量 >=2 倍于前 7 日均量）══════'
-- 为什么需要这一节：168 小时前没人会注意一把叫 "probe" 的 key。
-- 两个信号都要，因为它们的灵敏度不同（2026-11-10 实测校准）：
--   · 周环比（7d vs 前 7d）稳，但对「只涨了 5 天」的斜坡不敏感——
--     wanxing-probe 日内从 76 涨到 2,293（30 倍），周环比只有 1.7 倍。
--   · 近 3 日均量 vs 前 7 日均量 抓斜坡（同例：1,204/日 vs 385/日 = 3.1 倍）。
-- 阈值取 2 倍而非 3 倍：实测 `特价` 周环比 2.8 倍（$115/周），是本轮最大的真实增量，
-- 3 倍门槛会把它漏掉。放宽后仍需 man 复核——本查询只负责「让人看见」，不负责判定异常性质。
WITH win AS (
    SELECT u.api_key_id,
           sum(COALESCE(u.actual_cost,0)) FILTER (WHERE u.created_at >= now() - interval '7 days')  AS cur_usd,
           count(*)                       FILTER (WHERE u.created_at >= now() - interval '7 days')  AS cur_reqs,
           sum(COALESCE(u.actual_cost,0)) FILTER (WHERE u.created_at >= now() - interval '14 days'
                                                    AND u.created_at <  now() - interval '7 days')  AS prev_usd,
           count(*)                       FILTER (WHERE u.created_at >= now() - interval '14 days'
                                                    AND u.created_at <  now() - interval '7 days')  AS prev_reqs,
           count(*)                       FILTER (WHERE u.created_at >= now() - interval '3 days')  AS last3_reqs,
           sum(COALESCE(u.actual_cost,0)) FILTER (WHERE u.created_at >= now() - interval '3 days')  AS last3_usd
    FROM usage_logs u
    WHERE u.created_at >= now() - interval '14 days'
    GROUP BY u.api_key_id
),
scored AS (
    SELECT w.*,
           COALESCE(w.cur_usd,0) / NULLIF(w.prev_usd,0)          AS usd_growth_x,
           COALESCE(w.cur_reqs,0) / NULLIF(w.prev_reqs,0)        AS req_growth_x,
           (COALESCE(w.last3_reqs,0) / 3.0)
             / NULLIF(COALESCE(w.prev_reqs,0) / 7.0, 0)          AS ramp_x
    FROM win w
)
SELECT k.name                                   AS key_name,
       k.group_id,
       COALESCE(s.prev_reqs,0)                  AS reqs_prev7,
       COALESCE(s.cur_reqs,0)                   AS reqs_cur7,
       COALESCE(s.last3_reqs,0)                 AS reqs_last3d,
       round(COALESCE(s.prev_usd,0)::numeric,2) AS usd_prev7,
       round(COALESCE(s.cur_usd,0)::numeric,2)  AS usd_cur7,
       round(s.usd_growth_x::numeric,1)         AS usd_x,
       round(s.ramp_x::numeric,1)               AS ramp_x
FROM scored s JOIN api_keys k ON k.id = s.api_key_id
WHERE COALESCE(s.cur_usd,0) >= 0.5
  AND (s.usd_growth_x >= 2 OR s.ramp_x >= 2
       OR (s.prev_usd IS NULL AND COALESCE(s.cur_usd,0) >= 1))
ORDER BY COALESCE(s.cur_usd,0) DESC;
