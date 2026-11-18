-- 词元 rerank 车道每请求价格（fork 缝点，ADR-0024 / tasks 3.1）。
-- nil=未配置：按 0 计费并告警；开放车道前须在 admin 配置。
ALTER TABLE groups ADD COLUMN IF NOT EXISTS rerank_price_per_request DECIMAL(20,8);

COMMENT ON COLUMN groups.rerank_price_per_request IS
    '词元 rerank 车道每请求价格（USD/次）；nil=未配置按 0 计费并告警';
