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
-- >>> generated:line-aliases（真源 tools/line-aliases.json；改它，别改这里）
aliases(seg, line, why) AS (
    VALUES
        ('lex',        '识律 lex',      '主键名'),
        ('lexkg',      '识律 lex',      '同一产品的知识图谱键，2026-11-10 归并'),
        ('wanxing',    '萬星 wanxing',  '主键名'),
        ('wx',         '萬星 wanxing',  '萬星简写，2026-11-10 归并'),
        ('law',        'law-bench',     'law-bench 早期短名'),
        ('lawbench',   'law-bench',     'law-bench 无连字符写法'),
        ('yizuo',      '壹座 yizuo',    '平台自身承载的调用'),
        ('platform',   '壹座 platform', '平台内部/演示 key'),
        ('特价',       '自有平台',      '路由命名的共享键（折扣渠道），不是产品线；user_id=1 的自有账号，消费方是 dsh harness，键名不含线信息 ⇒ 单列（证据见 openspec ciyuan-consumer-surface 5.5-1）；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测 7 日内部构成：deepseek-harness ~$99（壹座 dsh，经入口节点桥 10.<internal>.4/.9/.10 轮转——三桥花费几乎相等，是同一调用方不是三个）+ AsyncOpenAI/Python 单一内网主机 10.<internal>.1 ~$46（与 wanxing-probe- 前缀规则同一台机，属非产品流量却计入本桶 ⇒ 「自有平台」略被高估）+ 其余 ~$3'),
        ('openrouter', '自有平台',      '同上：上游路由命名的共享键，不是产品线，消费方同为 dsh harness。注意与模型 id `openrouter/free` 同词不同物——本字段只匹配 key 名首段；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测：本键 99.6% 是 deepseek-harness（~$90，入口桥 10.<internal>.12.0→12.1 于 11-14 切换）'),
        ('vaas',       'VAAS',          'VAAS 是一条产品线（有独立产线镜像与集群车道），它的键全部以 `vaas-` 起首、挂在该产品的专属账号下 ⇒ 整族就是这一条线，不需要逐条细则。2026-11-16 实读 7 日：`vaas-media-grok` $0.8100（federation@example.com）、`vaas-wanxing-imagevideo` $0.1340 / `vaas-wanxing-voice` $0.0135（admin@example.com），同族另有 `vaas-tts` / `vaas-rerank` / `vaas-image-video` 三键同属 vaas@example.invalid（本期无用量）。此前整族落 unknown，约占 unknown 桶的三成')
),
prefix_aliases(prefix, line, why) AS (
    VALUES
        ('wanxing-probe-', '非产品流量',         '键名自称 probe，实测是从单一内网主机 10.<internal>.1 连续跑了 168/168 小时的 harness 工作负载（7 日 7,674 次、只打 deepseek-v4.1-flash、均值 9,672 tok / 26.7 s、缓存读 17.3k）⇒ 可以确定不是萬星的产品用量；但它替谁跑，数据里看不出来，故单列而不并入任何产品线'),
        ('fd-text-core-',  '文本核心 text-core', '`text-core` 是已声明的产品线（`all-services/prod/line-manifests.yaml` 的 19 条之一），而这条线的调用键名就叫 `fd-text-core-*`——首段 `fd` 是组织前缀，导致这条线在自己的报表里看不到自己的调用。2026-11-16 实读：`fd-text-core-embedding-bulk` 7 日 $0.4449 / 2,783 次，全部是 embedding 批量调用，属文本核心的检索/向量面'),
        ('wanxing-lex-',   '识律 lex',           '识律控制台副驾：key 挂在萬星账号名下（首段 wanxing），实际调用方是识律控制台——2026-11-15 实测 UA 为 deepseek-harness、来源 pod 10.<internal>.8.0，7 日 2,346 次 / $7.81，而识律自己的 lex 键同期只有 1 次请求 ⇒ 受益方是识律。不改名，因为改名只活在单个数据库、换环境即失效')
),
resolved(key_id, line, why, by_rule, rule, raw_seg) AS (
    SELECT k.id,
           COALESCE(p.line, a.line, 'unknown'),
           COALESCE(p.why, a.why, ''),
           CASE WHEN p.line IS NOT NULL THEN 'prefix'
                WHEN a.line IS NOT NULL THEN 'seg'
                ELSE 'none' END,
           COALESCE(p.prefix, a.seg),
           CASE WHEN p.line IS NULL AND a.line IS NULL
                THEN NULLIF(split_part(COALESCE(k.name, ''), '-', 1), '')
           END
    FROM api_keys k
    LEFT JOIN LATERAL (
        SELECT pa.prefix, pa.line, pa.why
        FROM prefix_aliases pa
        WHERE starts_with(COALESCE(k.name, ''), pa.prefix)
        ORDER BY length(pa.prefix) DESC
        LIMIT 1
    ) p ON true
    LEFT JOIN aliases a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
),
route_segs(seg) AS (
    VALUES
        ('特价'      ),
        ('openrouter')
),
-- <<< generated:line-aliases
-- 归属走生成的 `resolved`（首段 / 声明前缀 / 未归因都在那一处算），本文件不再自己 split_part。
attributed AS (
    SELECT COALESCE(r.line, 'unknown') AS line,
           COALESCE(r.by_rule, 'none') AS by_rule,
           COALESCE(r.rule, '')        AS rule,
           CASE WHEN r.line IS NULL OR r.line = 'unknown'
                THEN COALESCE(r.raw_seg, '(deleted key)') END AS raw_seg,
           us.actual_usd, us.list_usd
    FROM usage us
    LEFT JOIN resolved r ON r.key_id = us.api_key_id
)
SELECT line,
       by_rule,
       COALESCE(raw_seg, rule)                                AS attributed_by,
       count(*)                                               AS requests,
       round(sum(actual_usd), 2)                              AS actual_usd,
       round(sum(list_usd), 2)                                AS list_usd,
       round(100 * sum(actual_usd) / NULLIF((SELECT sum(actual_usd) FROM attributed), 0), 1) AS pct
