-- 154_routing_strategies.sql
-- 智能路由策略引擎：按请求属性（模型 / 客户端类型 / User-Agent）将请求路由到指定账号。
-- restrict = 只能路由到这些账号；prefer = 优先这些账号，不可用时回退到全量账号。
-- 评估时按 priority 升序，首个命中的策略获胜（first-match-wins）。
-- group_id 为空表示全局生效，否则仅对该分组生效。

CREATE TABLE IF NOT EXISTS routing_strategies (
    id          BIGSERIAL PRIMARY KEY,
    name        VARCHAR(128) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    priority    INTEGER NOT NULL DEFAULT 100,
    platform    VARCHAR(32) NOT NULL DEFAULT 'anthropic',
    group_id    BIGINT,
    match_mode  VARCHAR(8) NOT NULL DEFAULT 'all',
    conditions  JSONB NOT NULL DEFAULT '[]'::jsonb,
    action      VARCHAR(16) NOT NULL DEFAULT 'restrict',
    account_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

COMMENT ON TABLE routing_strategies IS '智能路由策略：按模型/客户端/UA 将请求路由到指定账号';
COMMENT ON COLUMN routing_strategies.priority IS '评估优先级，数值越小越先评估（first-match-wins）';
COMMENT ON COLUMN routing_strategies.platform IS '生效平台，空字符串表示任意平台';
COMMENT ON COLUMN routing_strategies.group_id IS '生效分组 ID，NULL 表示全局生效';
COMMENT ON COLUMN routing_strategies.match_mode IS '策略内多条件的组合方式：all（全部满足）| any（任一满足）';
COMMENT ON COLUMN routing_strategies.conditions IS '匹配条件列表：[{type,op,value}]，type=model|client|user_agent';
COMMENT ON COLUMN routing_strategies.action IS '动作：restrict（硬路由）| prefer（软优先回退）';
COMMENT ON COLUMN routing_strategies.account_ids IS '目标账号 ID 列表';

CREATE INDEX IF NOT EXISTS idx_routing_strategies_enabled_priority
    ON routing_strategies (enabled, priority) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_routing_strategies_group
    ON routing_strategies (group_id) WHERE deleted_at IS NULL;
