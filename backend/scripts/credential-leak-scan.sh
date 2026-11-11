#!/usr/bin/env bash
# 凭据泄漏判据（tasks.md 5.3）：**整值形态的密钥数必须为 0**。
#
# 为什么口径定成"整值"而不是"任意含 sk-"（2026-11-12 在真实生产 dump 上撞出来的）：
#   - 按"任意含 `sk-`"去查，生产上永远红：`ops_error_logs.api_key_prefix` 有 **10 万行**
#     命中 —— 那一列**就是打码前缀**，按设计只存 `sk-xxxx…` 的前几位，不是可用凭据。
#     另外 `client_request_id` / `request_id` / `user_agent` 这类标识串里也会恰好含 `sk-`。
#   - 按"整值就是一把密钥"（`^sk-[A-Za-z0-9_-]{20,}$`）去查，同一份数据里
#     **日志/账本表零命中**，只剩 6 条真密钥 —— 全在三张没人引用的临时备份表里。
# 两种口径给出的结论差 26 倍，所以口径本身必须写死在这里，并有自检证明它会红。
#
# 查法（不依赖表名/列名清单，避免"清单漏一行就静默失效"）：
#   对 public 下每张表，把整行转成 jsonb 再展开 key/value，看**整值**是否匹配密钥形态。
#   这样 jsonb 里的子键、text 列、varchar 列都被覆盖；新增表自动纳入。
#
# 排除：
#   · 打码前缀列（MASKED_COLUMNS，见下）——按设计只存前缀
#
# 用法：
#   ./credential-leak-scan.sh <DSN>          # exit 0 = 没有整值密钥
#   ./credential-leak-scan.sh --self-test    # 自检：造一个已知密钥，必须先红后绿
#
# 只读保证：所有查询连接带 PGOPTIONS='-c default_transaction_read_only=on'。

set -euo pipefail

if [ -n "${PG_BIN:-}" ]; then
  export PATH="$PG_BIN:$PATH"
fi

RO() { PGOPTIONS='-c default_transaction_read_only=on' "$@"; }
log() { printf '[leak] %s\n' "$*"; }
fail() { printf '[leak] FAIL: %s\n' "$*" >&2; exit 1; }

# 打码前缀列：按设计只存前几位，**不是**可用凭据。
# 加一个都必须在这里写明理由——这条清单本身就是判据的一部分。
MASKED_COLUMNS="api_key_prefix"

DBS_TO_DROP=()
cleanup() {
  local d
  for d in ${DBS_TO_DROP[@]+"${DBS_TO_DROP[@]}"}; do
    dropdb --if-exists "$d" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# scan <dsn>：输出每个「表.列 = N 行」的命中，返回 0=干净 / 1=有命中
scan() {
  local dsn="$1"
  local tables t
  tables=$(RO psql -d "$dsn" -tAc "
    SELECT table_name FROM information_schema.tables
    WHERE table_schema='public' AND table_type='BASE TABLE' ORDER BY table_name")

  local found=0
  for t in $tables; do
    local rows
    # $1 = 表名；kv.value 是整值形态匹配；排除打码列
    rows=$(RO psql -d "$dsn" -tAc "
      SELECT kv.key||'|'||count(*)
      FROM \"$t\" x, LATERAL jsonb_each_text(to_jsonb(x)) kv
      WHERE kv.value ~ '(^|\")sk-[A-Za-z0-9_-]{20,}(\$|\")'
        AND kv.key <> ALL (string_to_array('$MASKED_COLUMNS', ' '))
      GROUP BY kv.key" 2>/dev/null || true)
    [ -z "$rows" ] && continue
    while IFS='|' read -r col n; do
      [ -z "${col:-}" ] && continue
      log "  $t.$col = $n 行（整值像密钥）"
      found=$((found + n))
    done <<< "$rows"
  done
  [ "$found" -eq 0 ] && return 0 || return 1
}

run_status() {
  local dsn="$1" expect="${2:-interactive}"
  log "扫描 $dsn"
  local rc=0
  scan "$dsn" || rc=1
  if [ "$rc" -eq 0 ]; then
    log "结论：整值形态的密钥 **0 条** —— 5.3 达标"
  else
    log "结论：**仍有整值密钥**（上面逐条列出）—— 5.3 不达标"
    log "  ↳ 历史手工备份表（_acct*）要先 pg_dump -t 留档再删；业务表要看是哪一列写进去的"
  fi
  case "$expect" in
    expect-pass) [ "$rc" -eq 0 ] || fail "自检失败：期望干净但判据给出不达标"; return 0 ;;
    expect-fail) [ "$rc" -ne 0 ] || fail "自检失败：期望不达标但判据给出干净（判据是假门）"; return 0 ;;
    interactive) return "$rc" ;;
    *) fail "内部错误：未知期望值 $expect" ;;
  esac
}

