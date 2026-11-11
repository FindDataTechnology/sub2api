#!/usr/bin/env bash
# 凭据加密迁移的**只读状态盘**（对应 tasks.md 3.2 / 3.3 / 5.1 的判据）。
#
# 为什么需要它：那几关的判据散在文档里（六个 SQL、几条例外、谁先谁后），
# 上线窗口里靠人眼去数，最容易出的错不是算错而是**数错行**——
# 尤其"空 key 例外"这种，忘了加 `key <> ''` 就会把一个健康的库判成"没迁完"，
# 把发布门堵死（这个 bug 在回填工具里真实发生过一次）。
#
# 用法：
#   ./credential-migration-status.sh <DSN>     # 只读体检，exit 0=可以往下走
#   ./credential-migration-status.sh --self-test
#
# 只读保证不是靠"我只写了 SELECT"，而是每个连接都带
#   PGOPTIONS='-c default_transaction_read_only=on'
# 所以即便脚本被改坏，也写不进任何东西（自检里有一条专门验这件事）。
#
# 判据（全部为 0 才算 PASS；口径与回填工具**一字不差**）：
#   A accounts 待回填   (credentials_encrypted IS NULL OR credentials_hash IS NULL)
#   B api_keys 待回填   (key <> '' AND (key_encrypted IS NULL OR key_hash IS NULL))
#                       ↑ 空 key 例外：空串本来就没有派生值，算进去会让计数永不为 0
#   C has_refresh_token 缺失（迁移 250 未跑过）
#   D api_keys 未软删但缺指纹（3.5b 之后按指纹查，缺指纹=查不到）
#
# 只报告不判定：
#   E 明文里仍带 `sk-` 形态的行数（drop 之前**非零是正常的**；drop 之后这两列不存在）
#   F 明文列是否还在（用来判断当前处于哪个阶段）

set -euo pipefail

if [ -n "${PG_BIN:-}" ]; then
  export PATH="$PG_BIN:$PATH"
fi

# 只读：让服务端拒绝本会话里的任何写操作。
# 只包住**状态查询**，不能 export 到全局 —— 否则自检里造夹具用的 createdb/dropdb
# 也会被拒（实测：cannot execute CREATE DATABASE in a read-only transaction）。
RO() { PGOPTIONS='-c default_transaction_read_only=on' "$@"; }

log() { printf '[status] %s\n' "$*"; }
fail() { printf '[status] FAIL: %s\n' "$*" >&2; exit 1; }

