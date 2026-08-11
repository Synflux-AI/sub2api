CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_system_logs_trace_id
    ON ops_system_logs ((extra ->> 'trace_id'))
    WHERE extra ->> 'trace_id' IS NOT NULL;
