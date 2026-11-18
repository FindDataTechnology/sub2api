#!/usr/bin/env node
// ── 别名 CTE 生成器（ciyuan-consumer-surface 4.1）────────────────────────────
//
// 为什么需要它：`tools/line-metrics.sql` 的头注写着「别名表只能有一份——拆开之后
// 同一条线会在两个报表里变成两个数」，而那个文件自己把表**粘了三份**
// （2026-11-13 实测：第 47/85/118 行），加上 `attribution-by-line.sql` 一份，
// 全仓共 4 份、两种形状（其中两份丢了 `why` 理由列）。也就是说：口径其实还没有
// 单一真源，只是「四份当前恰好一致」。下一次加线要手改四处，漏一处就漂。
//
// 现在：真源是 `tools/line-aliases.json`；两个 .sql 里的别名 CTE 变成**生成区域**，
// 由本脚本写入。`--check` 不写文件，只比对当前内容与生成结果，不一致即红（CI / 手跑）。
//
//   node tools/gen-line-aliases.mjs           # 重新生成
//   node tools/gen-line-aliases.mjs --check    # 只比对，有漂移退出 1
//
// 生成区域的边界是两行标记；标记之外的内容一律不碰。

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const HERE = dirname(fileURLToPath(import.meta.url));
const SOURCE = join(HERE, "line-aliases.json");
const TARGETS = [
  { file: join(HERE, "attribution-by-line.sql"), regions: 1 },
  { file: join(HERE, "line-metrics.sql"), regions: 3 },
  // line-quota.sql 是**唯一有第二个消费方**的报表：告警评估器用 psycopg 执行它
  // （其它两份只给人用 psql 看，`\echo` 分隔块与 `-v` 默认值是它们的正常形态）。
  // 所以元命令红线只钉这一个——哪天另一份也有了程序化消费方，就在这里补 `psycopg: true`。
  { file: join(HERE, "line-quota.sql"), regions: 1, psycopg: true },
];

// Go 侧也要认同一份别名（词元控制台的「按线」视图在 Go 里做归并）。
// 不 embed 而是生成一个 .go：`tools/` 不在 `backend/` 的包目录下，go:embed 取不到；
// 生成件由 --check 一并校验，所以真源仍然只有 json 一处。
const GO_TARGET = join(HERE, "..", "backend", "internal", "pkg", "usagestats", "line_aliases_gen.go");

const BEGIN = "-- >>> generated:line-aliases（真源 tools/line-aliases.json；改它，别改这里）";
const END = "-- <<< generated:line-aliases";

// ── 前缀匹配：连字符边界 ────────────────────────────────────────────────────
//
// 命中条件 = 名字**等于前缀本身**，或名字以 `<prefix>-` 开头（前缀自带尾 '-' 时退化为
// 纯粹的 starts_with，既有三条规则的读数一个字节都不变）。
//
// 为什么要这一档：有些内部服务的键名**恰好等于服务名**（`fd-memory`），没有更深一层的
// `fd-memory-xyz`。`starts_with` 语义当然是 true，但生成器原先要求前缀以 '-' 结尾 ⇒ 这种
// 形态根本写不进去（2026-11-16 实测：`fd-memory` 7 日 9,571 次嵌入调用、$0.1053，是调用量
// 最高的内部产品键，却只能落 unknown）。而一旦允许不带尾 '-'，就必须同时收紧语义：
// `fd-memory` 不许吃掉 `fd-memory2`（那是另一个服务名，不是它的子段）。
//
// 两个消费方（SQL 的 LATERAL、Go 的 LineAliasForName）用的是同一条判据，SQL 侧的表达式
// 在 render() 里，Go 侧在 line_stats.go:prefixMatches。
function prefixMatches(name, prefix) {
  if (name === prefix) return true;
  return name.startsWith(prefix.endsWith("-") ? prefix : `${prefix}-`);
}

// 去掉尾 '-' 之后的键：仅用于「同一条规则的两种写法」去重与排序，不改变写进生成区的原文。
function prefixKey(prefix) {
  return prefix.endsWith("-") ? prefix.slice(0, -1) : prefix;
}

