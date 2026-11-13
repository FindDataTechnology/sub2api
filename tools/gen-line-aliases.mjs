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
  { file: join(HERE, "line-quota.sql"), regions: 1 },
];

const BEGIN = "-- >>> generated:line-aliases（真源 tools/line-aliases.json；改它，别改这里）";
const END = "-- <<< generated:line-aliases";

function loadAliases() {
  const doc = JSON.parse(readFileSync(SOURCE, "utf8"));
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
    seen.set(row.seg, row.line);
  }
  return rows;
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

function render(rows) {
  const pad = (s, target) => s + " ".repeat(Math.max(0, target - displayWidth(s)));
  const wSeg = Math.max(...rows.map((r) => displayWidth(`'${r.seg}',`)));
  const wLine = Math.max(...rows.map((r) => displayWidth(`'${r.line}',`)));
  // VALUES 的每一行必须自带括号：`('lex', '识律 lex', '主键名'),`
  // 首版漏了它，真库直接 `syntax error at or near "'lex'"`——形状测试看不出来。
  const body = rows
    .map((r) => `        (${pad(`'${r.seg}',`, wSeg)} ${pad(`'${r.line}',`, wLine)} '${r.why}')`)
    .join(",\n");
  // 结尾的 `),` 属于生成区域：这四个落点全是 WITH 列表里的 CTE 项，逗号是分隔符。
  // 首版只写 `)`，把 SQL 直接改成语法错误——验证时要 `psql` 真跑或至少看 EXPLAIN。
  return `${BEGIN}
aliases(seg, line, why) AS (
    VALUES
${body}
),
${END}`;
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

function main() {
  const check = process.argv.includes("--check");
  const rows = loadAliases();
  const generated = render(rows);
  let drift = 0;

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

  if (check) {
    if (drift > 0) {
      console.error(`\n✗ ${drift} 个报表的别名区域有漂移。跑 node tools/gen-line-aliases.mjs 重新生成，别手改 .sql。`);
      process.exit(1);
    }
    console.log(`✓ 别名口径一致：${rows.length} 条别名，${TARGETS.length} 个报表`);
  }
}

main();
