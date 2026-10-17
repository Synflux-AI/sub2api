-- 只索引异常结束与 unknown 的行：stop / tool_use / pause_turn 占绝大多数，走索引没有收益，
-- 不进索引可避免在热表上再维护一个近似全量的 B-tree。
-- 按原因筛选使用逐值参数化 IN (...)，参数值落在本谓词内时可命中；修改异常集合需新增迁移。
-- CONCURRENTLY 避免阻塞热表写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_finish_reason_created_at
    ON usage_logs (finish_reason, created_at)
    WHERE finish_reason IN (
        'truncated', 'refusal', 'content_filter', 'incomplete',
        'failed', 'cancelled', 'interrupted', 'empty', 'unknown'
    );