function loadAliases() {  const doc = JSON.parse(readFileSync(SOURCE, "utf8"));
  const rows = doc.aliases;
  if (!Array.isArray(rows) || rows.length === 0) {
    throw new Error("tools/line-aliases.json 里没有 aliases 数组");
  }
  const seen = new Map();
  for (const row of rows) {
    for (const key of ["seg", "line", "why"]) {
      if (typeof row[key] !== "string" || row[key].trim() === "") {
        // 不写理由的归并等同于猜，这里直接拒——理由见 json 的 _rules。
        throw new Error(`别名缺 ${key}：${JSON.stringify(row)}`);
      }
    }
    if (seen.has(row.seg)) {
      throw new Error(`seg 重复：${row.seg}（既有 ${seen.get(row.seg)}，又来 ${row.line}）`);
    }
    // ── route：声明「这个 seg 是路由键」（declare-route-keys-in-report）──────────
    // 路由键 = 键名编码上游路由（折扣渠道）而非产品线的共享键。它必须**显式声明**：
    // 靠键名猜（含"折"/"channel"之类）会把真正的产品线误伤进不可归因桶——
    // 「不可归因」是一个强断言，宁可漏报也不许误报，所以只认显式 true。
    if (row.route !== undefined && typeof row.route !== "boolean") {
      throw new Error(`route 必须是布尔值：${JSON.stringify(row)}`);
    }
    if (row.route === true && (typeof row.why !== "string" || row.why.trim() === "")) {
      throw new Error(`route:true 的行必须带 why（声明不可归因却不给理由，等于拍脑袋）：${row.seg}`);
    }
    seen.set(row.seg, row.line);
  }

  // ── name_prefixes：首段撒谎时的修正 ────────────────────────────────────────
  // 三条校验各自拦一种「静默改写」：
  //  ① 不以 '-' 结尾 ⇒ 它其实是个 seg（`wanxing-lex` 会把 `wanxing-lexfoo-1` 也吃进来）；
  //  ② 不在本条 seg 之下 ⇒ 打错一个字就能把无关的键族整片换归属，而报表看不出来；
  //  ③ 全局重复 ⇒ 两条规则都命中时归属取决于生成顺序，那是「看运气」不是口径。
  const prefixes = [];
  const seenPrefix = new Map();
  for (const row of rows) {
    const list = row.name_prefixes;
    if (list === undefined) continue;
    if (!Array.isArray(list) || list.length === 0) {
      throw new Error(`name_prefixes 必须是非空数组（要么不写这个键）：${JSON.stringify(row)}`);
    }
    for (const p of list) {
      for (const key of ["prefix", "line", "why"]) {
        if (typeof p[key] !== "string" || p[key].trim() === "") {
          throw new Error(`name_prefixes 缺 ${key}：${JSON.stringify(p)}（挂在 seg=${row.seg} 下）`);
        }
      }
      if (!p.prefix.endsWith("-")) {
        throw new Error(
          `name_prefixes 的 prefix 必须以 '-' 结尾：${p.prefix}` +
            `\n  否则它就是个 seg（会连 '${p.prefix}foo-1' 一起吃），请改成 '${p.prefix}-' 或挪进 aliases。`,
        );
      }
      if (!p.prefix.startsWith(`${row.seg}-`)) {
        throw new Error(
          `name_prefixes 的 prefix 必须落在本条 seg 之下：'${p.prefix}' 不以 '${row.seg}-' 开头` +
            `\n  前缀规则只能细化一个已登记的首段，不能凭空改写别的键族（打错一个字就会静默换归属）。`,
        );
      }
      if (seenPrefix.has(prefixKey(p.prefix))) {
        throw new Error(
          `name_prefixes 重复：${p.prefix}（既有 ${seenPrefix.get(prefixKey(p.prefix))}，又来 ${p.line}）`,
        );
      }
      seenPrefix.set(prefixKey(p.prefix), p.line);
      prefixes.push({ prefix: p.prefix, line: p.line, why: p.why, seg: row.seg });
    }
  }
  // ── prefix_families + prefixes：族前缀下的细则 ──────────────────────────────
  // 为什么需要第二档：线身份是 key 名首段，而有些首段是**组织/族前缀**（`fd-`），不是一条线；
  // `name_prefixes` 只能细化一个已登记的首段，于是「首段是族前缀」的键族永远归不了。
  // 2026-11-16 线上实读（7 日）：`text-core` 是 19 条已声明线之一，而它的调用键叫
  // `fd-text-core-embedding-bulk`（$0.4449 / 2,783 次），首段是 `fd` ⇒ 整条线在报表里看不见自己。
  //
  // 四条校验各自拦一种「静默改写」：
  //  ① 细则没钉住任何真实键名 ⇒ 打错一个字（`fd-tex-core-`）会变成一条**匹配不到任何键**的规则：
  //     报表静默不变、没人发现。所以族内细则必须带 `covers`（至少一个精确键名），
  //     且每个键名都得以该前缀开头——错一个字母当场红；
  //  ② 不以 '-' 结尾 ⇒ 它其实是首段规则（`fd-tex` 会连 `fd-texfoo-1` 一起吃）；
  //  ③ 与 name_prefixes 全局重复 ⇒ 两条规则都命中时归属取决于生成顺序，那是「看运气」不是口径；
  //  ④ 族前缀本身就是已登记的别名 seg ⇒ 那种情形本该写 name_prefixes，两条路并存就是第二处口径。
  const families = new Map();
  const famList = doc.prefix_families;
  if (famList !== undefined) {
    if (!Array.isArray(famList) || famList.length === 0) {
      throw new Error("prefix_families 必须是非空数组（要么不写这个键）");
    }
    for (const f of famList) {
      for (const key of ["seg", "why"]) {
        if (typeof f[key] !== "string" || f[key].trim() === "") {
          throw new Error(`prefix_families 缺 ${key}：${JSON.stringify(f)}`);
        }
      }
      if (f.seg.includes("-")) {
        throw new Error(`prefix_families 的 seg 只能是首个连字符之前那一段：${f.seg}`);
      }
      if (seen.has(f.seg)) {
        throw new Error(
          `prefix_families 的 '${f.seg}' 已经是别名 seg（line=${seen.get(f.seg)}）` +
            `\n  那种情形请在该 seg 下写 name_prefixes——两条路并存就是第二处口径。`,
        );
      }
      if (families.has(f.seg)) throw new Error(`prefix_families 重复：${f.seg}`);
      families.set(f.seg, f.why);
    }
  }

  const famPrefixes = doc.prefixes;
  if (famPrefixes !== undefined && !Array.isArray(famPrefixes)) {
    throw new Error("prefixes 必须是数组（要么不写这个键）");
  }
  for (const p of famPrefixes || []) {
    for (const key of ["prefix", "line", "why"]) {
      if (typeof p[key] !== "string" || p[key].trim() === "") {
        throw new Error(`prefixes 缺 ${key}：${JSON.stringify(p)}`);
      }
    }
    // 前缀按**连字符边界**匹配（见 prefixMatches）：带不带尾 '-' 都合法。
    // 不带尾 '-' 的那一形态是为「键名恰好等于服务名」准备的（`fd-memory`），
    // 而它不会吞掉 `fd-memory2` 这种近邻——边界判据只管 `<prefix>` 与其 `-子段`。
    // 因此「漏写尾 '-'」不再是一条格式错误，而是一条**能被 covers 抓到**的语义错误。
    const seg = p.prefix.indexOf("-") < 0 ? p.prefix : p.prefix.slice(0, p.prefix.indexOf("-"));
    if (!families.has(seg)) {
      throw new Error(
        `prefixes 的 '${p.prefix}' 落在未登记的前缀族 '${seg}' 下` +
          `\n  先在 prefix_families 里声明这个族（为什么它是族前缀而不是一条线），再写细则。`,
      );
    }
    // covers：这条规则**钉住**的键名。族是按首段算的，所以中间那段打错一个字不会造出新族，
    // 只会变出一条匹配不到任何键的规则——报表静默不变，没有人会发现。把键名写下来，
    // 拼错就不再是「静默」而是「生成器拒」。
    if (!Array.isArray(p.covers) || p.covers.length === 0) {
      throw new Error(
        `prefixes 的 '${p.prefix}' 缺 covers（至少一个精确键名）：` +
          `\n  规则必须钉住真实键名，否则拼错一个字就会静默变成「匹配不到任何键」。`,
      );
    }
    for (const name of p.covers) {
      if (typeof name !== "string" || name.trim() === "") {
        throw new Error(`prefixes 的 '${p.prefix}' 的 covers 里有非字符串/空串：${JSON.stringify(p.covers)}`);
      }
      if (!prefixMatches(name, p.prefix)) {
        throw new Error(
          `prefixes 的 '${p.prefix}' 的 covers 里有键不满足该前缀的连字符边界：'${name}'` +
            `\n  边界 = 等于前缀本身、或以其 '-子段' 开头。拼错一个字母就会静默失效，这里当场拒。`,
        );
      }
    }
    if (p.prefix === `${seg}-` || p.prefix === seg) {
      // 整族归一条线不需要细则：把它写成 aliases 里的一行 seg 即可（`vaas` 就是这样）。
      throw new Error(
        `prefixes 的 '${p.prefix}' 只有首段，那是一条首段规则而不是细则：请挪进 aliases。`,
      );
    }
    if (seenPrefix.has(prefixKey(p.prefix))) {
      throw new Error(
        `prefixes 重复：${p.prefix}（既有 ${seenPrefix.get(prefixKey(p.prefix))}，又来 ${p.line}）`,
      );
    }
    seenPrefix.set(prefixKey(p.prefix), p.line);
    prefixes.push({ prefix: p.prefix, line: p.line, why: p.why, seg });
  }

  // 最长优先由**生成顺序**保证（SQL 的 LATERAL 与 Go 的循环都按这个顺序取第一条命中），
  // 所以排序放在这里——两个消费方自然同序，不必各自再排一次。
  // 长度按**去掉尾 '-'** 之后算：`fd-text-core-` 与 `fd-text-core` 是同一条规则的两种写法，
  // 排序不能因为写法不同而落到不同位置。
  prefixes.sort(
    (a, b) =>
      [...prefixKey(b.prefix)].length - [...prefixKey(a.prefix)].length ||
      (prefixKey(a.prefix) < prefixKey(b.prefix) ? -1 : 1),
  );
  const routeSegs = rows.filter((r) => r.route === true).map((r) => r.seg);
  return { rows, prefixes, routeSegs };
}

