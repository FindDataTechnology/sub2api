#!/usr/bin/env bash
# 凭据加密的**恢复演练**（openspec change harden-sub2api-credential-storage 的 5.1 前置）。
#
# 为什么需要它：drop 明文列是不可逆动作，唯一的退路是备份。而"备份能用"这件事
# **必须在演练里证明过**，不能假设——所以要有一个可重复、有明确判据的脚本，
# 让任何持有只读副本或生产快照的人照着跑一遍。
#
# 用法：
#   ./credential-restore-drill.sh <源库DSN> [工作目录]    # 真跑一次演练
#   ./credential-restore-drill.sh --self-test             # 自检：证明三条判据不是假门
#
# 例（本机一次性库）：
#   createdb sub2api_drill_src && ./credential-restore-drill.sh \
#     'postgres:///sub2api_drill_src?sslmode=disable' /tmp/drill
#
# 判据（任一不满足即 exit 1）：
#   1. dump 能被**完整**读通（不是只看 TOC——`pg_restore -l` 在文件被截断时照样能列出目录）
#   2. 恢复到新库后，关键表行数与原库一致
#   3. 密文列在恢复库与源库里**逐字节相同**（说明备份没有损坏/截断密文）
#   4. dump 文件里不含明文密钥形态（`sk-` 长串）
#
# 不在这里验的：解密是否正确——那件事由 Go 侧的
# `TestCredentialsFromEntity_*` / `TestBackfillRehearsal_ProductionScale` 覆盖。
# 本脚本只回答一个问题：**备份能不能把密文完整地还回来**。
#
# 关于 `--self-test`：判据写出来不等于判据有用。`-l` 在截断上永远是绿的、泄漏计数
# 在 drop 前后必须会翻转、密文比对必须能被一处字节翻转打红——这三件事都靠
# `--self-test` 在本地一次性库上实测，每次都重新证明，而不是靠注释里的信念。

set -euo pipefail

# pg_dump/pg_restore 需要与**服务端**大版本兼容（旧版工具连新版 server 会直接 abort）。
# 本机常见情况：PATH 里是 postgresql@14，而 server 是 17。允许显式覆盖：
#   PG_BIN=/opt/homebrew/opt/postgresql@17/bin ./credential-restore-drill.sh ...
# 用 PATH 前置而不是只替换两个变量：createdb/dropdb/psql 也必须同版本，
# 否则会出现"pg_restore 17 把库建在 createdb 14 的手里"这种混搭。
if [ -n "${PG_BIN:-}" ]; then
  export PATH="$PG_BIN:$PATH"
fi
PG_DUMP="pg_dump"; PG_RESTORE="pg_restore"

log() { printf '[drill] %s\n' "$*"; }
fail() { printf '[drill] FAIL: %s\n' "$*" >&2; exit 1; }

