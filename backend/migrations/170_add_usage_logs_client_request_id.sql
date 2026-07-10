-- 为 usage_logs 增加端到端关联键 client_request_id（issue #60）。
--
-- 级联部署下（linkyrouter → crs15 → 官方/渠道），同一请求在各级实例间通过
-- X-Client-Request-ID 串联；此列把该关联键落库到用量明细，便于按其检索计费请求。
-- 历史行为 NULL；索引采用部分索引（WHERE client_request_id IS NOT NULL），
-- 与 group_id 部分索引的先例一致，避免为全表历史 NULL 行浪费空间。
--
-- 幂等：ADD COLUMN IF NOT EXISTS + CREATE INDEX IF NOT EXISTS。

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS client_request_id VARCHAR(64);

CREATE INDEX IF NOT EXISTS idx_usage_logs_client_request_id
    ON usage_logs (client_request_id)
    WHERE client_request_id IS NOT NULL;

COMMENT ON COLUMN usage_logs.client_request_id IS '端到端关联键：跨级联实例贯穿同一请求链路（issue #60）；NULL 为历史行或未参与关联的请求';