// 统一的 CTE 形状：三列都保留。`why` 在某些报表里不被 SELECT，但留着零成本，
// 而且让四处生成结果逐字相同——这正是「只有一份」在文本上的证明。
// 终端里 CJK 占两列；按 code point 数补空格会让中文行看起来错位
// （首版就是这样：`'law',      'law-bench',   'law-bench 早期短名'`）。
function displayWidth(s) {
  let w = 0;
  for (const ch of s) {
    const cp = ch.codePointAt(0);
    const wide =
      (cp >= 0x1100 && cp <= 0x115f) ||   // 韩文字母
      (cp >= 0x2e80 && cp <= 0xa4cf) ||   // CJK 部首 … 彝文
      (cp >= 0xac00 && cp <= 0xd7a3) ||   // 韩文音节
      (cp >= 0xf900 && cp <= 0xfaff) ||   // CJK 兼容汉字
      (cp >= 0xfe30 && cp <= 0xfe6f) ||   // CJK 兼容形式
      (cp >= 0xff00 && cp <= 0xff60) ||   // 全角
      (cp >= 0xffe0 && cp <= 0xffe6);
    w += wide ? 2 : 1;
  }
  return w;
}

function render(rows, prefixes, routeSegs) {
  const pad = (s, target) => s + " ".repeat(Math.max(0, target - displayWidth(s)));
  const wSeg = Math.max(...rows.map((r) => displayWidth(`'${r.seg}',`)));
  const wLine = Math.max(...rows.map((r) => displayWidth(`'${r.line}',`)));
  // VALUES 的每一行必须自带括号：`('lex', '识律 lex', '主键名'),`
  // 首版漏了它，真库直接 `syntax error at or near "'lex'"`——形状测试看不出来。
  const body = rows
    .map((r) => `        (${pad(`'${r.seg}',`, wSeg)} ${pad(`'${r.line}',`, wLine)} '${r.why}')`)
    .join(",\n");

  // 空前缀表也必须能跑：`VALUES ()` 是语法错误，用 `SELECT … WHERE false` 表空集。
  // 这一支不是假想——「把 name_prefixes 清空、报表应回到改前数值」正是本单的自证用例，
  // 而第一版把 `VALUES` 写死在模板里，清空后渲染成 `VALUES SELECT …`，直到真跑才暴露。
  let prefixCte;
  if (prefixes.length === 0) {
    prefixCte =
      "prefix_aliases(prefix, line, why) AS (\n" +
      "    SELECT NULL::text, NULL::text, NULL::text WHERE false\n" +
      ")";
  } else {
    const wPref = Math.max(...prefixes.map((p) => displayWidth(`'${p.prefix}',`)));
    const wPLine = Math.max(...prefixes.map((p) => displayWidth(`'${p.line}',`)));
    const prefixRows = prefixes
      .map((p) => `        (${pad(`'${p.prefix}',`, wPref)} ${pad(`'${p.line}',`, wPLine)} '${p.why}')`)
      .join(",\n");
    prefixCte = `prefix_aliases(prefix, line, why) AS (\n    VALUES\n${prefixRows}\n)`;
  }

  // ── route_segs：路由键清单（declare-route-keys-in-report）────────────────────
  // 与 prefix_aliases 同一条先例：空表必须能跑，用 `SELECT … WHERE false`，
  // 不许把 `VALUES` 写死在模板里（2026-11-15 空前缀臂就是这么红的）。
  // 三份 SQL 的生成区**同构注入**（line-metrics 正文不引用它，CTE 未被引用时零开销）——
  // 避免造出「哪份有哪份没有」的第二处漂移面。
  let routeCte;
  if (routeSegs.length === 0) {
    routeCte =
      "route_segs(seg) AS (\n" +
      "    SELECT NULL::text WHERE false\n" +
      ")";
  } else {
    // 单列 VALUES 的行是 `('seg')`——首版照抄两列形状写了 `('seg',  )`，
    // 行内尾逗号是语法错误（真库 14:39Z 抓到；形状测试看不出来，必须真跑）。
    const wRoute = Math.max(...routeSegs.map((s) => displayWidth(`'${s}'`)));
    const routeRows = routeSegs
      .map((s) => `        (${pad(`'${s}'`, wRoute)})`)
      .join(",\n");
    routeCte = `route_segs(seg) AS (\n    VALUES\n${routeRows}\n)`;
  }

  // 结尾的 `),` 属于生成区域：这四个落点全是 WITH 列表里的 CTE 项，逗号是分隔符。
  // 首版只写 `)`，把 SQL 直接改成语法错误——验证时要 `psql` 真跑或至少看 EXPLAIN。
  //
  // `resolved` 把「按什么归的」也变成数据（by_rule + rule），这是报表能自证口径的前提：
  // 只有金额、没有依据时，「归因生效了」与「规则一条都没命中」在读数上同形。
  return `${BEGIN}
aliases(seg, line, why) AS (
    VALUES
${body}
),
${prefixCte},
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
        WHERE COALESCE(k.name, '') = pa.prefix
           OR starts_with(COALESCE(k.name, ''),
                          CASE WHEN right(pa.prefix, 1) = '-' THEN pa.prefix
                               ELSE pa.prefix || '-' END)
        ORDER BY length(rtrim(pa.prefix, '-')) DESC
        LIMIT 1
    ) p ON true
    LEFT JOIN aliases a ON a.seg = split_part(COALESCE(k.name, ''), '-', 1)
),
${routeCte},
${END}`;
}