DBS_TO_DROP=()
cleanup() {
  local d
  for d in ${DBS_TO_DROP[@]+"${DBS_TO_DROP[@]}"}; do
    dropdb --if-exists "$d" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# ── 判据实现 ────────────────────────────────────────────────────────────────

# 完整读一遍归档。`pg_restore -l` 只读文件头里的 TOC，**截断的包它照样绿**，
# 所以必须用 `-f /dev/null` 让 pg_restore 真正读完整个归档的全部数据块。
full_read_ok() {
  local dump="$1" errfile="$2"
  "$PG_RESTORE" -f /dev/null "$dump" >/dev/null 2>"$errfile"
}

# 逐行比对某一列的指纹（md5 足够——这里判的是"是不是同一串字节"，不是抗碰撞）。
# 比 count(*) 更严：能抓出"行数对但内容被截断/替换"的情况。
compare_column() {
  local src="$1" dst="$2" work="$3" table="$4" column="$5"
  # 工作目录必须自己建：重定向写进不存在的目录会返回非零，于是"写文件失败"会被
  # 静默读成"两库不一致"（我第一版自检就这么假绿过一次）。
  mkdir -p "$work"
  local src_fp="$work/src.$table.$column.fp" dst_fp="$work/dst.$table.$column.fp"
  psql -d "$src" -tAc "SELECT id || ':' || coalesce(md5($column),'-') FROM $table ORDER BY id" >"$src_fp"
  psql -d "$dst" -tAc "SELECT id || ':' || coalesce(md5($column),'-') FROM $table ORDER BY id" >"$dst_fp"
  diff -q "$src_fp" "$dst_fp" >/dev/null || { diff "$src_fp" "$dst_fp" | head -10 >&2; return 1; }
  printf '%s' "$(wc -l <"$src_fp" | tr -d ' ')"
}

# 判据 3：**两张表的密文列都要比**。
#
# 只比 accounts 是漏的：`api_keys.key_encrypted` 在 drop 之后同样是明文的唯一副本，
# 它被截断/替换照样会让"备份能用"这个结论变成假话。判据必须对准坏的那个本身，
# 而不是它旁边那个看起来更重要的列。
compare_ciphertext() {
  local src="$1" dst="$2" work="$3" rows
  rows=$(compare_column "$src" "$dst" "$work" accounts credentials_encrypted) || return 1
  printf 'accounts.credentials_encrypted %s 行；' "$rows"
  rows=$(compare_column "$src" "$dst" "$work" api_keys key_encrypted) || return 1
  printf 'api_keys.key_encrypted %s 行' "$rows"
}

# 泄漏计数（出现次数，不是行数）。
#
# 判据成立的前提：密文是 `base64.StdEncoding`（internal/repository/aes_encryptor.go:59），
# 字母表是 A-Za-z0-9+/=，**没有 `-`**，所以 `sk-` 不可能出现在密文里，
# 这条 grep 不会因为随机密文误报。
# 若将来改成 RawURLEncoding（含 `-` 与 `_`），必须同步改成带边界的形态
# （`[^A-Za-z0-9_-]sk-[A-Za-z0-9_-]{16,}`），否则判据会开始报假阳性。
leak_count() {
  local dump="$1"
  # `grep` 无匹配时退出码是 1，在 `set -e` 下会直接把脚本带走 —— 而"零泄漏"恰恰是
  # drop 之后**期望**的结果。所以必须吞掉这个退出码，只取计数。
  { "$PG_RESTORE" -f - "$dump" 2>/dev/null || true; } | grep -oE 'sk-[A-Za-z0-9_-]{16,}' | wc -l | tr -d ' ' || true
}

# ── 演练主体 ────────────────────────────────────────────────────────────────
# run_drill <源DSN> <工作目录> <期望> [恢复库名]
# 期望 ∈ interactive | expect-leaks | expect-clean
# 恢复库名由调用方给：自检会在子 shell 里跑"错期望"探针，库名若在函数内部生成，
# 子 shell 就登记不到父 shell 的清理表里，必然留下野库。
# 结束时导出 DRILL_DUMP 供自检复用。
run_drill() {
  local src="$1" work="$2" expect="$3"
  local restore_db="${4:-}"
  if [ -z "$restore_db" ]; then
    restore_db="sub2api_drill_restore_$$_$RANDOM"
    DBS_TO_DROP+=("$restore_db")
  fi
  mkdir -p "$work"
  local dump="$work/credential-drill.dump"
  DRILL_DUMP="$dump"

  log "工作目录: $work"

  # 1) 备份
  log "pg_dump -Fc ... (工具: $(command -v "$PG_DUMP"))"
  "$PG_DUMP" -Fc -d "$src" -f "$dump"
  [ -s "$dump" ] || fail "dump 为空"
  log "dump 大小: $(wc -c <"$dump" | tr -d ' ') 字节"

  # 2) 完整性：通读整包（关键）
  log "判据 1：完整读一遍 dump（不是只看 TOC）..."
  if ! full_read_ok "$dump" "$work/restore-read.err"; then
    cat "$work/restore-read.err" >&2
    fail "dump 无法完整读出（可能被截断/损坏）"
  fi
  log "       通过"

  # 3) 恢复到新库
  log "恢复并判据 2/3：恢复到临时库 $restore_db ..."
  createdb "$restore_db"
  "$PG_RESTORE" -d "$restore_db" --no-owner --no-privileges "$dump" 2>"$work/restore.err" || {
    cat "$work/restore.err" >&2
    fail "pg_restore 失败"
  }

  local tbl src_n dst_n
  for tbl in accounts api_keys; do
    src_n=$(psql -d "$src" -tAc "SELECT count(*) FROM $tbl")
    dst_n=$(psql -d "$restore_db" -tAc "SELECT count(*) FROM $tbl")
    log "       判据 2 $tbl: 源=$src_n 恢复=$dst_n"
    [ "$src_n" = "$dst_n" ] || fail "$tbl 行数不一致（$src_n vs ${dst_n}）——备份不完整"
  done

  local rows
  rows=$(compare_ciphertext "$src" "$restore_db" "$work") || fail "密文列在恢复库与源库不一致——备份不可用"
  log "       判据 3 密文指纹逐一相同（${rows}）"

  # 4) 泄漏判据
  local leaks
  leaks=$(leak_count "$dump")
  log "       判据 4 明文形态命中: $leaks 处"
  case "$expect" in
    expect-leaks)
      [ "$leaks" -gt 0 ] || fail "自检失败：源库含明文列，泄漏计数却是 0 —— 判据 4 是假门"
      log "       判据 4 按预期命中（明文列尚未 drop）"
      ;;
    expect-clean)
      [ "$leaks" -eq 0 ] || fail "明文列已 drop，dump 里仍有 $leaks 处 sk- 形态 —— 备份仍含明文，不可作为 drop 后退路"
      log "       判据 4 按预期为 0（明文列已 drop）"
      ;;
    interactive)
      if [ "$leaks" != "0" ]; then
        log "⚠️  dump 里仍有 $leaks 处 sk- 形态的明文串"
        log "    —— 若此刻明文列尚未 drop，这是**预期**的；drop 之后重跑本脚本必须变为 0。"
      else
        log "dump 里无 sk- 长串（明文列已 drop 的预期结果）"
      fi
      ;;
    *) fail "内部错误：未知期望值 $expect" ;;
  esac

  log "完成。判据 1–3 全部通过；判据 4 = ${leaks}。"
}

