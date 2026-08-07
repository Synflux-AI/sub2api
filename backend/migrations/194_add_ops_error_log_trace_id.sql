-- Persist the request-scoped trace ID for future ops error records.
-- Existing rows remain NULL; no backfill is attempted because an inbound
-- X-Trace-Id cannot be reconstructed reliably from request_id.
ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS trace_id VARCHAR(64);

CREATE INDEX IF NOT EXISTS idx_ops_error_logs_trace_id
    ON ops_error_logs (trace_id)
    WHERE trace_id IS NOT NULL;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
    EXECUTE 'CREATE INDEX IF NOT EXISTS idx_ops_error_logs_trace_id_trgm
             ON ops_error_logs USING gin (trace_id gin_trgm_ops)';
  END IF;
END $$;
