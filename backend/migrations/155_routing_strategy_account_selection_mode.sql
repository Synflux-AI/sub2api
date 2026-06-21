-- 155_routing_strategy_account_selection_mode.sql
-- 智能路由策略：账号优先级（与 account_ids 对齐）。
--   数值越小越优先；相同数值视为同一优先级，再按负载率 / LRU 选择（默认算法：优先级 + 负载 + LRU）。
--   留空表示所有账号同一优先级（即纯负载 + LRU）。

ALTER TABLE routing_strategies
    ADD COLUMN IF NOT EXISTS account_priorities JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN routing_strategies.account_priorities
    IS '与 account_ids 对齐的账号优先级数组（数值越小越优先；相同数值为同一优先级，再按负载 / LRU 选择）';