# ── 自检 ────────────────────────────────────────────────────────────────────
# 造两个一次性库：pre 带明文列、post 已 drop 明文列。同一份密文，只差明文列。
# 然后分别跑 run_drill，断言泄漏计数会翻转；再验两件判据本身能否打红。

seed_fixture() {
  local db="$1" with_plaintext="$2"
  dropdb --if-exists "$db" >/dev/null 2>&1 || true
  createdb "$db"
  # 固定密文（200 字符 base64，不含 `-`，模拟 AES-GCM 输出形态；内容无需可解）
  local ct1 ct2 ct3
  ct1="q83v$(printf 'Zm9vYmFy%.0s' {1..24})"
  ct2="7b1x$(printf 'cXV1eHF1dXg%.0s' {1..19})"
  ct3="0z9k$(printf 'bWl4ZWRzaXpl%.0s' {1..17})"

  if [ "$with_plaintext" = "yes" ]; then
    psql -q -d "$db" -v ON_ERROR_STOP=1 <<SQL
CREATE TABLE accounts(
  id bigserial PRIMARY KEY,
  name text NOT NULL DEFAULT '',
  credentials jsonb,
  credentials_meta jsonb,
  credentials_hash text,
  credentials_encrypted text
);
CREATE TABLE api_keys(
  id bigserial PRIMARY KEY,
  key text NOT NULL DEFAULT '',
  key_hash text,
  key_encrypted text,
  deleted_at timestamptz
);
INSERT INTO accounts(name, credentials, credentials_meta, credentials_hash, credentials_encrypted) VALUES
 ('acc-oauth','{"access_token":"sk-ant-oat01-PLAINTEXTPLAINTEXT0123456789","refresh_token":"gAAAA-plain-refresh-0123456789","base_url":"https://api.anthropic.example"}','{"base_url":"https://api.anthropic.example","has_refresh_token":true}','5f0c1d','${ct1}'),
 ('acc-openai','{"api_key":"sk-proj-PLAINTEXTPLAINTEXT9876543210","account_mode":"codex"}','{"account_mode":"codex","has_refresh_token":false}','9a3e2b','${ct2}'),
 ('acc-null',NULL,NULL,NULL,'${ct3}');
INSERT INTO api_keys(key, key_hash, key_encrypted) VALUES
 ('sk-live-PLAINTEXTPLAINTEXTabcdefghij','a1b2c3','${ct1}'),
 ('__deleted__sk-live-PLAINTEXTPLAINTEXTklmnopqrst','d4e5f6','${ct2}');
SQL
  else
    # 已 drop 明文列：结构与密文都和 pre 完全一致，只少了 credentials / key
    psql -q -d "$db" -v ON_ERROR_STOP=1 <<SQL
CREATE TABLE accounts(
  id bigserial PRIMARY KEY,
  name text NOT NULL DEFAULT '',
  credentials_meta jsonb,
  credentials_hash text,
  credentials_encrypted text
);
CREATE TABLE api_keys(
  id bigserial PRIMARY KEY,
  key_hash text,
  key_encrypted text,
  deleted_at timestamptz
);
INSERT INTO accounts(name, credentials_meta, credentials_hash, credentials_encrypted) VALUES
 ('acc-oauth','{"base_url":"https://api.anthropic.example","has_refresh_token":true}','5f0c1d','${ct1}'),
 ('acc-openai','{"account_mode":"codex","has_refresh_token":false}','9a3e2b','${ct2}'),
 ('acc-null',NULL,NULL,'${ct3}');
INSERT INTO api_keys(key_hash, key_encrypted) VALUES
 ('a1b2c3','${ct1}'),
 ('d4e5f6','${ct2}');
SQL
  fi
}

