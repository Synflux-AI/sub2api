CREATE TABLE IF NOT EXISTS billing_entities (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(200) NOT NULL UNIQUE,
    currency VARCHAR(3) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT billing_entities_currency_check CHECK (char_length(currency) = 3),
    CONSTRAINT billing_entities_status_check CHECK (status IN ('active', 'inactive'))
);

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS billing_entity_id BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'users_billing_entities_users'
          AND conrelid = 'users'::regclass
    ) THEN
        ALTER TABLE users
            ADD CONSTRAINT users_billing_entities_users
            FOREIGN KEY (billing_entity_id)
            REFERENCES billing_entities(id)
            ON DELETE RESTRICT;
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS idx_users_billing_entity_id
    ON users(billing_entity_id);
