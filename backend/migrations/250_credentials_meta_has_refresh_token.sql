-- 站点 A：把"凭据里有非空 refresh_token"作为派生事实写进 credentials_meta。
--
-- 无 DDL（列已存在）；幂等，可重复执行；**不动明文列**，所以回滚无损。
-- 语义与代码里的 credentialHasRefreshToken 逐字一致：BTRIM 去首尾空白后非空即"有"。
--
-- 上线顺序（反了是静默漏刷新，不报错）：
--   先跑本迁移 -> 过闸门（待补 key = 0）-> 再部署读该 key 的代码。
--   反了的话 `credentials_meta @> '{"has_refresh_token": true}'` 对全部存量行为假，
--   刷新候选集变空，所有 OAuth 账号的 access_token 到期后开始 401。

UPDATE accounts
SET credentials_meta = credentials_meta || jsonb_build_object(
        'has_refresh_token',
        COALESCE(NULLIF(BTRIM(credentials ->> 'refresh_token'), '') IS NOT NULL, false))
WHERE NOT (credentials_meta ? 'has_refresh_token');

COMMENT ON COLUMN accounts.credentials_meta IS
    '凭据非密钥子键明文副本 + 派生事实；复制白名单={base_url,account_mode}，事实={has_refresh_token}';