// ── 纯 SQL 红线：禁 psql 元命令 ──────────────────────────────────────────────
//
// `\if` / `\set` / `\echo` 这类元命令只有 psql 客户端认。告警评估器
// （`fd-industry-data/scripts/ciyuan_line_alerts.py`）用 psycopg 把整段 SQL 当**一条语句**
// 发给 Postgres，遇到 `\` 会直接 `syntax error at or near "\"`——而且是在接产后的
// 第一次真查时才炸，静态形状测试全绿。
//
// 2026-11-14 实测教训：为了让 `psql -f` 不带 `-v days=7` 也能跑，在 line-quota.sql 头部
// 加了 `\if :{?days}` 默认块；而评估器的 load_sql() 文档正写着「刻意不用 \if/\set 默认块，
// 默认值改由调用方给」。两边各自都有理由、各自都是绿的，契约却在中间断了。
// 纯 SQL 两个消费方都能用（psql 用 `-v days=N` 替换 `:days`），所以把元命令钉成红线，
// 别再为了「不带参数也能跑」这一点方便去换一次真实故障。
function lintPsqlMeta(file) {
  const bad = [];
  readFileSync(file, "utf8").split("\n").forEach((line, i) => {
    if (line.trimStart().startsWith("\\")) bad.push(i + 1);
  });
  return bad;
}

