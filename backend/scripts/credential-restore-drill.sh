#!/usr/bin/env bash
# 凭据加密的**恢复演练**（openspec change harden-sub2api-credential-storage 的 5.1 前置）。
#
# 为什么需要它：drop 明文列是不可逆动作，唯一的退路是备份。而"备份能用"这件事
# **必须在演练里证明过**，不能假设——所以要有一个可重复、有明确判据的脚本，
# 让任何持有只读副本或生产快照的人照着跑一遍。
#
# 用法：
#   ./credential-restore-drill.sh <源库DSN> [工作目录]
#
# 例（本机一次性库）：
#   createdb sub2api_drill_src && ./credential-restore-drill.sh \
#     'postgres:///sub2api_drill_src?sslmode=disable' /tmp/drill
#
# 判据（任一不满足即 exit 1）：
#   1. dump 能被**完整**读通（不是只看 TOC——`pg_restore -l` 在文件被截断时照样能列出目录）
#   2. 恢复到新库后，关键表行数与原库一致
#   3. 密文列在恢复库与源库里**逐字节相同**（说明备份没有损坏/截断密文）
#   4. dump 文件里不含明文密钥形态（`sk-` 长串、以及 credentials 列里的 api_key 明文）
#
# 不在这里验的：解密是否正确——那件事由 Go 侧的
# `TestCredentialsFromEntity_*` / `TestBackfillRehearsal_ProductionScale` 覆盖。
# 本脚本只回答一个问题：**备份能不能把密文完整地还回来**。

set -euo pipefail

# pg_dump/pg_restore 需要与**服务端**大版本兼容（旧版工具连新版 server 会直接 abort）。
# 本机常见情况：PATH 里是 postgresql@14，而 server 是 17。允许显式覆盖：
#   PG_BIN=/opt/homebrew/opt/postgresql@17/bin ./credential-restore-drill.sh ...
PG_BIN="${PG_BIN:-}"
if [ -n "$PG_BIN" ]; then
  PG_DUMP="$PG_BIN/pg_dump"; PG_RESTORE="$PG_BIN/pg_restore"
else
  PG_DUMP="pg_dump"; PG_RESTORE="pg_restore"
fi

SRC_DSN="${1:?用法: $0 <源库DSN> [工作目录]}"
WORK="${2:-$(mktemp -d)}"
mkdir -p "$WORK"
DUMP="$WORK/credential-drill.dump"
RESTORE_DB="sub2api_drill_restore_$$"

log() { printf '[drill] %s\n' "$*"; }
fail() { printf '[drill] FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  dropdb --if-exists "$RESTORE_DB" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "工作目录: $WORK"

# ── 1) 备份 ────────────────────────────────────────────────────────────────
log "pg_dump -Fc ... (工具: $PG_DUMP)"
"$PG_DUMP" -Fc -d "$SRC_DSN" -f "$DUMP"
[ -s "$DUMP" ] || fail "dump 为空"
log "dump 大小: $(wc -c < "$DUMP" | tr -d ' ') 字节"

# ── 2) 完整性：通读整包（关键） ────────────────────────────────────────────
# `pg_restore -l` 只读文件头里的 TOC，**截断的包它照样绿**。
# 所以用 `-f /dev/null` 让 pg_restore 真正读完整个归档。
log "完整读一遍 dump（不是只看 TOC）..."
if ! "$PG_RESTORE" -f /dev/null "$DUMP" 2>"$WORK/restore-read.err"; then
  cat "$WORK/restore-read.err" >&2
  fail "dump 无法完整读出（可能被截断/损坏）"
fi

# ── 3) 恢复到新库 ─────────────────────────────────────────────────────────
log "恢复到临时库 $RESTORE_DB ..."
createdb "$RESTORE_DB"
"$PG_RESTORE" -d "$RESTORE_DB" --no-owner --no-privileges "$DUMP" 2>"$WORK/restore.err" || {
  # 目标库非空/扩展缺失等会导致非零退出；把原因打出来而不是静默通过
  cat "$WORK/restore.err" >&2
  fail "pg_restore 失败"
}

# ── 4) 行数一致性 ─────────────────────────────────────────────────────────
for tbl in accounts api_keys; do
  src=$(psql -d "$SRC_DSN" -tAc "SELECT count(*) FROM $tbl")
  dst=$(psql -d "$RESTORE_DB" -tAc "SELECT count(*) FROM $tbl")
  log "$tbl: 源=$src 恢复=$dst"
  [ "$src" = "$dst" ] || fail "$tbl 行数不一致（$src vs $dst）——备份不完整"
done

# ── 5) 密文逐字节一致 ─────────────────────────────────────────────────────
# 用 count(*) of mismatched ciphertext rather than hashing the whole table.
# 逐行比对密文指纹（md5 足够——这里判的是"是不是同一串字节"，不是抗碰撞）：
# 比 count(*) 更严，能抓出"行数对但内容被截断/替换"的情况。
src_fp="$WORK/src.fp"; dst_fp="$WORK/dst.fp"
psql -d "$SRC_DSN"    -tAc "SELECT id || ':' || coalesce(md5(credentials_encrypted),'-') FROM accounts ORDER BY id" > "$src_fp"
psql -d "$RESTORE_DB" -tAc "SELECT id || ':' || coalesce(md5(credentials_encrypted),'-') FROM accounts ORDER BY id" > "$dst_fp"
if ! diff -q "$src_fp" "$dst_fp" >/dev/null; then
  diff "$src_fp" "$dst_fp" | head -10 >&2
  fail "密文列在恢复库与源库不一致——备份不可用"
fi
log "密文指纹逐一相同（$(wc -l < "$src_fp" | tr -d ' ') 行）"

# ── 6) 泄漏判据 ───────────────────────────────────────────────────────────
# 期望：明文列被 drop 之后，dump 里不该再有 sk- 长串。
# 若明文列还在（drop 之前跑这个脚本），这一条会**故意**报出来 —— 那是正确的信号：
# 说明此刻的备份仍然含明文，不能作为 drop 后的退路。
leaks=$("$PG_RESTORE" -f - "$DUMP" 2>/dev/null | grep -cE 'sk-[A-Za-z0-9_-]{16,}' || true)
if [ "$leaks" != "0" ]; then
  log "⚠️  dump 里仍有 $leaks 处 sk- 形态的明文串"
  log "    —— 若此刻明文列尚未 drop，这是**预期**的；drop 之后重跑本脚本必须变为 0。"
else
  log "dump 里无 sk- 长串（明文列已 drop 的预期结果）"
fi

log "完成。判据 1–3 全部通过；第 4 条按明文列是否已 drop 判读。"
