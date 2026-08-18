ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS image_response_format VARCHAR(16);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'usage_logs_image_response_format_check'
          AND conrelid = 'usage_logs'::regclass
    ) THEN
        ALTER TABLE usage_logs
            ADD CONSTRAINT usage_logs_image_response_format_check
            CHECK (
                image_response_format IS NULL
                OR image_response_format IN ('url', 'b64_json')
            ) NOT VALID;
    END IF;
END $$;
