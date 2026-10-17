# 用量结束原因（finish_reason）

用量明细记录每次上游响应"怎么结束的"，用于排查 HTTP 200 但内容异常的请求（拒答、内容过滤、截断等）。观测结果**只用于展示与筛选**，不参与计费、换号、SLA 或账号健康度判断，也不保存任何响应正文。

## 存储

| 列 | 说明 |
| --- | --- |
| `usage_logs.finish_reason` | 按固定优先级汇总的标准化原因，用于列表展示与筛选（迁移 246） |
| `usage_logs.response_outcome` | JSONB，保留上游原始事实：协议、Responses `status`、原始 `stop_reason` / `finish_reason`（Gemini 为首候选 `finishReason`）、多 choice 原因、Gemini `promptFeedback.blockReason`（`block_reason`）、`incomplete_details.reason`、Anthropic `stop_details`、错误类型，以及 `refusal` / `tool_call` / `has_output` / `terminal` / `missing_finish_reason` / `client_disconnected` 标记 |

- `finish_reason` 为 `NULL` 表示历史行或未采集路径，**不能**视为 `stop`。
- `response_outcome` 只在非正常结束时写入：`stop` / `tool_use` 的事实几乎都能由 `finish_reason` 推出，为控制热表体积不写；但客户端中途断开的行仍会写入。
- `response_outcome` 仅管理端可见，用户侧只返回 `finish_reason`。
- 迁移 247 的部分索引只覆盖异常结束集合与 `unknown`（字面量谓词，需与代码中的 `AbnormalReasons` 保持一致，有集成测试守护）；按 `stop` 等正常原因筛选不走该索引。

## 标准化取值

| 取值 | 来源 | 是否计入 `abnormal` |
| --- | --- | --- |
| `stop` | Anthropic `end_turn` / `stop_sequence`；Chat `stop`；Responses `completed`；Gemini `STOP` | 否 |
| `tool_use` | 需要客户端执行的工具调用：Anthropic `tool_use`；Chat `tool_calls` / `function_call`；Responses `completed` 且含 `function_call` 等客户端工具项或 `mcp_approval_request`；Gemini `STOP` 且含 `functionCall`。服务端工具（Anthropic `server_tool_use` / `mcp_tool_use`，Responses `mcp_call` / `web_search_call` 等）不算 | 否 |
| `pause_turn` | Anthropic `pause_turn`（服务端工具循环暂停，需客户端续发） | 否 |
| `truncated` | Anthropic `max_tokens` / `model_context_window_exceeded`；Chat `length`；Responses `incomplete` + `max_output_tokens`；Gemini `MAX_TOKENS` / `CONTINUATION` | 是 |
| `refusal` | 供应商明示的结构化拒答：Anthropic `stop_reason=refusal`；Chat `message.refusal` / `delta.refusal`；Responses `refusal` 内容块或 `response.refusal.*` 事件 | 是 |
| `content_filter` | Chat `content_filter`；Responses `incomplete` + `content_filter`；Gemini `SAFETY` / `RECITATION` / `LANGUAGE` / `BLOCKLIST` / `PROHIBITED_CONTENT` / `SPII` / `IMAGE_SAFETY` / `IMAGE_PROHIBITED_CONTENT` / `IMAGE_RECITATION` / `ESCALATION`，以及 `promptFeedback.blockReason` 有值（`BLOCK_REASON_UNSPECIFIED` / `BLOCKED_REASON_UNSPECIFIED` 除外） | 是 |
| `incomplete` | Responses `incomplete` 的其它原因（`max_messages`、`steered` 或未知）；Gemini `MALFORMED_FUNCTION_CALL` / `UNEXPECTED_TOOL_CALL` / `TOO_MANY_TOOL_CALLS` / `MISSING_THOUGHT_SIGNATURE` / `MALFORMED_RESPONSE` / `NO_IMAGE` / `IMAGE_OTHER` | 是 |
| `failed` | Responses `failed`，或流内 `error` 事件；Gemini 错误信封 `{"error":{...}}`，或 `PUP_LIMITED_DISABLED`（上游账号因违反使用政策被限制，与本次内容无关） | 是 |
| `cancelled` | Responses `cancelled` | 是 |
| `interrupted` | 未观察到完整终态（断流、网关停止读取上游等）。Chat 多 choice 时，只要有一个已出现的 choice 未结束且没收到 `[DONE]` 就算中断；Gemini 首候选始终没给出 `finishReason` 且提示词未被拦截 | 是 |
| `empty` | 正常终态但没有任何可消费输出（文本、工具调用、拒答说明、图像/音频等；推理内容不算），包括非流式 Chat `choices: []` | 是 |
| `unknown` | 上游给出了未识别的原因值（含 Gemini `OTHER` / `FINISH_REASON_UNSPECIFIED`）；或 Chat 响应已结束但有 choice 没有给出 `finish_reason`（只发 `[DONE]`、非流式 `finish_reason: null`） | 否，单独展示 |

