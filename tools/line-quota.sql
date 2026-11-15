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
        ('特价',       '自有平台',      '路由命名的共享键（折扣渠道），不是产品线；user_id=1 的自有账号，消费方是 dsh harness，键名不含线信息 ⇒ 单列（证据见 openspec ciyuan-consumer-surface 5.5-1）；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测 7 日内部构成：deepseek-harness ~$99（壹座 dsh，经入口节点桥 10.<internal>.4/.9/.10 轮转——三桥花费几乎相等，是同一调用方不是三个）+ AsyncOpenAI/Python 单一内网主机 10.<internal>.1 ~$46（与 wanxing-probe- 前缀规则同一台机，属非产品流量却计入本桶 ⇒ 「自有平台」略被高估）+ 其余 ~$3'),
        ('openrouter', '自有平台',      '同上：上游路由命名的共享键，不是产品线，消费方同为 dsh harness。注意与模型 id `openrouter/free` 同词不同物——本字段只匹配 key 名首段；已声明为路由键（route:true），报表以 route_keys 读数呈现其不可拆分性；2026-11-15 按 UA 实测：本键 99.6% 是 deepseek-harness（~$90，入口桥 10.<internal>.12.0→12.1 于 11-14 切换）')
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
-- 归属走生成出来的 `resolved`：首段 / 声明前缀 / 未归因三档都在那一处算，报表不再自己 split_part。
-- `by_rule` 必须一路带下去——只有金额、没有依据时，「修正生效了」与「规则一条都没命中」读数同形。
attributed AS (
    SELECT COALESCE(r.line, 'unknown') AS line,
           COALESCE(r.by_rule, 'none') AS by_rule,
           COALESCE(r.rule, '')        AS rule,
           CASE WHEN r.line IS NULL OR r.line = 'unknown'
                THEN COALESCE(r.raw_seg, '(deleted key)') END AS raw_seg,
           us.day,
           us.actual_usd
    FROM usage us
    LEFT JOIN resolved r ON r.key_id = us.api_key_id
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
-- 归属依据的审计：每线按 seg / prefix / none 拆开（与 per_line_day 同源，只是多一个维度）。
-- 这一块不参与阈值判定，只回答「这一行的数是怎么来的」。
by_rule_all AS (
    SELECT line,
           by_rule,
           count(*)                  AS requests,
           round(sum(actual_usd), 4) AS actual_usd
    FROM attributed
    GROUP BY line, by_rule
),
-- 每条声明前缀在本窗口的命中量：0 命中的那条必须打出来。
-- 「规则写着、但一个键都没命中」与「规则生效了」在金额上同形，只有这一列能分开。
prefix_hits AS (
    SELECT COALESCE(r.rule, '') AS prefix,
           count(*)             AS requests,
           round(sum(us.actual_usd), 4) AS actual_usd
    FROM usage us
    JOIN resolved r ON r.key_id = us.api_key_id
    WHERE r.by_rule = 'prefix'
    GROUP BY 1
),
-- 声明过的线（首段表 + 前缀表的并集）。**用途只有一个**：把「声明了但本窗口没有用量」的线
-- 单独点名。少了它，一条线会整条从 per_line_day 里消失，读起来像「这条线没了」。
declared_lines AS (
    SELECT line FROM aliases
    UNION
    SELECT line FROM prefix_aliases
),
-- 路由键（route_segs 由生成器注入）在本窗口的逐键花费。
-- 取法走 resolved.rule（seg 命中时 rule=seg 本身）而不是 attributed.raw_seg——
-- 后者只对 unknown 行非空，路由键经 seg 命中后 raw_seg 是 NULL（2026-11-15 首版踩过）。
route_key_spend AS (
    SELECT r.rule                       AS seg,
           count(*)                     AS requests,
           round(sum(us.actual_usd), 4) AS usd
    FROM usage us
    JOIN resolved r ON r.key_id = us.api_key_id
    WHERE r.by_rule = 'seg'
      AND r.rule IN (SELECT seg FROM route_segs)
    GROUP BY 1
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
    ), '[]'::json),
    -- 归因依据：per_line_day 只给金额，读者无法由此判断「是按首段算的还是按声明前缀算的」。
    -- 这一块是那半个答案；hit=false 的前缀行＝声明了但本窗口一个键都没命中。
    'attribution', json_build_object(
        'by_rule', COALESCE((
            SELECT json_agg(json_build_object(
                       'line', line,
                       'by_rule', by_rule,
                       'requests', requests,
                       'usd', actual_usd)
                   ORDER BY line, by_rule)
            FROM by_rule_all
        ), '[]'::json),
        'declared_prefixes', COALESCE((
            SELECT json_agg(json_build_object(
                       'prefix', pa.prefix,
                       'line', pa.line,
                       'why', pa.why,
                       'hit', ph.prefix IS NOT NULL,
                       'requests', COALESCE(ph.requests, 0),
                       'usd', COALESCE(ph.actual_usd, 0))
                   ORDER BY pa.prefix)
            FROM prefix_aliases pa
            LEFT JOIN prefix_hits ph ON ph.prefix = pa.prefix
        ), '[]'::json),
        -- 声明了但没有用量的线：不是「这条线不存在」，是这个窗口里没有键在用（或全被前缀改写走）。
        'declared_lines_without_usage', COALESCE((
            SELECT json_agg(d.line ORDER BY d.line)
            FROM declared_lines d
            WHERE NOT EXISTS (SELECT 1 FROM by_rule_all b WHERE b.line = d.line)
        ), '[]'::json),
        -- 路由键读数（declare-route-keys-in-report）：别名表声明 route:true 的 seg，
        -- 其键名编码上游路由（折扣渠道）而非产品线，流量结构上不可按线拆分。
        -- 这笔钱归「自有平台」只因为键名归属登记——把它当杂项或当产品线都是误读，
        -- 所以它必须作为一等读数出现在报表里，而不是只活在别名表的注释里。
        -- 无声明时 keys 为空集、note 缺省，绝不伪造逐键条目；share 分母为 0 时 NULL
        -- （「没有花费」与「算不出份额」不同）。
        'route_keys', json_build_object(
            'keys', COALESCE((
                SELECT json_agg(json_build_object(
                           'seg', rk.seg,
                           'requests', rk.requests,
                           'usd', rk.usd)
                       ORDER BY rk.usd DESC)
                FROM route_key_spend rk
            ), '[]'::json),
            'total_usd', COALESCE((SELECT round(sum(usd), 4) FROM route_key_spend), 0),
            'requests', COALESCE((SELECT sum(requests) FROM route_key_spend), 0),
            'share_of_window',
                (SELECT round(sum(usd), 4) FROM route_key_spend)
                / NULLIF((SELECT round(sum(actual_usd), 4) FROM usage), 0),
            'note', CASE WHEN EXISTS (SELECT 1 FROM route_segs) THEN
                '键名编码的是上游路由（折扣渠道），不是产品线；流量结构上无法按线拆分，归入其声明的线仅因键名归属登记'
            END
        )
    )
)::text;
