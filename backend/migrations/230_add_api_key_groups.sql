-- Migration: 230_add_api_key_groups
-- API Key 绑定多个已有分组，按命中分组独立计费（issue #171）。
-- 新增 api_key_groups 关联表，并从 api_keys.group_id 回填存量绑定。
--
-- 设计要点：
-- 1. api_keys.group_id 列与其索引保留不动，语义收窄为「默认分组指针」；本迁移只新增关联表，
--    不删列、不改列。默认分组指针指向的分组同时会出现在 api_key_groups 里（见下面的回填）。
-- 2. platform 是写入时从 groups.platform 取的快照列，配合唯一索引
--    (api_key_id, platform) 保证「同一个 Key 在同一平台下只能绑定一个分组」。
--    分组的 platform 之后被改动时，快照与 groups.platform 会出现偏差，
--    由应用层负责联动（属于 #171 的服务层职责，不在本迁移内解决）。
-- 3. groups / api_keys 都走软删（UPDATE deleted_at），外键的 ON DELETE CASCADE 只在物理删除时
--    触发，因此关联行在软删路径上的清理必须由应用层显式 DELETE 负责
--    （参照 group_repo.DeleteCascade 里对 user_allowed_groups / account_groups 的处理）。
--
-- 回滚窗口注意事项：
-- migrations_runner 会跳过 schema_migrations 里已记录的迁移，本文件永不重跑；本表也只增不删，
-- 所以「回滚应用镜像」这一步本身是安全的（旧代码完全忽略 api_key_groups）。
-- 需要人工善后的是**回滚窗口内旧代码写过分组的那些 Key**，共两种。
-- 两条语句**必须按下面的顺序跑**：先清残留、再回填。反过来的话，残留行会让回填的
-- ON CONFLICT DO NOTHING 静默跳过那一行，看起来成功、实际什么都没修。
--
-- 步骤 1（**必须修，不会自愈**）：清残留关联行。
-- 旧代码把默认组从 A 改成同平台的 C 时只写 api_keys.group_id，关联行 A 留在表里。
-- 回滚回来后读模型做「默认组 UNION 关联表」得到 {A, C} —— 同一平台两个分组，破坏 C1。
-- 注意关联表自身没有重复（UNIQUE (api_key_id, platform) 保证只有 A 一行），
-- 双绑是 UNION 出来的，所以查重复行是查不出问题的。
-- 选组按 (platform, id) 命中 id 较小的那个，于是请求可能按 A 计费而不是 C，无报错无日志。
--   DELETE FROM api_key_groups akg
--    USING api_keys k, groups g
--    WHERE akg.api_key_id = k.id
--      AND g.id = k.group_id
--      AND akg.platform = g.platform
--      AND akg.group_id <> k.group_id;
--
-- 步骤 2：补缺失的关联行。旧代码新建的 Key 只写了 api_keys.group_id。
-- 这一种其实能自愈（读模型 boundGroupsFromEntity 会把默认组并进绑定集合，
-- 网关与列表筛选都不受影响），补齐是为了让关联表与 group_id 重新对齐，也补上
-- 步骤 1 刚删掉的那些行。幂等，可安全重跑：
--   INSERT INTO api_key_groups (api_key_id, group_id, platform)
--   SELECT k.id, k.group_id, g.platform
--     FROM api_keys k JOIN groups g ON g.id = k.group_id
--    WHERE k.group_id IS NOT NULL AND k.deleted_at IS NULL
--   ON CONFLICT DO NOTHING;
--
-- 自检（两步跑完后应返回 0 行）：
--   SELECT k.id
--     FROM api_keys k
--     JOIN groups g          ON g.id = k.group_id
--     JOIN api_key_groups akg ON akg.api_key_id = k.id
--    WHERE k.deleted_at IS NULL
--      AND akg.platform = g.platform
--      AND akg.group_id <> k.group_id;
--
-- 另注：多分组功能一旦被使用，回滚就是有损的 —— 旧代码只认 api_keys.group_id，
-- 非默认平台的绑定会静默按默认组计费。可无损回滚的窗口截止到第一把多绑 Key 建立之前。

CREATE TABLE IF NOT EXISTS api_key_groups (
    api_key_id  BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    group_id    BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    platform    VARCHAR(50) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (api_key_id, group_id)
);

COMMENT ON TABLE api_key_groups IS 'API Key 与分组的多对多绑定关系；api_keys.group_id 仍表示默认分组';
COMMENT ON COLUMN api_key_groups.platform IS '绑定时从 groups.platform 取的快照，保证同一 Key 同一平台只绑一个分组';

-- 同一个 Key 在同一平台下只能绑定一个分组。
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_key_groups_key_platform
    ON api_key_groups(api_key_id, platform);

-- 反向查询：按分组找绑定了它的 API Key（认证缓存失效、分组删除清理都要用）。
CREATE INDEX IF NOT EXISTS idx_api_key_groups_group_id
    ON api_key_groups(group_id);

-- 存量回填：把默认分组指针 api_keys.group_id 镜像成一条关联行，platform 取自 groups.platform。
-- 每个 Key 最多产生一行，不可能触发 (api_key_id, platform) 冲突；ON CONFLICT DO NOTHING
-- 覆盖两个唯一约束，保证本语句可重复执行。
INSERT INTO api_key_groups (api_key_id, group_id, platform)
SELECT k.id, k.group_id, g.platform
FROM api_keys k
JOIN groups g ON g.id = k.group_id
WHERE k.group_id IS NOT NULL
  AND k.deleted_at IS NULL
ON CONFLICT DO NOTHING;