FROM attributed
GROUP BY line, by_rule, COALESCE(raw_seg, rule)
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
-- >>> generated:line-aliases（真源 tools/line-aliases.json；改它，别改这里）
aliases(seg, line, why) AS (
    VALUES
        ('lex',        '识律 lex',      '主键名'),
        ('lexkg',      '识律 lex',      '同一产品的知识图谱键，2026-11-10 归并'),
        ('wanxing',    '萬星 wanxing',  '主键名'),
        ('wx',         '萬星 wanxing',  '萬星简写，2026-11-10 归并'),
        ('law',        'law-bench',     'law-bench 早期短名'),
        ('lawbench',   'law-bench',     'law-bench 无连字符写法'),
        ('yizuo',      '壹座 yizuo',    '平台自身承载的调用'),
        ('platform',   '壹座 platform', '平台内部/演示 key'),
        ('特价',       '自有平台',      '路由命名的共享键（折扣渠道），不是产品线；user_id=1 的自有账号，消费方是 dsh harness，键名不含线信息 ⇒ 单列（证据见 openspec ciyuan-consumer-surface 5.5-1）；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测 7 日内部构成：deepseek-harness ~$99（壹座 dsh，经入口节点桥 10.<internal>.4/.9/.10 轮转——三桥花费几乎相等，是同一调用方不是三个）+ AsyncOpenAI/Python 单一内网主机 10.<internal>.1 ~$46（与 wanxing-probe- 前缀规则同一台机，属非产品流量却计入本桶 ⇒ 「自有平台」略被高估）+ 其余 ~$3'),
        ('openrouter', '自有平台',      '同上：上游路由命名的共享键，不是产品线，消费方同为 dsh harness。注意与模型 id `openrouter/free` 同词不同物——本字段只匹配 key 名首段；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测：本键 99.6% 是 deepseek-harness（~$90，入口桥 10.<internal>.12.0→12.1 于 11-14 切换）'),
        ('vaas',       'VAAS',          'VAAS 是一条产品线（有独立产线镜像与集群车道），它的键全部以 `vaas-` 起首、挂在该产品的专属账号下 ⇒ 整族就是这一条线，不需要逐条细则。2026-11-16 实读 7 日：`vaas-media-grok` $0.8100（federation@example.com）、`vaas-wanxing-imagevideo` $0.1340 / `vaas-wanxing-voice` $0.0135（admin@example.com），同族另有 `vaas-tts` / `vaas-rerank` / `vaas-image-video` 三键同属 vaas@example.invalid（本期无用量）。此前整族落 unknown，约占 unknown 桶的三成')
),
prefix_aliases(prefix, line, why) AS (
    VALUES
        ('wanxing-probe-', '非产品流量',         '键名自称 probe，实测是从单一内网主机 10.<internal>.1 连续跑了 168/168 小时的 harness 工作负载（7 日 7,674 次、只打 deepseek-v4.1-flash、均值 9,672 tok / 26.7 s、缓存读 17.3k）⇒ 可以确定不是萬星的产品用量；但它替谁跑，数据里看不出来，故单列而不并入任何产品线'),
        ('fd-text-core-',  '文本核心 text-core', '`text-core` 是已声明的产品线（`all-services/prod/line-manifests.yaml` 的 19 条之一），而这条线的调用键名就叫 `fd-text-core-*`——首段 `fd` 是组织前缀，导致这条线在自己的报表里看不到自己的调用。2026-11-16 实读：`fd-text-core-embedding-bulk` 7 日 $0.4449 / 2,783 次，全部是 embedding 批量调用，属文本核心的检索/向量面'),
        ('wanxing-lex-',   '识律 lex',           '识律控制台副驾：key 挂在萬星账号名下（首段 wanxing），实际调用方是识律控制台——2026-11-15 实测 UA 为 deepseek-harness、来源 pod 10.<internal>.8.0，7 日 2,346 次 / $7.81，而识律自己的 lex 键同期只有 1 次请求 ⇒ 受益方是识律。不改名，因为改名只活在单个数据库、换环境即失效')
),
resolved(key_id, line, why, by_rule, rule, raw_seg) AS (
    SELECT k.id,
           COALESCE(p.line, a.line, 'unknown'),
           COALESCE(p.why, a.why, ''),
           CASE WHEN p.line IS NOT NULL THEN 'prefix'
                WHEN a.line IS NOT NULL THEN 'seg'
                ELSE 'none' END,
           COALESCE(p.prefix, a.seg),
           CASE WHEN p.line IS NULL AND a.line IS NULL
                THEN NULLIF(split_part(COALESCE(k.name, ''), '-', 1), '')
           END
    FROM api_keys k
    LEFT JOIN LATERAL (
        SELECT pa.prefix, pa.line, pa.why
        FROM prefix_aliases pa
        WHERE starts_with(COALESCE(k.name, ''), pa.prefix)
        ORDER BY length(pa.prefix) DESC
        LIMIT 1
    ) p ON true
    LEFT JOIN aliases a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
),
route_segs(seg) AS (
    VALUES
        ('特价'      ),
        ('openrouter')
),
-- <<< generated:line-aliases
tagged AS (
    SELECT COALESCE(r.line, 'unknown') AS line,
           COALESCE(r.by_rule, 'none') AS by_rule,
           COALESCE(r.rule, '')        AS rule,
           CASE WHEN r.line IS NULL OR r.line = 'unknown'
                THEN COALESCE(r.raw_seg, '(deleted key)') END AS raw_seg,
           er.status_code
    FROM errs er
    LEFT JOIN resolved r ON r.key_id = er.api_key_id
)
SELECT line,
       by_rule,
       COALESCE(raw_seg, rule)                              AS attributed_by,
       count(*)                                             AS errors,
       count(*) FILTER (WHERE status_code = 403)            AS s403,
       count(*) FILTER (WHERE status_code = 429)            AS s429,
       count(*) FILTER (WHERE status_code BETWEEN 500 AND 599) AS s5xx
