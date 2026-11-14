-- 词元按线花费 + 分组额度（只读，输出**一行 JSON**）
--
-- 给告警评估器吃（fd-industry-data/scripts/ciyuan_line_alerts.py），也给人数：
--   kubectl --context cheap-ts -n sub2api exec -i sub2api-postgres-0 -- \
--     psql -U sub2api -d sub2api -tA -v days=7 -f - < tools/line-quota.sql | python3 -m json.tool
--
-- 为什么是 JSON 而不是三节 psql 报表：评估器要的是**值**，人手看的是**表**。
-- 让同一份口径同时服务两者，唯一稳的办法是输出结构化数据，展示层各自渲染。
-- 反例是「SQL 里 \echo 一段、再 SELECT 一段」——评估器只能靠解析文本，形状一变就静默断。
--
-- 三条口径纪律（与 attribution-by-line.sql / line-metrics.sql 同一套）：
--   1. 线身份 = key 名首段；归并只认 tools/line-aliases.json 生成的 aliases CTE。
--   2. 无法归因的键**单列**（本文件给 unknown 的合计与前三名），不并入任何线。
--   3. 额度为 NULL 时**原样给 null**，并由 `limits_unset` 显式标真——
--      「没配上限」与「上限是 0」是两件事，评估器必须能分清（0 会读成"已经用满"）。

-- `:days` 没有默认值，必须由调用方给：psql 用 `-v days=7`，告警评估器由 load_sql() 替换。
-- **别在这里加 `\if`/`\set` 默认块**（2026-11-14 加过又撤回）：元命令只有 psql 认，
-- 而评估器用 psycopg 把整段当一条语句发给 Postgres，会直接 `syntax error at or near "\"`
-- ——症状出现在接产后的第一次真查，而静态形状测试全绿。红线由
-- `tools/gen-line-aliases.mjs --check` 强制（它把本文件标成 psycopg 消费方并 lint 反斜杠行）。

WITH usage AS (
    SELECT u.api_key_id,
           date_trunc('day', u.created_at)::date AS day,
           COALESCE(u.actual_cost, 0)::numeric AS actual_usd
    FROM usage_logs u
    WHERE u.created_at >= date_trunc('day', now()) - make_interval(days => :days - 1)
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
        ('特价',       '自有平台',      '路由命名的共享键（折扣渠道），不是产品线；user_id=1 的自有账号，消费方是 dsh harness，键名不含线信息 ⇒ 单列（证据见 openspec ciyuan-consumer-surface 5.5-1）'),
        ('openrouter', '自有平台',      '同上：上游路由命名的共享键，不是产品线，消费方同为 dsh harness。注意与模型 id `openrouter/free` 同词不同物——本字段只匹配 key 名首段')
),
-- <<< generated:line-aliases
attributed AS (
    SELECT COALESCE(a.line, 'unknown') AS line,
           CASE WHEN a.line IS NULL THEN split_part(COALESCE(k.name, '(deleted key)'), '-', 1) END AS raw_seg,
           us.day,
           us.actual_usd
    FROM usage us
    LEFT JOIN api_keys k ON k.id = us.api_key_id
    LEFT JOIN aliases  a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
),
per_line_day AS (
    SELECT line,
           CASE WHEN line = 'unknown' THEN COALESCE(raw_seg, '(deleted key)') ELSE '' END AS raw_prefix_if_unknown,
           day,
           count(*)                  AS requests,
           round(sum(actual_usd), 4) AS actual_usd
    FROM attributed
    GROUP BY line, raw_prefix_if_unknown, day
),
group_limits AS (
    SELECT g.name                                    AS group_name,
           g.subscription_type,
           g.daily_limit_usd,
           g.weekly_limit_usd,
           g.monthly_limit_usd,
           -- 「三个额度都没配」= 无上限。只用 IS NULL 判，不用 =0（0 是"立刻用满"）。
           (g.daily_limit_usd IS NULL
            AND g.weekly_limit_usd IS NULL
            AND g.monthly_limit_usd IS NULL)         AS limits_unset,
           COALESCE((
               SELECT round(sum(us.actual_usd), 4)
               FROM usage us
               JOIN api_keys k ON k.id = us.api_key_id
               WHERE k.group_id = g.id
                 AND us.day = now()::date
           ), 0)                                     AS today_usd
    FROM groups g
)
SELECT json_build_object(
    'generated_at', now(),
    'window_days', :days,
    'per_line_day', COALESCE((
        SELECT json_agg(json_build_object(
                   'line', line,
                   'raw_prefix_if_unknown', raw_prefix_if_unknown,
                   'day', day,
                   'requests', requests,
                   'usd', actual_usd)
               ORDER BY line, day)
        FROM per_line_day
    ), '[]'::json),
    'groups', COALESCE((
        SELECT json_agg(json_build_object(
                   'group', group_name,
                   'subscription_type', subscription_type,
                   'daily_limit_usd', daily_limit_usd,
                   'weekly_limit_usd', weekly_limit_usd,
                   'monthly_limit_usd', monthly_limit_usd,
                   'limits_unset', limits_unset,
                   'today_usd', today_usd)
               ORDER BY group_name)
        FROM group_limits
    ), '[]'::json)
)::text;
