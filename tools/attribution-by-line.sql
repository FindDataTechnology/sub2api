-- 词元按线归因（只读）——30 天花费/请求数，按「线」聚合
--
-- 为什么需要这个文件（而不是临时敲 SQL）：
--   能力快照解决了「能不能服务」，但没有回答「每条线花了多少」。临时 SQL 每个人写一遍，
--   别名归一会各写各的，于是同一条线的花费在两次报表里能差一倍（实测：识律分裂成
--   `lex` 与 `lexkg`，萬星分裂成 `wanxing` 与 `wx`）。把口径钉在这一个文件里，谁跑都得到
--   同一个数；改口径必须改这个文件，留下 diff。
--
-- 用法（只读，不写任何东西）：
--   kubectl --context cheap-ts -n sub2api exec -i sub2api-postgres-0 -- \
--     psql -U sub2api -d sub2api -v days=30 -f - < tools/attribution-by-line.sql
--
-- 口径声明（报表标题必须一起带上，否则会误导）：
--   **维度 = 计费主体（谁持 key 调用），不是受益方。**
--   键名同时编码了两件事：`wanxing-lex-console-copilot` 的首段是「萬星在调」，
--   但它是替**识律**调的。按首段归因只会把花费记到萬星头上。若要按受益方，
--   需要另一张键→受益方的映射（目前没有，也不该靠猜）。
--
-- 三条硬纪律：
--   1. 无法归因的键**单列**，不并入任何一条线（`特价` 占 26.9%，并入哪条都是编数字）。
--   2. 别名映射只允许「同一实体」的写法归一；不确定就留 unknown，别猜。
--   3. `actual_cost` 是实扣，`total_cost` 是标价；两者都给，别只给一个。

\if :{?days}
\else
  \set days 30
\endif

WITH usage AS (
    SELECT u.api_key_id,
           u.model,
           COALESCE(u.actual_cost, 0)::numeric AS actual_usd,
           COALESCE(u.total_cost, 0)::numeric  AS list_usd
    FROM usage_logs u
    WHERE u.created_at >= now() - make_interval(days => :days)
),
-- ── 别名映射（唯一的归一化真相；新增线在这里加一行，并写清「为什么是同一实体」）──
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
        ('特价',       '自有平台',      '路由命名的共享键（折扣渠道），不是产品线；user_id=1 的自有账号，消费方是 dsh harness，键名不含线信息 ⇒ 单列（证据见 openspec ciyuan-consumer-surface 5.5-1）；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性'),
        ('openrouter', '自有平台',      '同上：上游路由命名的共享键，不是产品线，消费方同为 dsh harness。注意与模型 id `openrouter/free` 同词不同物——本字段只匹配 key 名首段；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性')
),
prefix_aliases(prefix, line, why) AS (
    VALUES
        ('wanxing-probe-', '非产品流量', '键名自称 probe，实测是从单一内网主机 10.<internal>.1 连续跑了 168/168 小时的 harness 工作负载（7 日 7,674 次、只打 deepseek-v4.1-flash、均值 9,672 tok / 26.7 s、缓存读 17.3k）⇒ 可以确定不是萬星的产品用量；但它替谁跑，数据里看不出来，故单列而不并入任何产品线'),
        ('wanxing-lex-',   '识律 lex',   '识律控制台副驾：key 挂在萬星账号名下（首段 wanxing），实际调用方是识律控制台——2026-11-15 实测 UA 为 deepseek-harness、来源 pod 10.<internal>.8.0，7 日 2,346 次 / $7.81，而识律自己的 lex 键同期只有 1 次请求 ⇒ 受益方是识律。不改名，因为改名只活在单个数据库、换环境即失效')
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
-- 归属走生成的 `resolved`（首段 / 声明前缀 / 未归因都在那一处算）。
-- `by_rule` 与 `rule` 一起带出来：读者要能分清「这条线是按首段算的」还是「按声明前缀改写的」。
attributed AS (
    SELECT COALESCE(r.line, 'unknown') AS line,
           COALESCE(r.by_rule, 'none') AS by_rule,
           COALESCE(r.rule, '')        AS rule,
           CASE WHEN r.line IS NULL OR r.line = 'unknown'
                THEN COALESCE(r.raw_seg, '(deleted key)') END AS raw_seg,
           us.actual_usd, us.list_usd
    FROM usage us
    LEFT JOIN resolved r ON r.key_id = us.api_key_id
),
-- 取法与 line-quota.sql 的 route_key_spend 一致：走 resolved.rule（seg 命中时 rule=seg），
-- 不走 attributed.raw_seg（它只对 unknown 行非空，路由键经 seg 命中后是 NULL）。
route_key_spend AS (
    SELECT r.rule                       AS seg,
           count(*)                     AS requests,
           round(sum(us.actual_usd), 4) AS usd
    FROM usage us
    JOIN resolved r ON r.key_id = us.api_key_id
    WHERE r.by_rule = 'seg'
      AND r.rule IN (SELECT seg FROM route_segs)
    GROUP BY 1
)
-- 路由键读数（declare-route-keys-in-report）并进**同一条语句**，用 `by_rule='route'` 标记、
-- 排在末尾：别名表声明 route:true 的 seg 编码的是上游路由（折扣渠道）而非产品线，流量结构上
-- 不可按线拆分，归入「自有平台」只因键名归属登记。这笔钱必须作为一等读数摆出来，否则读的人
-- 会把该桶当杂项或当产品线。无声明时这组行不存在（不是伪造的零值行）。
--
-- 为什么不是「追加一条独立 SELECT」：CTE 是**语句级**的，第二条语句看不见 attributed /
-- route_key_spend，会报 relation does not exist；而 psql 默认遇错继续，把 stderr 一关，
-- 输出看上去「跑完了」（2026-11-15 首版就是这个形态，自己踩的）。
SELECT line,
       by_rule,
       attributed_by,
       requests,
       actual_usd,
       list_usd,
       pct
FROM (
    SELECT line,
           by_rule,
           COALESCE(raw_seg, rule)                              AS attributed_by,
           count(*)                                             AS requests,
           round(sum(actual_usd), 2)                             AS actual_usd,
           round(sum(list_usd), 2)                               AS list_usd,
           round(100 * sum(actual_usd) / NULLIF((SELECT sum(actual_usd) FROM attributed), 0), 1) AS pct,
           0                                                     AS sort_route
    FROM attributed
    GROUP BY line, by_rule, COALESCE(raw_seg, rule)
    UNION ALL
    SELECT rk.seg,
           'route',
           rk.seg,
           rk.requests,
           rk.usd,
           NULL::numeric,
           round(100 * rk.usd / NULLIF((SELECT sum(actual_usd) FROM attributed), 0), 1),
           1
    FROM route_key_spend rk
) t
ORDER BY sort_route, actual_usd DESC;