DBS_TO_DROP=()
cleanup() {
  local d
  for d in ${DBS_TO_DROP[@]+"${DBS_TO_DROP[@]}"}; do
    dropdb --if-exists "$d" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# q <dsn> <sql>（只读会话）
q() { RO psql -d "$1" -tAc "$2" | tr -d " "; }

# col_exists <dsn> <table> <column> → yes/no
col_exists() {
  local n
  n=$(q "$3" "SELECT count(*) FROM information_schema.columns WHERE table_name = '$1' AND column_name = '$2'")
  [ "$n" != "0" ] && printf 'yes' || printf 'no'
}

# run_status <dsn> [expect] ；expect ∈ interactive | expect-pass | expect-fail
run_status() {
  local dsn="$1" expect="${2:-interactive}"
  local rc=0

  local has_plaintext
  has_plaintext=$(col_exists accounts credentials "$dsn")
  local has_key_col
  has_key_col=$(col_exists api_keys key "$dsn")

  local acc_pending meta_missing
  acc_pending=$(q "$dsn" "SELECT count(*) FROM accounts WHERE (credentials_encrypted IS NULL OR credentials_hash IS NULL)")
  meta_missing=$(q "$dsn" "SELECT count(*) FROM accounts WHERE NOT (credentials_meta ? 'has_refresh_token')")

  log "A accounts 待回填                 : $acc_pending   (要求 0)"
  [ "$acc_pending" = "0" ] || { log "  ↳ 跑 cmd/credential-backfill -apply"; rc=1; }
  log "C 缺 has_refresh_token 的账号     : $meta_missing  (要求 0)"
  [ "$meta_missing" = "0" ] || { log "  ↳ 迁移 250 未跑过或只跑了一半"; rc=1; }

  # B/D 都依赖 `key` 列来区分"空 key（合法终态）"与"没回填（坏了）"。
  # drop 之后这两列不存在，**无法再区分**这两件事——所以不能继续用它们当门：
  # 一条空 key 行在 drop 后与一条坏行长得一模一样。
  if [ "$has_key_col" = "yes" ]; then
    local api_pending key_no_hash
    api_pending=$(q "$dsn" "SELECT count(*) FROM api_keys WHERE (key <> '' AND (key_encrypted IS NULL OR key_hash IS NULL))")
    # D 也必须带空 key 例外 —— 否则库里只要有一行空 key（合法终态），这项永远 > 0，
    # 脚本就永远报"不可往下走"。这正是回填工具踩过的 bug，自检 A 档当场抓出来了。
    key_no_hash=$(q "$dsn" "SELECT count(*) FROM api_keys WHERE key_hash IS NULL AND key <> '' AND deleted_at IS NULL")
    log "B api_keys 待回填（空 key 除外）  : $api_pending   (要求 0)"
    [ "$api_pending" = "0" ] || { log "  ↳ 同上（回填工具会一起处理 api_keys）"; rc=1; }
    log "D 未软删但缺指纹的 key            : $key_no_hash   (要求 0)"
    [ "$key_no_hash" = "0" ] || { log "  ↳ 有未软删的 key 没有指纹：3.5b 之后这些 key 认证不到"; rc=1; }
  else
    local hash_null
    hash_null=$(q "$dsn" "SELECT count(*) FROM api_keys WHERE key_hash IS NULL AND deleted_at IS NULL")
    log "B api_keys 待回填                 : 跳过（key 列已 drop，无法区分空 key 与漏回填）"
    log "D 未软删但缺指纹的 key            : $hash_null 行（**仅供参考**：含合法的空 key 行）"
  fi

  if [ "$has_plaintext" = "yes" ]; then
    local leak_acc leak_keys
    leak_acc=$(q "$dsn" "SELECT count(*) FROM accounts WHERE credentials ? 'api_key' AND credentials ->> 'api_key' LIKE 'sk-%'")
    leak_keys=$(q "$dsn" "SELECT count(*) FROM api_keys WHERE key LIKE 'sk-%'")
    log "E 明文里的 sk- 形态             : accounts=$leak_acc api_keys=$leak_keys"
    log "  ↳ drop 之前**非零是正常的**；drop 之后本节应显示「明文列已不存在」"
    log "F 明文列                        : 仍在（阶段二/三）"
  else
    log "E 明文里的 sk- 形态             : 跳过（credentials 列已不存在）"
    log "F 明文列                        : 已 drop（阶段四完成）"
  fi

  if [ "$rc" -eq 0 ]; then
    log "结论：判据全为 0 —— 可以往下走下一步"
  else
    log "结论：**不可往下走**（上面 ↳ 行是要做的事）"
  fi

  # 期望档位只是自检用。注意 expect-fail 断言成功后必须 return 0：
  # 直接把 rc（=1）返回出去，会在 `set -e` 下把调用方整个带走 ——
  # 实测症状是"B 档打印完就没了，C 档一行都没跑"。
  case "$expect" in
    expect-pass) [ "$rc" -eq 0 ] || fail "自检失败：期望 PASS 但判据给出 FAIL"; return 0 ;;
    expect-fail) [ "$rc" -ne 0 ] || fail "自检失败：期望 FAIL 但判据给出 PASS（判据是假门）"; return 0 ;;
    interactive) return "$rc" ;;
    *) fail "内部错误：未知期望值 $expect" ;;
  esac
}

