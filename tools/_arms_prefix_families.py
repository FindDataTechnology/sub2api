#!/usr/bin/env python3
"""逐条红证：五条校验各自必须真的拒一次，且拒绝时不得写任何生成件。"""
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


def arm(name, mutate, expect):
    base = json.loads(BAK.read_text(encoding="utf-8"))
    mutate(base)
    JSON.write_text(json.dumps(base, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    before = {p.name: sha(p) for p in GEN}
    r = subprocess.run(["node", "tools/gen-line-aliases.mjs"], cwd=ROOT,
                       capture_output=True, text=True)
    after = {p.name: sha(p) for p in GEN}
    out = (r.stdout or "") + (r.stderr or "")
    ok = r.returncode != 0 and expect in out and before == after
    print(f"[{'✓' if ok else '✗'}] {name}  rc={r.returncode} 说中了={expect in out} 未写文件={before == after}")
    if expect not in out:
        print("    实际输出：", out.strip().splitlines()[-1] if out.strip() else "(空)")
    return ok


def no_dash(p):
    p["prefixes"][0]["prefix"] = "fd-text-core"


def typo_prefix(p):
    # 中间段拼错一个字：族仍是 fd，规则将匹配不到任何键 —— 靠 covers 与前缀不一致来抓
    p["prefixes"][0]["prefix"] = "fd-tex-core-"


def no_covers(p):
    p["prefixes"][0].pop("covers", None)


def bare_family(p):
    p["prefixes"][0]["prefix"] = "fd-"
    p["prefixes"][0]["covers"] = ["fd-anything"]


def family_is_alias_seg(p):
    p["prefix_families"].append({"seg": "wanxing", "why": "自证：这应当被拒"})


def main():
    shutil.copy2(JSON, BAK)
    try:
        results = [
            arm("① prefix 不以 '-' 结尾", no_dash, "必须以 '-' 结尾"),
            arm("② 前缀拼错一个字（匹配不到任何键）", typo_prefix, "不以该前缀开头"),
            arm("③ 缺 covers", no_covers, "缺 covers"),
            arm("④ prefix 只有首段（那是首段规则，应挪进 aliases）", bare_family, "只有首段"),
            arm("⑤ 族前缀本身已是别名 seg（两条路并存）", family_is_alias_seg, "已经是别名 seg"),
        ]
    finally:
        shutil.copy2(BAK, JSON)
        BAK.unlink()
    # 上面第 ② 条臂把生成件写成了拼错前缀的版本，所以还原真源后必须重生成再 check
    subprocess.run(["node", "tools/gen-line-aliases.mjs"], cwd=ROOT, capture_output=True, text=True)
    r = subprocess.run(["node", "tools/gen-line-aliases.mjs", "--check"], cwd=ROOT,
                       capture_output=True, text=True)
    green = r.returncode == 0
    print(f"[{'✓' if green else '✗'}] 还原真源并重生成后 --check 恢复绿  rc={r.returncode}")
    print(f"臂数={len(results)} 全过={all(results) and green}")
    return 0 if all(results) and green else 1


sys.exit(main())