Gemini 原值以官方枚举为准：[FinishReason](https://ai.google.dev/api/generate-content#FinishReason)、[BlockReason](https://ai.google.dev/api/generate-content#BlockReason)。

## 汇总优先级

多项事实同时出现时按以下顺序取第一个，原始事实仍完整保留在 `response_outcome` 中：

`failed > cancelled > refusal > content_filter > interrupted > truncated > incomplete > pause_turn > tool_use > unknown > empty > stop`

例如拒答与截断同时出现时显示 `refusal`，`incomplete_reason=max_output_tokens` 仍可在原始事实中看到。Chat 多 choice 时逐 choice 记录原因，请求级取优先级最高者。

## 采集边界

- 只识别供应商明示的结构化信号。普通文本中的"无法协助"等表述不判定为拒答；若后续增加文本规则，需另设 `suspected_refusal` 与检测来源，不与明示拒答混用。
- 观测在兼容转换之前进行（例如 Anthropic `max_tokens` 不会因为转换成 Chat `length` 而丢失原始值）。
- 每次 Forward 尝试新建观测器，换号重试只保留最终账号的结果；WebSocket 同一连接上的多个响应按 `response_id` 分别记录。
- 客户端中途断开而网关继续读完上游时，结束原因仍取上游终态（例如 `stop`），并在原始事实中标记 `client_disconnected`。
- Gemini 流没有独立的终态事件：首候选给出 `finishReason` 或提示词被拦截即视为终态。多候选只看首候选（`index` 缺省或为 0），与 ops 带内信号判定一致。
- Gemini 错误信封在任何时刻出现都记为 `failed`，包括已经收到 `finishReason` 之后：Gemini 账号的各路径同样按上游失败处理（流式路径整次请求报错，非流式与上游流式聚合路径登记上游错误并计入 SLA）。这与 Responses 不同：Responses 的裸 `error` 之后若有成功终态，以终态为准。
- 客户端非流式、上游流式（Code Assist）的聚合路径逐个 chunk 观测：`finishReason` 常在不带 parts 的末尾 chunk 上，聚合结果取最后一个带 parts 的 chunk，会丢掉它。

## 采集范围

`abnormal` 筛选只覆盖下表中会采集的路径；未采集路径的行 `finish_reason` 为 `NULL`，不会出现在任何按原因筛选的结果里。

| 路径 | 是否采集 |
| --- | --- |
| Anthropic Messages：直连、API Key 透传、AWS Bedrock（流式与非流式） | 是 |
| Anthropic 上游的 Chat Completions / Responses 兼容转换、OpenAI 侧原生 Anthropic 账号 | 是 |
| Antigravity 的 Anthropic 形态上游（`ForwardUpstream`，复用同一 Anthropic 观测点） | 是 |
| OpenAI Chat Completions：raw 直通、CC 管道（含 Responses / Messages 的 Chat 回退） | 是 |
| OpenAI Responses：HTTP/SSE、透传、Chat / Messages 兼容转换 | 是 |
| OpenAI Responses WebSocket：ingress、HTTP 桥接、v2 透传（按 `response_id` 分别记录） | 是 |
| Gemini 上游：原生 `/v1beta`（流式与非流式，含上游流式聚合）、`/v1/messages` 与 `/v1/chat/completions` 兼容转换 | 是 |
| Antigravity 的 v1internal（Gemini 形态）上游：Gemini 路径，以及经它转换的 Claude 路径（`handleClaudeStreamingResponse` / `collectClaudeStreamResponse`），协议记为 `gemini_generate_content` | 是 |
| 图片、视频、音频、Embeddings、搜索等非对话接口 | 否 |
