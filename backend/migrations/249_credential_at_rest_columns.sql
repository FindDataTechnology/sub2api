-- 凭据落盘加密：阶段一（加列 + 双写 + 边界解密）。
-- 见 openspec/changes/harden-sub2api-credential-storage/design.md D1 / D7。
--
-- 本迁移只加列，全部可空或带默认值 —— 回滚方式就是删掉这几列，行为回到改动前。
-- 明文列 accounts.credentials 与 api_keys.key 在本阶段保持完整可读，不删不改。

-- ---------------------------------------------------------------------------
-- accounts
-- ---------------------------------------------------------------------------

-- 非密钥子键的明文副本，供 SQL 侧筛选。白名单只有 base_url 与 account_mode
-- （生产 SQL 只提取这两个非密钥子键）；密钥子键绝不进这一列。
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_meta JSONB NOT NULL DEFAULT '{}'::jsonb;

-- 整份 credentials 明文的 AES-256-GCM 密文（base64(nonce+ct+tag)）。
-- NULL = 该行尚未迁移，读路径回落到明文列并打 credential_plaintext_fallback 计数。
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_encrypted TEXT;

-- sha256(规范化明文 JSON)。存在的唯一理由是乐观并发比较：AES-GCM 带随机 nonce，
-- 同一明文两次加密的密文不同，所以 `credentials = $1::jsonb` /
-- `IS DISTINCT FROM` 这类整文档等值比较必须改比这个哈希。
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_hash VARCHAR(64);

-- sha256(api_key)，供"按 api_key 找账号"（ollama / opencode 用量车道）。
-- 列变密文后 `credentials ->> 'api_key'` 会静默返回 NULL（不报错），
-- 这类查询必须走这个指纹列。
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credentials_api_key_hash VARCHAR(64);

-- 部分索引：只为已迁移的行建索引，避免历史 NULL 行占位。
CREATE INDEX IF NOT EXISTS idx_accounts_credentials_api_key_hash
    ON accounts (credentials_api_key_hash)
    WHERE credentials_api_key_hash IS NOT NULL;

COMMENT ON COLUMN accounts.credentials_meta IS
    '凭据非密钥子键明文副本，白名单={base_url,account_mode}，供 SQL 筛选';
COMMENT ON COLUMN accounts.credentials_encrypted IS
    'credentials 的 AES-256-GCM 密文；NULL=未迁移，读取回落明文列';
COMMENT ON COLUMN accounts.credentials_hash IS
    'sha256(规范化明文)，供乐观并发比较（密文非确定性，不能直接比）';
COMMENT ON COLUMN accounts.credentials_api_key_hash IS
    'sha256(api_key)，供按 api_key 找账号；替代 credentials ->> api_key';

-- ---------------------------------------------------------------------------
-- api_keys
-- ---------------------------------------------------------------------------

-- sha256(key)，用于"拿 key 找行"而不必解密。可空 + 唯一：
-- Postgres 唯一索引允许多个 NULL，所以迁移完成前全部 NULL 不冲突。
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_hash VARCHAR(64);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS key_encrypted TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_hash
    ON api_keys (key_hash)
    WHERE key_hash IS NOT NULL;

COMMENT ON COLUMN api_keys.key_hash IS
    'sha256(key)，鉴权查找键；替代明文 key 等值查询';
COMMENT ON COLUMN api_keys.key_encrypted IS
    'key 的 AES-256-GCM 密文；NULL=未迁移，读取回落明文列';
