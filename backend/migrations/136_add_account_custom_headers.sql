-- Add advanced-mode custom outbound headers configuration to accounts.
-- custom_headers_enabled: optional toggle, default false (feature off).
-- custom_headers: JSONB string-to-string map, default empty.
-- These headers are merged into outbound upstream requests at gateway dispatch
-- when custom_headers_enabled is true. Hop-by-hop headers and a small protected
-- set (Host, Content-Length) are filtered out in application code.

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS custom_headers_enabled boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN accounts.custom_headers_enabled IS 'Whether to merge custom_headers into outbound upstream requests (advanced mode toggle).';

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS custom_headers jsonb NOT NULL DEFAULT '{}'::jsonb;
COMMENT ON COLUMN accounts.custom_headers IS 'Custom outbound headers as a JSONB string-to-string map (advanced mode payload).';
