#!/usr/bin/env python3
"""逐条红证：每条校验必须真的拒一次，且拒绝时不得写任何生成件；
另有一条**正证**：不带尾 '-' 的前缀（键名恰好等于服务名）必须被接受。

为什么需要正证这一档：本片把「prefix 必须以 '-' 结尾」这条**格式**校验换成了
「按连字符边界匹配」这条**语义**校验。如果只保留红证，那么「把整条校验删掉」
和「校验变成恒真」两种写法都会一路绿——正证（该过的必须过）才是那条判据还在的证明。
"""
import hashlib
import json
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
JSON = ROOT / "tools" / "line-aliases.json"
BAK = ROOT / "tools" / "line-aliases.json.armsbak"
GEN = [ROOT / "tools" / "line-quota.sql", ROOT / "tools" / "line-metrics.sql",
       ROOT / "tools" / "attribution-by-line.sql",
       ROOT / "backend" / "internal" / "pkg" / "usagestats" / "line_aliases_gen.go"]


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run_gen():
    return subprocess.run(["node", "tools/gen-line-aliases.mjs"], cwd=ROOT,
                          capture_output=True, text=True)


def _load():
    return json.loads(BAK.read_text(encoding="utf-8"))


def _write(doc):
    JSON.write_text(json.dumps(doc, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def arm(name, mutate, expect):
    """红证：期望生成器拒绝、报文说中、且一个生成件都没被写。"""
    doc = _load()
    mutate(doc)
    _write(doc)
    before = {p.name: sha(p) for p in GEN}
    r = run_gen()
    after = {p.name: sha(p) for p in GEN}
    out = (r.stdout or "") + (r.stderr or "")
    ok = r.returncode != 0 and expect in out and before == after
    print(f"[{'✓' if ok else '✗'}] {name}  rc={r.returncode} 说中了={expect in out} 未写文件={before == after}")
    if expect not in out:
        print("    实际输出：", out.strip().splitlines()[-1] if out.strip() else "(空)")
    return ok


def posit(name, mutate, check):
    """正证：期望生成器**接受**，且生成区里确实出现了这次改动。"""
    doc = _load()
    mutate(doc)
    _write(doc)
    r = run_gen()
    out = (r.stdout or "") + (r.stderr or "")
    wrote = check()
    ok = r.returncode == 0 and wrote
    print(f"[{'✓' if ok else '✗'}] {name}  rc={r.returncode} 生成区含改动={wrote}")
    if not ok:
        print("    实际输出：", out.strip().splitlines()[-1] if out.strip() else "(空)")
    return ok


# ── 变异体 ──────────────────────────────────────────────────────────────────
def no_dash_ok(p):
    # 正证：把既有细则的尾 '-' 去掉。键名 fd-text-core-embedding-bulk 满足边界
    # （以 'fd-text-core-' 开头）⇒ 必须被接受，且生成区里前缀原文变成 'fd-text-core'
    p["prefixes"][0]["prefix"] = "fd-text-core"


def no_dash_typo(p):
    # 红证：不带尾 '-' 的前缀拼错一个字 ⇒ 边界不成立（既不等、也不以 'fd-memor-' 开头）
    p["prefixes"][1]["prefix"] = "fd-memor"


def dash_typo(p):
    p["prefixes"][0]["prefix"] = "fd-tex-core-"


def no_covers(p):
    p["prefixes"][0].pop("covers", None)


def covers_outside(p):
    # covers 里的键名不在该前缀的连字符边界内（另一条子族）
    p["prefixes"][0]["covers"] = ["fd-memory"]


def bare_family(p):
    p["prefixes"][0]["prefix"] = "fd-"
    p["prefixes"][0]["covers"] = ["fd-anything"]


def bare_seg(p):
    # 前缀恰好等于族首段（不带尾 '-'）⇒ 那是一条首段规则，请挪进 aliases
    p["prefixes"][0]["prefix"] = "fd"
    p["prefixes"][0]["covers"] = ["fd-memory"]


def dup_after_normalize(p):
    # 'fd-memory' 与 'fd-memory-' 是同一条规则的两种写法 ⇒ 去重必须按去掉尾 '-' 的键
    p["prefixes"].append({"prefix": "fd-memory-", "line": "记忆 fd-memory",
                          "why": "自证：与上一条重复", "covers": ["fd-memory-bulk"]})


def family_is_alias_seg(p):
    p["prefix_families"].append({"seg": "wanxing", "why": "自证：这应当被拒"})


def sql_shape():
    t = (ROOT / "tools" / "line-quota.sql").read_text(encoding="utf-8")
    return ("= pa.prefix" in t) and ("pa.prefix || '-'" in t) and ("length(rtrim(pa.prefix, '-'))" in t)


def main():
    shutil.copy2(JSON, BAK)
    try:
        results = [
            posit("⓪ 不带尾 '-' 的前缀被接受（键名恰好等于服务名这一档）",
                  no_dash_ok, sql_shape),
            arm("① 不带尾 '-' 的前缀拼错一个字 ⇒ 边界不成立",
                no_dash_typo, "不满足该前缀的连字符边界"),
            arm("② 带尾 '-' 的前缀拼错一个字 ⇒ 匹配不到任何键",
                dash_typo, "不满足该前缀的连字符边界"),
            arm("③ 缺 covers", no_covers, "缺 covers"),
            arm("④ covers 里的键名不在该前缀的边界内",
                covers_outside, "不满足该前缀的连字符边界"),
            arm("⑤ prefix 只有首段 '<seg>-'（那是首段规则，应挪进 aliases）",
                bare_family, "只有首段"),
            arm("⑥ prefix 恰好等于族首段 '<seg>'（同上）",
                bare_seg, "只有首段"),
            arm("⑦ 同一条规则的两种写法（'fd-memory' 与 'fd-memory-'）",
                dup_after_normalize, "prefixes 重复"),
            arm("⑧ 族前缀本身已是别名 seg（两条路并存）",
                family_is_alias_seg, "已经是别名 seg"),
        ]
    finally:
        shutil.copy2(BAK, JSON)
        BAK.unlink()
    # 正证那一臂写了生成件（前缀去掉了尾 '-'），所以还原真源后必须重生成再 check
    run_gen()
    r = subprocess.run(["node", "tools/gen-line-aliases.mjs", "--check"], cwd=ROOT,
                       capture_output=True, text=True)
    green = r.returncode == 0
    print(f"[{'✓' if green else '✗'}] 还原真源并重生成后 --check 恢复绿  rc={r.returncode}")
    print(f"臂数={len(results)} 红证={sum(1 for ok in results if ok)} 全过={all(results) and green}")
    return 0 if all(results) and green else 1


sys.exit(main())
