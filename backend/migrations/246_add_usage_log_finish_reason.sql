-- usage_logs.finish_reason 是按固定优先级汇总的标准化结束原因
-- （stop / tool_use / pause_turn / truncated / refusal / content_filter /
-- incomplete / failed / cancelled / interrupted / empty / unknown），只用于展示与筛选。
-- usage_logs.response_outcome 保留上游原始结束原因、拒答 / 工具调用 / 输出 / 终态等
-- 结构化事实，不含任何响应正文。
-- 两列 NULL 都表示历史行或未采集路径，不能视为 stop。
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS finish_reason VARCHAR(32);

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS response_outcome JSONB;
