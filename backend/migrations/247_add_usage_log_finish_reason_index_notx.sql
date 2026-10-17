-- 只索引已采集结束原因的行；按原因筛选走逐值参数化 IN (...)，可由谓词证明命中部分索引。
-- CONCURRENTLY 避免阻塞热表写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_finish_reason_created_at
    ON usage_logs (finish_reason, created_at)
    WHERE finish_reason IS NOT NULL;