run_self_test() {
  local mismatch_db="sub2api_drill_mismatch_$$"
  local pre="sub2api_drill_fx_pre_$$" post="sub2api_drill_fx_post_$$" corrupt="sub2api_drill_fx_corrupt_$$"
  local a_db="sub2api_drill_A_$$" b_db="sub2api_drill_B_$$"
  local work
  work="$(mktemp -d)"
  # 显式给了库名就必须显式登记清理：write_drill 只在**自己生成**名字时才登记，
  # 漏登记的结果是演练跑完留下野库（第一次跑 E 探针时就这么漏了两个）。
  DBS_TO_DROP+=("$pre" "$post" "$corrupt" "$mismatch_db" "$a_db" "$b_db")
  log "自检开始（工作目录 ${work}）"

  seed_fixture "$pre" yes
  seed_fixture "$post" no

  log "── 自检 A：明文列尚在 → 判据 4 必须命中 ──"
  run_drill "$pre" "$work/pre" expect-leaks "$a_db"

  log "── 自检 B：明文列已 drop → 判据 4 必须为 0 ──"
  run_drill "$post" "$work/post" expect-clean "$b_db"

  # 期望值写反时必须失败。少了这一条，A/B "两档都通过"无法区分
  # "判据真的在判"与"判据是空的"——空断言同样能让两边都过。
  # 放在子 shell 里跑是因为 fail 会 exit 1，正好用来判退出码；
  # 库名由父 shell 给，否则子 shell 登记不进清理表，会留野库。
  log "── 自检 E：期望值写反时自检必须失败（证明 A/B 不是空断言）──"
  if ( run_drill "$post" "$work/mismatch" expect-leaks "$mismatch_db" ) >/dev/null 2>&1; then
    fail "自检 E 失效：明文已 drop 的库用 expect-leaks 竟然通过 —— 期望分支是空的"
  fi
  log "       错期望按预期失败"

  log "── 自检 C：截断包上，pg_restore -l 绿而完整读红 ──"
  local dump="$DRILL_DUMP" trunc="$work/truncated.dump"
  local size cut
  size=$(wc -c <"$dump" | tr -d ' '); cut=$((size * 99 / 100))
  head -c "$cut" "$dump" >"$trunc"
  local toc_rc=0 read_rc=0
  "$PG_RESTORE" -l "$trunc" >/dev/null 2>&1 || toc_rc=$?
  full_read_ok "$trunc" "$work/trunc.err" || read_rc=$?
  log "       只读 TOC rc=${toc_rc}，完整读 rc=${read_rc}（保留 $cut/$size 字节）"
  [ "$toc_rc" -eq 0 ] || fail "自检 C 前提不成立：-l 在截断包上报错了；这个版本的 TOC 或许不在文件头，注释需订正"
  [ "$read_rc" -ne 0 ] || fail "自检 C 失效：完整读**没**发现截断 —— 判据 1 是永远绿的假门"

  log "── 自检 D：把恢复库里一处密文翻一个字节 → 判据 3 必须打红（两列各验一次）──"
  local table column db
  for probe in "accounts credentials_encrypted" "api_keys key_encrypted"; do
    set -- $probe; table="$1"; column="$2"
    db="${corrupt}_${table}"
    DBS_TO_DROP+=("$db")
    createdb "$db"
    "$PG_RESTORE" -d "$db" --no-owner --no-privileges "$dump" >/dev/null 2>&1 || fail "自检 D 准备失败：无法恢复"
    # 先证明"没坏的时候比对是通过的"。少了这一步，一个**永远失败**的比对函数
    # 也能让下面的负例通过 —— 那样这条自检本身就是假绿的（我第一版就栽在这里：
    # 重定向写进不存在的目录 → 比对恒失败 → 负例"按预期打红"，其实什么都没验）。
    if ! compare_ciphertext "$post" "$db" "$work/intact.$table" >/dev/null 2>&1; then
      fail "自检 D 前提不成立：未损坏的恢复库上判据 3 就不通过，无法据此判断负例"
    fi
    psql -q -d "$db" -c "UPDATE $table SET $column = $column || 'X' WHERE id = 1" >/dev/null
    if compare_ciphertext "$post" "$db" "$work/corrupt.$table" >/dev/null 2>&1; then
      fail "自检 D 失效：$table.$column 已被改一个字节，判据 3 仍然通过 —— 这列是没被比对的假覆盖"
    fi
    log "       $table.${column}：未损坏时通过、改一字节后打红"
  done

  # 证明新增的 api_keys 覆盖不是装饰：在**上面那个 api_keys 已损坏的库**上，
  # 只比 accounts 仍然通过 —— 也就是说旧版判据会把这份坏备份判成"可用"。
  if ! compare_column "$post" "${corrupt}_api_keys" "$work/old-scope" accounts credentials_encrypted >/dev/null 2>&1; then
    fail "自检 D 前提不成立：该库的 accounts 列也不一致，无法证明新增覆盖是必要的"
  fi
  log "       旧版（只比 accounts）在同一个坏库上仍然通过 —— 新增的 api_keys 覆盖确实必要"

  log "自检全部通过：三条判据都能分辨好坏（不是永真条件）"
  log "临时库将在退出时清理：${DBS_TO_DROP[*]}"
}

# ── 入口 ────────────────────────────────────────────────────────────────────

if [ "${1:-}" = "--self-test" ]; then
  run_self_test
  exit 0
fi

SRC_DSN="${1:?用法: $0 <源库DSN> [工作目录]，或 $0 --self-test}"
WORK="${2:-$(mktemp -d)}"
run_drill "$SRC_DSN" "$WORK" interactive
