-- 计费补扣队列（issue #270）
--
-- 扣费事务因锁等待超时、连接抖动等原因失败时，不再按 0 放过，而是把已经算好的
-- UsageBillingCommand 原样落到这里，由后台定时任务重放（周期与最大次数可配）。重放走同一个
-- usage_billing_dedup 幂等键，即使原事务其实已经提交（提交应答丢失）也不会重复扣费。
--
-- 只保存待处理（pending）与放弃重试（failed）的行；重放成功即删除，failed 留给人工对账。
-- 幂等执行：可重复运行。

CREATE TABLE IF NOT EXISTS usage_billing_retry_queue (
    id BIGSERIAL PRIMARY KEY,
    request_id VARCHAR(255) NOT NULL,
    api_key_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    command JSONB NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_billing_retry_queue_request_api_key
    ON usage_billing_retry_queue (request_id, api_key_id);

CREATE INDEX IF NOT EXISTS idx_usage_billing_retry_queue_due
    ON usage_billing_retry_queue (next_attempt_at)
    WHERE status = 'pending';