FROM tagged
GROUP BY line, by_rule, COALESCE(raw_seg, rule)
ORDER BY errors DESC;

\echo ''
\echo '══════ 3. 延迟（按计费主体 × 前 2 模型，近' :days '天）══════'
WITH usage AS (
    SELECT u.api_key_id, lower(u.model) AS model_lower, u.duration_ms, u.actual_cost
    FROM usage_logs u
    WHERE u.created_at >= now() - make_interval(days => :days)
      AND u.duration_ms IS NOT NULL
),
-- >>> generated:line-aliases（真源 tools/line-aliases.json；改它，别改这里）
aliases(seg, line, why) AS (
    VALUES
        ('lex',        '识律 lex',      '主键名'),
        ('lexkg',      '识律 lex',      '同一产品的知识图谱键，2026-11-10 归并'),
        ('wanxing',    '萬星 wanxing',  '主键名'),
        ('wx',         '萬星 wanxing',  '萬星简写，2026-11-10 归并'),
        ('law',        'law-bench',     'law-bench 早期短名'),
        ('lawbench',   'law-bench',     'law-bench 无连字符写法'),
        ('yizuo',      '壹座 yizuo',    '平台自身承载的调用'),
        ('platform',   '壹座 platform', '平台内部/演示 key'),
        ('特价',       '自有平台',      '路由命名的共享键（折扣渠道），不是产品线；user_id=1 的自有账号，消费方是 dsh harness，键名不含线信息 ⇒ 单列（证据见 openspec ciyuan-consumer-surface 5.5-1）；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测 7 日内部构成：deepseek-harness ~$99（壹座 dsh，经入口节点桥 10.<internal>.4/.9/.10 轮转——三桥花费几乎相等，是同一调用方不是三个）+ AsyncOpenAI/Python 单一内网主机 10.<internal>.1 ~$46（与 wanxing-probe- 前缀规则同一台机，属非产品流量却计入本桶 ⇒ 「自有平台」略被高估）+ 其余 ~$3'),
        ('openrouter', '自有平台',      '同上：上游路由命名的共享键，不是产品线，消费方同为 dsh harness。注意与模型 id `openrouter/free` 同词不同物——本字段只匹配 key 名首段；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测：本键 99.6% 是 deepseek-harness（~$90，入口桥 10.<internal>.12.0→12.1 于 11-14 切换）'),
        ('vaas',       'VAAS',          'VAAS 是一条产品线（有独立产线镜像与集群车道），它的键全部以 `vaas-` 起首、挂在该产品的专属账号下 ⇒ 整族就是这一条线，不需要逐条细则。2026-11-16 实读 7 日：`vaas-media-grok` $0.8100（federation@example.com）、`vaas-wanxing-imagevideo` $0.1340 / `vaas-wanxing-voice` $0.0135（admin@example.com），同族另有 `vaas-tts` / `vaas-rerank` / `vaas-image-video` 三键同属 vaas@example.invalid（本期无用量）。此前整族落 unknown，约占 unknown 桶的三成')
),
prefix_aliases(prefix, line, why) AS (
    VALUES
        ('wanxing-probe-', '非产品流量',         '键名自称 probe，实测是从单一内网主机 10.<internal>.1 连续跑了 168/168 小时的 harness 工作负载（7 日 7,674 次、只打 deepseek-v4.1-flash、均值 9,672 tok / 26.7 s、缓存读 17.3k）⇒ 可以确定不是萬星的产品用量；但它替谁跑，数据里看不出来，故单列而不并入任何产品线'),
        ('fd-text-core-',  '文本核心 text-core', '`text-core` 是已声明的产品线（`all-services/prod/line-manifests.yaml` 的 19 条之一），而这条线的调用键名就叫 `fd-text-core-*`——首段 `fd` 是组织前缀，导致这条线在自己的报表里看不到自己的调用。2026-11-16 实读：`fd-text-core-embedding-bulk` 7 日 $0.4449 / 2,783 次，全部是 embedding 批量调用，属文本核心的检索/向量面'),
        ('wanxing-lex-',   '识律 lex',           '识律控制台副驾：key 挂在萬星账号名下（首段 wanxing），实际调用方是识律控制台——2026-11-15 实测 UA 为 deepseek-harness、来源 pod 10.<internal>.8.0，7 日 2,346 次 / $7.81，而识律自己的 lex 键同期只有 1 次请求 ⇒ 受益方是识律。不改名，因为改名只活在单个数据库、换环境即失效')
),
resolved(key_id, line, why, by_rule, rule, raw_seg) AS (
    SELECT k.id,
           COALESCE(p.line, a.line, 'unknown'),
           COALESCE(p.why, a.why, ''),
           CASE WHEN p.line IS NOT NULL THEN 'prefix'
                WHEN a.line IS NOT NULL THEN 'seg'
                ELSE 'none' END,
           COALESCE(p.prefix, a.seg),
           CASE WHEN p.line IS NULL AND a.line IS NULL
                THEN NULLIF(split_part(COALESCE(k.name, ''), '-', 1), '')
           END
    FROM api_keys k
    LEFT JOIN LATERAL (
        SELECT pa.prefix, pa.line, pa.why
        FROM prefix_aliases pa
        WHERE starts_with(COALESCE(k.name, ''), pa.prefix)
        ORDER BY length(pa.prefix) DESC
        LIMIT 1
    ) p ON true
    LEFT JOIN aliases a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
),
route_segs(seg) AS (
    VALUES
        ('特价'      ),
        ('openrouter')
),
-- <<< generated:line-aliases
tagged AS (
    SELECT COALESCE(r.line, 'unknown') AS line, us.model_lower, us.duration_ms, us.actual_cost
    FROM usage us
    LEFT JOIN resolved r ON r.key_id = us.api_key_id
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

-- ═══════════════════════════════════════════════════════════════════════════
-- 任务车道（add-ciyuan-task-lane 5.4）
--
-- 要能回答三个问题：①多少任务被推迟、省了多少钱 ②多少任务死在 deadline
-- ③排队是不是在堆积（pending 峰值 / 等待时长）
--
-- 口径纪律：
--   - 节省额 = 实扣 vs 「如果不推迟、按提交时刻峰价跑」的估价差。
--     估价来自 estimated_cost_usd（提交时按峰谷因子算的经验估价），
--     因此本查询的节省是**估价口径**，不是财务口径 —— 别当账单用。
--   - 「死在 deadline」只认 reason='deadline_exceeded' 前缀，
--     那样能把「重排时预判超期」与「到点淘汰」一并算进来。
-- ═══════════════════════════════════════════════════════════════════════════

-- ① 车道总览（近 N 天）
SELECT status,
       count(*)                                        AS tasks,
       count(*) FILTER (WHERE deferred_until IS NOT NULL) AS deferred_tasks,
       round(sum(estimated_cost_usd)::numeric, 4)      AS est_usd_sum,
       round(sum(actual_cost_usd)::numeric, 4)         AS actual_usd_sum,
       round(avg(attempts)::numeric, 2)                AS avg_attempts
FROM lanes_task_jobs
WHERE created_at >= now() - interval '7 days'
GROUP BY status
ORDER BY tasks DESC;

-- ② 推迟的收益（估价口径）：被推迟且成功的任务，实扣 vs 峰价估价
SELECT count(*)                                                   AS deferred_completed,
       round(sum(estimated_cost_usd)::numeric, 4)                 AS est_at_peak_usd,
       round(sum(actual_cost_usd)::numeric, 4)                    AS actual_usd,
       round(sum(estimated_cost_usd - actual_cost_usd)::numeric, 4) AS est_saving_usd,
       round((1 - sum(actual_cost_usd) / NULLIF(sum(estimated_cost_usd),0))::numeric, 3) AS saving_ratio
FROM lanes_task_jobs
WHERE deferred_until IS NOT NULL
  AND status = 'completed'
  AND created_at >= now() - interval '7 days';

-- ③ 死在 deadline 的任务（含原因分布）
SELECT COALESCE(split_part(reason, ':', 1), '(none)') AS reason_class,
       count(*)                                       AS tasks,
       round(avg(attempts)::numeric, 1)               AS avg_attempts
FROM lanes_task_jobs
WHERE status = 'failed'
  AND created_at >= now() - interval '7 days'
GROUP BY 1
ORDER BY tasks DESC;

-- ④ 排队健康度：pending 是否在堆积 + 等待时长
--    I3 的日常自证：第一行必须恒为 0（pending 且 deadline 已过）。
SELECT count(*) FILTER (WHERE deadline_at IS NOT NULL AND deadline_at < now()) AS pending_past_deadline_MUST_BE_ZERO,
       count(*)                                                               AS pending_now,
       max(now() - created_at)                                                AS oldest_pending_age,
       max(deferred_until)                                                    AS latest_deferred_until
FROM lanes_task_jobs
WHERE status = 'pending';

-- ⑤ 每类执行健康度（按 model）：成功率与 p95 耗时
SELECT j.model,
       count(*)                                                        AS attempts,
       count(*) FILTER (WHERE a.outcome = 'completed')                 AS completed,
       count(*) FILTER (WHERE a.outcome = 'retry_queue_full')          AS retried_queue_full,
       count(*) FILTER (WHERE a.outcome = 'retry_transient')           AS retried_transient,
       count(*) FILTER (WHERE a.outcome = 'failed_client')             AS failed_client,
       round(100.0 * count(*) FILTER (WHERE a.outcome = 'completed') / count(*), 1) AS success_pct,
       round(percentile_cont(0.95) WITHIN GROUP (
           ORDER BY extract(epoch FROM (a.finished_at - a.started_at)))::numeric, 1) AS p95_seconds
FROM lanes_task_attempts a
JOIN lanes_task_jobs j ON j.id = a.job_id
WHERE a.finished_at >= now() - interval '7 days'
GROUP BY j.model
ORDER BY attempts DESC;

-- ⑥ 血缘抽查：任务行能否追到账单行
--    usage_logs.request_id 形如 'client:task-<job>-<attempt>'，按尝试号唯一。
--    关联口径：优先按 attempts 表（每次执行一行，最精确）；
--    attempts 缺失时回退到 j.last_request_id（历史行与 v0.2.19-6 之前的 mode=now 属于这类，
--    它们**确实开票了**，若只看 attempts 会误报「未开票」—— 实测踩过）。
SELECT j.id AS job_id, j.status,
       a.attempt_no, a.outcome,
       COALESCE(a.request_id, j.last_request_id) AS request_id,
       (a.request_id IS NULL)                    AS lineage_from_job_row,
       u.id AS usage_log_id, u.actual_cost,
       (u.id IS NOT NULL) AS billed
FROM lanes_task_jobs j
LEFT JOIN lanes_task_attempts a ON a.job_id = j.id AND a.outcome = 'completed'
LEFT JOIN usage_logs u
       ON u.request_id = 'client:' || COALESCE(a.request_id, j.last_request_id)
      AND u.api_key_id = j.api_key_id
WHERE j.created_at >= now() - interval '2 days'
ORDER BY j.id DESC
LIMIT 50;
