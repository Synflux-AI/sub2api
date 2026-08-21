-- Migration: 229_routing_strategy_group_ids
-- 智能路由策略分组多选：group_id 单值列 -> group_ids 数组列。
-- group_id 列与 idx_routing_strategies_group 索引保留不动（回滚窗口内仍需读写），
-- 下个版本随代码一并删除。
--
-- 回滚窗口注意事项：
-- 1. migrations_runner 会跳过 schema_migrations 里已记录的迁移，本文件永不重跑。回滚到 229 之前的
--    镜像期间，旧代码只写 group_id，group_ids 停在列默认值 '[]'；回滚回来后读路径按设计只信
--    group_ids（不做旧列兜底），这些行会静默变成「全局生效」——restrict 策略会命中所有分组。
--    因此回滚到 229 之前的镜像期间，不要新建或编辑路由策略。
-- 2. 未来负责 DROP group_id 列的那个迁移，必须在 DROP 之前先重跑下面这段回填，
--    再执行 DROP，否则回滚窗口内产生的行会永久丢失分组作用域：
--      UPDATE routing_strategies SET group_ids = jsonb_build_array(group_id)
--       WHERE group_id IS NOT NULL AND group_ids = '[]'::jsonb;

ALTER TABLE routing_strategies
    ADD COLUMN IF NOT EXISTS group_ids JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN routing_strategies.group_ids
    IS '生效分组 ID 列表，空数组表示全局生效';

-- 存量回填：单值列转单元素数组（幂等，仅回填仍为空数组的行）
UPDATE routing_strategies
   SET group_ids = jsonb_build_array(group_id)
 WHERE group_id IS NOT NULL
   AND group_ids = '[]'::jsonb;
