ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN groups.model_rate_multipliers IS
    'Per-model sale rate multipliers: [{"models":[pattern...],"rate_multiplier":number}]; a matching model replaces groups.rate_multiplier and takes precedence over the user-specific group rate';

ALTER TABLE user_group_rate_multipliers
    ADD COLUMN IF NOT EXISTS model_rate_multipliers JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN user_group_rate_multipliers.model_rate_multipliers IS
    'User overrides of the group model rate rules: {"<group model pattern>": number}; absent patterns inherit groups.model_rate_multipliers';
