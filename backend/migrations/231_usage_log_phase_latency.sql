-- Persist the per-phase latency breakdown for successful requests.
--
-- The four phases (auth / routing / upstream / response) are already measured on
-- every request and stashed on the gin context, but until now they were only
-- written on the error path (ops_error_logs). Successful requests kept just
-- duration_ms and first_token_ms, so a slow request could not be attributed to
-- internal routing churn (account selection, retries, group fallback) versus a
-- slow upstream -- production ops_error_logs shows routing p95 at 19.3s.
--
-- TTFT is deliberately absent: first_token_ms already carries it.
--
-- INT matches first_token_ms/duration_ms on this table (ops_error_logs uses
-- BIGINT; no phase realistically exceeds int32 milliseconds ~ 24.8 days).
-- Nullable with no default: NULL means "not measured", 0 means "measured, <1ms".
-- Historical rows are not backfilled.
--
-- usage_logs is partitioned (035_usage_logs_partitioning.sql); ADD COLUMN on the
-- parent propagates to every partition. IF NOT EXISTS keeps it idempotent.
-- On PostgreSQL 11+ adding a nullable column without a default is a metadata-only
-- change -- no table rewrite -- but it still takes a brief ACCESS EXCLUSIVE lock.
--
-- lock_timeout is set here rather than left to the operator: migrations run
-- automatically at startup (internal/repository/ent.go) inside a transaction, so
-- there is no human in the loop to set it. Without it, an ALTER that lands while a
-- long analytics query holds a conflicting lock waits under the runner's shared
-- 10-minute context -- and every read and write arriving behind it queues too,
-- stalling usage traffic for most of a deployment. Failing startup fast is the far
-- cheaper outcome: the deploy retries, the queue never forms. Values match the
-- house precedent in 033_ops_monitoring_vnext.sql.
--
-- No index: the detail drawer reads a single row by id. Filtering or sorting by
-- phase latency would need one, and that is the expensive part -- not added.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS auth_latency_ms INT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS routing_latency_ms INT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_latency_ms INT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS response_latency_ms INT;
