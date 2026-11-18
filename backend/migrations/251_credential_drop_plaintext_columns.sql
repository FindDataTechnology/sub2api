-- 阶段 5.2（不可逆）：drop 凭据明文列。
--
--   accounts.credentials  -> 只剩 credentials_encrypted / _meta / _hash / _api_key_hash
--   api_keys.key          -> 只剩 key_encrypted / key_hash
--
-- ⚠️ 这是整个 change 里**唯一不可回滚的一步**。drop 之后回滚镜像也救不回来：
-- ent 的 SELECT 会列出 schema 里的全部字段，旧镜像一读账号就
-- `column "credentials" does not exist`（不是某条路径坏，是账号读写全挂）。
-- 唯一的退路是「从 5.1 验证过的备份恢复」，所以本迁移的**前置条件**是：
--   1. `scripts/credential-migration-status.sh <DSN>` 的 A/B/C/D 全 0；
--   2. 异地备份（sub2api-backup CronJob，每日 04:37 CST）存在且已验证
--      「能恢复 + 能解密抽查」（5.1 在真实备份上做过：accounts 49 行、api_keys 168 行全解）。
--
-- 顺序：新镜像（代码里已无任何一处引用这两列）先就绪，迁移在应用启动时执行。
-- 反过来（先 drop 再上代码）必然全站 503 —— 详见 stage4_ordering_test.go 的真库演示。
--
-- 刻意**不含**三张 `_acct*` 死表的 DROP：它们只存在于生产库（过去事故时手工
-- `CREATE TABLE AS` 出来的整行副本，任何全新 schema 里都没有），而且删之前
-- 必须先各做一次 `pg_dump -t <表>` 留档到 /opt/backups/sub2api/retired-tables/。
-- 那个动作属于 release 窗口里的手工步骤，不适合放进启动时自动执行的迁移。
--
-- 幂等：重复执行不报错（IF EXISTS）。

ALTER TABLE accounts DROP COLUMN IF EXISTS credentials;

ALTER TABLE api_keys DROP COLUMN IF EXISTS key;

COMMENT ON COLUMN accounts.credentials_encrypted IS
    '凭据 AES-256-GCM 密文（唯一权威副本）；迁移 251 之后 NULL = 该账号没有凭据';
COMMENT ON COLUMN api_keys.key_encrypted IS
    'API key AES-256-GCM 密文（唯一权威副本）；迁移 251 之后 NULL = 该行没有 key';