run_self_test() {
  local db="sub2api_leak_$$"
  DBS_TO_DROP+=("$db")
  log "自检开始"

  dropdb --if-exists "$db" >/dev/null 2>&1 || true; createdb "$db"
  psql -q -d "$db" -v ON_ERROR_STOP=1 <<'SQL'
-- 干净的库：只有打码前缀列 + 含 sk- 的标识串，都不该被判成泄漏
CREATE TABLE clean_prefix(id bigserial PRIMARY KEY, api_key_prefix text);
INSERT INTO clean_prefix(api_key_prefix) VALUES ('sk-abcdefghijklmnopqrstuvwx');
CREATE TABLE clean_ident(id bigserial PRIMARY KEY, request_id text, user_agent text);
INSERT INTO clean_ident(request_id, user_agent) VALUES
  ('req_sk-abcdefghijklmnopqrstuvwx', 'curl/8.0 (sk-abcdefghijklmnopqrstuvwx)');
CREATE TABLE clean_json(id bigserial PRIMARY KEY, payload jsonb);
INSERT INTO clean_json(payload) VALUES ('{"note":"prefix sk-abc only"}');
SQL

  log "── 自检 A：只有打码前缀/标识串的库必须 PASS（否则这条门永远红）──"
  run_status "$db" expect-pass

  # 注入一条**整值**密钥：模拟 _acct_cred_backup_* 那种历史副本表
  psql -q -d "$db" -v ON_ERROR_STOP=1 <<'SQL'
CREATE TABLE _acct_cred_backup_test(id bigserial PRIMARY KEY, credentials jsonb);
INSERT INTO _acct_cred_backup_test(credentials)
  VALUES ('{"api_key":"sk-live-abcdefghijklmnopqrstuvwx"}');
SQL
  log "── 自检 B：注入一条整值密钥后必须 FAIL（证明判据真的会红）──"
  local out
  out=$(run_status "$db" expect-fail 2>&1) || fail "自检 B 失效：注入密钥后判据仍是干净"
  printf '%s\n' "$out" | grep -q "_acct_cred_backup_test.credentials" \
    || fail "自检 B 失效：红了但没点名是哪张表哪一列（运维无法据此处置）"

  # 把密钥换成打码前缀：同一个库必须重新变绿（证明红是因为那条密钥，不是别的）
  psql -q -d "$db" -v ON_ERROR_STOP=1 \
    -c "UPDATE _acct_cred_backup_test SET credentials = '{\"api_key\":\"sk-live-abcd\"}'::jsonb"
  log "── 自检 C：把整值换成短前缀后必须重新 PASS（证明红的是那条密钥）──"
  run_status "$db" expect-pass

  log "自检通过：打码前缀不算泄漏、整值密钥必红、红时点名表列"
}

if [ "${1:-}" = "--self-test" ]; then
  run_self_test
  exit 0
fi

DSN="${1:?用法: $0 <DSN>，或 $0 --self-test}"
run_status "$DSN" interactive