# ── 自检 ────────────────────────────────────────────────────────────────────
# 三档状态：干净（含空 key 例外）/ 脏 / 已 drop。三档只差一个事实，
# 所以"档与档的结论不同"本身就能证明判据在分辨东西。

seed_clean() {
  local db="$1"
  dropdb --if-exists "$db" >/dev/null 2>&1 || true; createdb "$db"
  psql -q -d "$db" -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE accounts(id bigserial PRIMARY KEY, credentials jsonb, credentials_meta jsonb,
  credentials_hash varchar(64), credentials_encrypted text);
CREATE TABLE api_keys(id bigserial PRIMARY KEY, key varchar(128) NOT NULL, key_hash varchar(64),
  key_encrypted text, deleted_at timestamptz);
INSERT INTO accounts(credentials, credentials_meta, credentials_hash, credentials_encrypted) VALUES
 ('{"api_key":"sk-live-1"}','{"has_refresh_token":false}','h1','ct1'),
 ('{"refresh_token":"r"}','{"has_refresh_token":true}','h2','ct2');
-- 空 key 行：本来就没有派生值，**不算待回填**（这条就是判据里那个例外）
INSERT INTO api_keys(key, key_hash, key_encrypted) VALUES ('sk-live-1','k1','ct1'), ('', NULL, NULL);
SQL
}

seed_dirty() {
  local db="$1"
  dropdb --if-exists "$db" >/dev/null 2>&1 || true; createdb "$db"
  psql -q -d "$db" -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE accounts(id bigserial PRIMARY KEY, credentials jsonb, credentials_meta jsonb,
  credentials_hash varchar(64), credentials_encrypted text);
CREATE TABLE api_keys(id bigserial PRIMARY KEY, key varchar(128) NOT NULL, key_hash varchar(64),
  key_encrypted text, deleted_at timestamptz);
INSERT INTO accounts(credentials, credentials_meta, credentials_hash, credentials_encrypted) VALUES
 ('{"api_key":"sk-live-1"}','{"has_refresh_token":false}','h1','ct1'),
 ('{"api_key":"sk-live-2"}','{}',NULL,NULL);          -- 缺 hash+密文、缺 meta 键
INSERT INTO api_keys(key, key_hash, key_encrypted) VALUES
 ('sk-live-1','k1','ct1'),
 ('sk-live-2',NULL,NULL);                              -- 有 key 但没指纹、没密文
SQL
}

seed_postdrop() {
  local db="$1"
  dropdb --if-exists "$db" >/dev/null 2>&1 || true; createdb "$db"
  psql -q -d "$db" -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE accounts(id bigserial PRIMARY KEY, credentials_meta jsonb,
  credentials_hash varchar(64), credentials_encrypted text);
CREATE TABLE api_keys(id bigserial PRIMARY KEY, key_hash varchar(64), key_encrypted text,
  deleted_at timestamptz);
INSERT INTO accounts(credentials_meta, credentials_hash, credentials_encrypted) VALUES
 ('{"has_refresh_token":true}','h1','ct1');
INSERT INTO api_keys(key_hash, key_encrypted) VALUES ('k1','ct1');
SQL
}

