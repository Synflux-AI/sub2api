-- Persist the inbound cross-service correlation ID separately from request_id.
-- request_id remains the existing usage-log idempotency key; historical rows
-- have no recoverable trace ID and therefore remain NULL.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS trace_id VARCHAR(64);

CREATE INDEX IF NOT EXISTS idx_usage_logs_trace_id
    ON usage_logs (trace_id)
    WHERE trace_id IS NOT NULL;
