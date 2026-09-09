-- 模型维度 RPM 限流规则。
-- 与 users.rpm_limit / groups.rpm_limit / user_group_rate_multipliers.rpm_override 三层并列，
-- 但配额维度是「公开模型名」：scope 决定配额怎么分（每用户一份 / 全站共享一池），
-- target_type 决定这条规则管谁（全部用户 / 某分组 / 某用户），两者正交。
--
-- 与 user_group_rate_multipliers 一致走原生 SQL 迁移，不进 ent schema：
-- 规则总量为几十条量级，读路径是全表加载进内存快照，无需 ent 的关联查询能力。

CREATE TABLE IF NOT EXISTS model_rpm_rules (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    model_pattern TEXT NOT NULL,
    scope TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id BIGINT,
    rpm_limit INTEGER NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT model_rpm_rules_scope_check CHECK (scope IN ('user', 'global')),
    CONSTRAINT model_rpm_rules_target_type_check CHECK (target_type IN ('all', 'group', 'user')),
    -- target_type=all 时 target_id 必须为 NULL；group/user 时必填。
    CONSTRAINT model_rpm_rules_target_id_check CHECK (
        (target_type = 'all' AND target_id IS NULL)
        OR (target_type <> 'all' AND target_id IS NOT NULL)
    ),
    -- rpm_limit 必须为正：本表不提供 rpm_override=0 那样的「免检绿灯」语义。
    CONSTRAINT model_rpm_rules_rpm_limit_check CHECK (rpm_limit > 0)
);

CREATE INDEX IF NOT EXISTS idx_model_rpm_rules_enabled
    ON model_rpm_rules(enabled);

CREATE INDEX IF NOT EXISTS idx_model_rpm_rules_target
    ON model_rpm_rules(target_type, target_id);

-- 完全相同的两条启用规则行为上等价（各占一个 Redis 桶），只会白白多一轮 RTT 并令排查困惑。
-- target_id 为 NULL 时 Postgres 视 NULL 互不相等，用 COALESCE 折叠到 0 才能真正约束 target_type=all。
CREATE UNIQUE INDEX IF NOT EXISTS idx_model_rpm_rules_enabled_unique
    ON model_rpm_rules(model_pattern, scope, target_type, COALESCE(target_id, 0))
    WHERE enabled = TRUE;