// 一份文件里可能有多处生成区域（line-metrics.sql 有三处：花费 / 错误 / 延迟）。
// 首版只替换第一处，剩下两处留成空壳——所以这里必须**遍历全部**，并核对数量。
function replaceRegions(text, generated, file, expected) {
  let out = text;
  let cursor = 0;
  let count = 0;
  for (;;) {
    const begin = out.indexOf(BEGIN, cursor);
    if (begin === -1) break;
    const end = out.indexOf(END, begin);
    if (end === -1) {
      throw new Error(`${file}: 在第 ${begin} 字符处找到 >>> 标记，后面没有 <<< 标记`);
    }
    const afterEnd = out.indexOf("\n", end);
    const tail = afterEnd === -1 ? "" : out.slice(afterEnd);
    out = out.slice(0, begin) + generated + tail;
    cursor = begin + generated.length;
    count += 1;
  }
  if (count !== expected) {
    throw new Error(`${file}: 期望 ${expected} 处生成区域，实际 ${count} 处`);
  }
  return out;
}

function renderGo(rows, prefixes, routeSegs) {
  const pad = (s, n) => s + " ".repeat(Math.max(0, n - [...s].length));
  const keys = rows.map((r) => r.seg);
  const w = Math.max(...keys.map((k) => [...`"${k}":`].length));
  // 路由键清单也要进 Go：控制台读面与三份报表必须用**同一份**声明。两处各写一张表，
  // 分叉时不是「两张表不一致」，是「同一件事有两个答案」，而读的人分不出哪个对。
  // 键宽按最长项补齐：生成件必须本身是 gofmt 干净的，否则 `gofmt -w` 与
  // `--check` 会互相打架（改一个就红另一个）。
  const rw = Math.max(0, ...routeSegs.map((s) => [...`"${s}":`].length));
  const rBody = routeSegs.map((s) => `\t${pad(`"${s}":`, rw)} {},`).join("\n");
  const body = rows
    .map((r) => `\t${pad(`"${r.seg}":`, w)} ${JSON.stringify(r.line)},`.replace(/"/g, '"'))
    .join("\n");
  // 前缀表按**生成顺序**（最长优先）排；Go 侧与 SQL 侧都取第一条命中，所以两边自然同序。
  const pBody = prefixes
    .map(
      (p) =>
        `\t{Prefix: ${JSON.stringify(p.prefix)}, Line: ${JSON.stringify(p.line)}, ` +
        `Why: ${JSON.stringify(p.why)}},`,
    )
    .join("\n");
  return `// Code generated by tools/gen-line-aliases.mjs from tools/line-aliases.json. DO NOT EDIT.
//
// 这个文件是**生成件**：真源是 tools/line-aliases.json（改它，再跑
// \`node tools/gen-line-aliases.mjs\`）。生成而不是 go:embed，是因为 tools/ 不在
// backend/ 的包目录下，embed 取不到；--check 会连带校验本文件，所以真源仍只有一处。
package usagestats

// lineAliases 把 key 名首段（split_part(name,'-',1)）归并到线名。
// 没有命中的段一律落 unknown，**不许猜**——推测的归属比缺失更坏。
var lineAliases = map[string]string{
${body}
}

// LineAliasReason 是归并理由，给读面解释「为什么 wx 算萬星」时用。
var LineAliasReason = map[string]string{
${rows.map((r) => `\t${pad(`"${r.seg}":`, w)} ${JSON.stringify(r.why)},`).join("\n")}
}

// linePrefixAliases 是「首段撒谎」的修正表：key 名首段属于 A 线、实际用量由 B 线受益时，
// 按**整名前缀**改写归属。顺序即优先级（最长前缀在前），与 tools/line-quota.sql 里
// resolved CTE 的 LATERAL 取法一致；两边一旦分叉，--check 会红。
var linePrefixAliases = []LinePrefixAlias{${pBody === "" ? "" : "\n" + pBody + "\n"}}

// lineRouteSegs 是声明为**路由键**（tools/line-aliases.json 里 "route": true）的段：
// 键名编码的是上游折扣路由，不是产品线，因此其花费在结构上拆不到任何一条线。
// 报表把它们的聚合摆成 route_keys 一等读数；控制台读面靠这张表把「含路由键的那一行」
// 标出来，免得「自有平台」被读成一条产品线。**这张表也必须只有一处真源。**
var lineRouteSegs = map[string]struct{}{${rBody === "" ? "" : "\n" + rBody + "\n"}}
`;
}

function main() {
  const check = process.argv.includes("--check");
  const { rows, prefixes, routeSegs } = loadAliases();
  const generated = render(rows, prefixes, routeSegs);
  let drift = 0;

  // 元命令检查先跑：它不看别名内容，坏了就是「psycopg 一定跑不起来」，
  // 与是否存在漂移无关，所以两种模式下都直接判死。只查 psycopg 消费的文件。
  let meta = 0;
  for (const { file, psycopg } of TARGETS) {
    if (!psycopg) continue;
    const bad = lintPsqlMeta(file);
    if (bad.length === 0) continue;
    meta += bad.length;
    console.error(
      `× ${file}:${bad.join(",")} 出现 psql 元命令（反斜杠开头）。\n` +
        `  这些文件要同时被 psql 和评估器的 psycopg 执行；元命令只有 psql 认，` +
        `psycopg 会报 syntax error at or near "\\"。\n` +
        `  默认值改由调用方给：人跑用 psql -v days=N，评估器由 load_sql() 替换 :days。`,
    );
  }
  if (meta > 0) process.exit(1);

  for (const { file, regions } of TARGETS) {
    const before = readFileSync(file, "utf8");
    const after = replaceRegions(before, generated, file, regions);
    if (before === after) continue;
    if (check) {
      drift += 1;
      console.error(`× ${file}：别名区域与 tools/line-aliases.json 不一致`);
      continue;
    }
    writeFileSync(file, after);
    console.log(`✓ 已生成 ${file}（${regions} 处区域）`);
  }

  const goBefore = readFileSync(GO_TARGET, "utf8");
  const goAfter = renderGo(rows, prefixes, routeSegs);
  if (goBefore !== goAfter) {
    if (check) {
      drift += 1;
      console.error(`× ${GO_TARGET}：Go 侧别名表与 tools/line-aliases.json 不一致`);
    } else {
      writeFileSync(GO_TARGET, goAfter);
      console.log(`✓ 已生成 ${GO_TARGET}`);
    }
  }

  if (check) {
    if (drift > 0) {
      console.error(`\n✗ ${drift} 处漂移。跑 node tools/gen-line-aliases.mjs 重新生成，别手改生成件。`);
      process.exit(1);
    }
    console.log(`✓ 别名口径一致：${rows.length} 条别名 + ${prefixes.length} 条前缀修正 + ${routeSegs.length} 条路由键（${routeSegs.join(" / ") || "无"}），${TARGETS.length} 个报表 + Go 侧`);
  }
}

main();
