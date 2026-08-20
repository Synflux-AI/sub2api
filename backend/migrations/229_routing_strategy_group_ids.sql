-- Migration: 229_routing_strategy_group_ids
-- 智能路由策略分组多选：group_id 单值列 -> group_ids 数组列。
-- group_id 列与 idx_routing_strategies_group 索引保留不动（回滚窗口内仍需读写），
-- 下个版本随代码一并删除。

ALTER TABLE routing_strategies
    ADD COLUMN IF NOT EXISTS group_ids JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN routing_strategies.group_ids
    IS '生效分组 ID 列表，空数组表示全局生效';

-- 存量回填：单值列转单元素数组（幂等，仅回填仍为空数组的行）
UPDATE routing_strategies
   SET group_ids = jsonb_build_array(group_id)
 WHERE group_id IS NOT NULL
   AND group_ids = '[]'::jsonb;