# seed_single <db> <which>：在"其余全干净"的库上只注入**一个**缺陷。
# 用途是逐项证明每个检查各自在场、且报的是它自己的补救说明
# —— 四项一起非零无法排除"某一行接错了数据源"。
seed_single() {
  local db="$1" which="$2"
  dropdb --if-exists "$db" >/dev/null 2>&1 || true; createdb "$db"
  psql -q -d "$db" -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE accounts(id bigserial PRIMARY KEY, credentials jsonb, credentials_meta jsonb,
  credentials_hash varchar(64), credentials_encrypted text);
CREATE TABLE api_keys(id bigserial PRIMARY KEY, key varchar(128) NOT NULL, key_hash varchar(64),
  key_encrypted text, deleted_at timestamptz);
INSERT INTO accounts(credentials, credentials_meta, credentials_hash, credentials_encrypted) VALUES
 ('{"api_key":"sk-1"}','{"has_refresh_token":false}','h1','ct1'),
 ('{"api_key":"sk-2"}','{"has_refresh_token":false}','h2','ct2');
INSERT INTO api_keys(key, key_hash, key_encrypted) VALUES ('sk-1','k1','ct1');
SQL
  case "$which" in
    accounts_pending) psql -q -d "$db" -c "UPDATE accounts SET credentials_encrypted = NULL WHERE id = 2" ;;
    meta_missing)     psql -q -d "$db" -c "UPDATE accounts SET credentials_meta = '{}'::jsonb WHERE id = 2" ;;
    api_pending)      psql -q -d "$db" -c "INSERT INTO api_keys(key) VALUES ('sk-2')" ;;
    key_no_hash)      psql -q -d "$db" -c "INSERT INTO api_keys(key, key_encrypted) VALUES ('sk-3','ct3')" ;;
    *) echo "unknown seed_single mode: $which" >&2; return 1 ;;
  esac
}

run_self_test() {
  local clean="sub2api_status_clean_$$" dirty="sub2api_status_dirty_$$" post="sub2api_status_post_$$"
  DBS_TO_DROP+=("$clean" "$dirty" "$post")
  log "自检开始"

  # 只读保证：本会话里尝试写必须报错
  log "── 自检 0：只读会话必须拒绝写入 ──"
  if RO psql -d postgres -c "CREATE TABLE sub2api_readonly_probe(x int)" >/dev/null 2>&1; then
    psql -d postgres -c "DROP TABLE sub2api_readonly_probe" >/dev/null 2>&1 || true
    fail "自检 0 失效：PGOPTIONS 没让会话变成只读 —— 这个脚本的只读保证是假的"
  fi
  log "       写入被拒绝（只读保证有效）"

  seed_clean "$clean"; seed_dirty "$dirty"; seed_postdrop "$post"

  log "── 自检 A：干净的库（含空 key 行）必须 PASS ──"
  run_status "$clean" expect-pass

  log "── 自检 B：脏库必须 FAIL ──"
  run_status "$dirty" expect-fail

  log "── 自检 C：已 drop 的库必须 PASS，且不能因列不存在而报错 ──"
  run_status "$post" expect-pass

  # 逐项：只注入一个缺陷，断言"判据变 FAIL **且** 报的是这一项的补救说明"。
  # 只断言 FAIL 不够 —— 四项一起非零时，某一行接错数据源也看不出来。
  log "── 自检 D：逐项单独注入一个缺陷，必须各自点名 ──"
  local case_db="sub2api_status_one_$$"
  DBS_TO_DROP+=("$case_db")
  local which needle
  for pair in \
      "accounts_pending|跑 cmd/credential-backfill -apply" \
      "meta_missing|迁移 250 未跑过或只跑了一半" \
      "api_pending|回填工具会一起处理 api_keys" \
      "key_no_hash|3.5b 之后这些 key 认证不到"; do
    which="${pair%%|*}"; needle="${pair#*|}"
    seed_single "$case_db" "$which"
    out=$(run_status "$case_db" expect-fail 2>&1) || fail "自检 D 失效：$which 没让判据变 FAIL"
    printf '%s\n' "$out" | grep -q "$needle" \
      || fail "自检 D 失效：$which 触发了，但没打出它自己的补救说明（检查项串了）"
    log "       $which 单独注入 → 按预期点名"
  done

  log "自检全部通过：三档结论互不相同，四项检查各自点名，只读保证有效"
}

# ── 入口 ────────────────────────────────────────────────────────────────────

if [ "${1:-}" = "--self-test" ]; then
  run_self_test
  exit 0
fi

DSN="${1:?用法: $0 <DSN>，或 $0 --self-test}"
run_status "$DSN" interactive
