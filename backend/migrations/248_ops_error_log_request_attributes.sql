-- 错误日志记录「请求本身的属性」，使下游能按与 usage_logs 相同的筛选值筛错误：
-- reasoning_effort / service_tier / image_size / image_input_size / image_response_format
-- 的类型与口径对齐 usage_logs 同名列；image_count 在错误侧记请求的张数（如 n），
-- 非出图请求为 NULL。失败时还没算出来的值为 NULL，历史行不回填。
--
-- duration_ms 列在 033 已存在但此前从未写入，本迁移不改它；应用自此起写入请求进入
-- 网关到写错误日志时的总耗时（毫秒）。
--
-- 只加可空、无默认值的列（PostgreSQL 11+ 仅改元数据，不重写表），不建索引。
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(20);
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS service_tier VARCHAR(16);
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_size VARCHAR(10);
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_input_size VARCHAR(32);
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_response_format VARCHAR(16);
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS image_count INT;
