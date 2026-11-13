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
        ('lex',      '识律 lex',      '主键名'),
        ('lexkg',    '识律 lex',      '同一产品的知识图谱键，2026-11-10 归并'),
        ('wanxing',  '萬星 wanxing',  '主键名'),
        ('wx',       '萬星 wanxing',  '萬星简写，2026-11-10 归并'),
        ('law',      'law-bench',     'law-bench 早期短名'),
        ('lawbench', 'law-bench',     'law-bench 无连字符写法'),
        ('yizuo',    '壹座 yizuo',    '平台自身承载的调用'),
        ('platform', '壹座 platform', '平台内部/演示 key')
),
-- <<< generated:line-aliases
attributed AS (
    SELECT COALESCE(a.line, 'unknown') AS line,
           CASE WHEN a.line IS NULL THEN split_part(COALESCE(k.name, '(deleted key)'), '-', 1) END AS raw_seg,
           us.actual_usd, us.list_usd
    FROM usage us
    LEFT JOIN api_keys k ON k.id = us.api_key_id
    LEFT JOIN aliases  a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
)
SELECT line,
       COALESCE(raw_seg, '')                                    AS raw_prefix_if_unknown,
       count(*)                                                 AS requests,
       round(sum(actual_usd), 2)                                AS actual_usd,
       round(sum(list_usd), 2)                                  AS list_usd,
       round(100 * sum(actual_usd) / NULLIF((SELECT sum(actual_usd) FROM attributed), 0), 1) AS pct
FROM attributed
GROUP BY line, raw_seg
ORDER BY actual_usd DESC;
