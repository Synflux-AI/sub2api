# 流中断原因可观测化 — 设计文档

日期：2026-08-17
上游 issue：[#143](https://github.com/Synflux-AI/sub2api/issues/143)（本变更是 #143 拆出的 **PR1**）
基准：`origin/release@3b574b37580631f6c5fee87f9ac682953f9c1af3`

## Context

### 为什么先做这个

#143 的方案分两块：**观测**（原方案第 5 点，收编自 #141）与**行为改造**（归一化 + 两级匹配 + 错误帧扣留）。后者的前提——「样本 B 是上游返回 200 + 一个 SSE `event: error` 后立即关流」——到今天为止**仍是假设**，不是抓到原始 SSE 之后的事实。#143 的 2026-08-17 review 补充已经承认了这一点。

`anthropicPassthroughSSEEventIsSemantic`（`backend/internal/service/gateway_anthropic_passthrough.go:559-573`）把任何非 ping、非 comment、非 error 的未知 SSE 数据都判为 semantic。这类未知事件同样会阻断规则评估，同样不会产生 usage/计费记录，落在 OpenObserve 里和样本 B 长得一模一样。也就是说，7 天 1272 次 `missing terminal event` 里到底有多少是 error 帧类、多少是未知 semantic 类，**现在无法区分**。

行为改造里风险最高的是「错误帧扣留」——它会改变客户端 wire 上收到的字节。在不知道真实分布的情况下先改 wire，是拿生产流量赌一个假设。

因此拆成两个 PR：

| PR | 内容 | 风险 | 依赖 |
| --- | --- | --- | --- |
| **PR1（本变更）** | 观测 + 脱敏 + 后台可搜索 | 对客零改动 | 无 |
| PR2 `normalize-stream-failure-rules` | 归一化 + 两级匹配 + 错误帧扣留 + 三终止点接入规则 | 改 wire 字节 | PR1 灰度 1–2 天的数据 |

PR1 上线后能直接回答三个问题，PR2 的设计参数全部依赖它们：

1. 1272 次里 error-frame 类占比多少？未知 semantic 类占比多少？
2. 三种 cause（`missing_terminal_event` / `read_error` / `interval_timeout`）各占多少？
3. 断流发生在首字之前还是之后（决定 PR2 能救多少）？

### 现状：真实原因是怎么丢的

样本 B 落库结果：

```
error_message          | Upstream request failed      ← 通用兜底文案
upstream_error_message | (空)
upstream_errors        | (空)
```

三处丢失，各有独立原因：

**1. 三个终止点都不写 ops。** `handleStreamingResponseAnthropicAPIKeyPassthroughWithRules` 的三个「上游把流搞断」出口：

| 位置 | 返回的 error | 是否写 ops |
| --- | --- | --- |
| `gateway_anthropic_passthrough.go:843` | `stream usage incomplete: missing terminal event` | ❌ |
| `gateway_anthropic_passthrough.go:862` | `stream read error: <cause>` | ❌ |
| `gateway_anthropic_passthrough.go:891` | `stream data interval timeout` | ❌ |

这些 error 一路返回到 handler，被 `ensureForwardErrorResponse`（`internal/handler/gateway_handler.go:1892`）换成对客的 `"Upstream request failed"`，`ops_error_logs.error_message` 记的就是这句通用文案。真实 cause 从来没有进入任何持久化字段。

**2. 第一级规则未命中时上游原文被完全丢弃。** `gateway_anthropic_passthrough.go:772` 确实调用了规则引擎，但 `decision.Matched == false` 时代码直接往下走（`:785` 起），上游 error 事件的原文既不落日志也不落 ops。运维因此无从得知该配什么关键字，只能持续监控、不断补关键字——这正是 #143 要根治的运维负担。

**3. 后台关键字搜索不覆盖 `upstream_error_message`。** `internal/repository/ops_repo.go:1016`：

```sql
(e.request_id ILIKE $n OR e.client_request_id ILIKE $n OR e.trace_id ILIKE $n OR e.error_message ILIKE $n)
```

即便把真实 cause 写进 `upstream_error_message` 列，后台按 `missing terminal event` 搜索仍然搜不到。这一条 #143 没提到，但它是验收 7「后台错误日志按 missing terminal event 能搜到」的必要条件。

### 现状：脱敏不够

`sanitizeUpstreamErrorMessage`（`internal/service/gemini_messages_compat_service.go:1772`）只做一件事：

```go
sensitiveQueryParamRegex = regexp.MustCompile(`(?i)([?&](?:key|client_secret|access_token|refresh_token)=)[^&"\s]+`)
```

即只处理 **URL query 里的 4 个参数**。JSON body 或 SSE data 里的 `api_key` / `authorization` / `token` / `password` 一律原样落库、原样进 stdout。

而且 `appendOpsUpstreamError`（`internal/service/ops_upstream_context.go:247-249`）**只脱敏 `Message`，不脱敏 `Detail`**。`Detail` 恰恰是承载上游响应体原文的字段，直接写进 `ops_error_logs.upstream_errors`。这是现网就存在的泄漏面，PR1 顺手补掉——因为 PR1 正要开始往日志里写更多上游原文，不先补等于扩大泄漏。

调用点的截断顺序也是反的：现有代码一律 `truncateString(string(body), maxBytes)` 之后才交给脱敏，属于「先截断后脱敏」。截断可能把 JSON 截成非法片段，导致结构化脱敏退化成文本脱敏。

## 决策

| 决策点 | 结论 | 理由 |
| --- | --- | --- |
| 内部 cause 走哪个通道 | 复用现成的 `upstream_error_message` 列 + `upstream_errors` JSON 数组 | 零 schema 变更；`upstream_error_message` 已是一等列，`channel_monitor_v2_aggregation.go:257` 已在聚合里检索它 |
| 是否按 #143 原方案改 `ensureForwardErrorResponse` 传 `err` | **否决** | 见下节 |
| `upstream_status_code` 写什么 | **不写**（传 0） | wire 上游状态确实是 200，合成 502 会污染上游状态维度；`OpsUpstreamErrorEvent` 的 `Reason` 字段注释明确说它存在就是为了「不用合成 HTTP 状态码来表达 cause」 |
| 计数通道 | stdout 结构化 warn（→ Vector → OpenObserve） | 请求经规则重试后最终成功时不会写 `ops_error_logs` 行，只有 stdout 能拿到完整计数 |
| 未命中日志去重 | hit-priority，每 attempt 最多一条 | 后续 error 命中规则时只保留 `error_handling_rule_matched`，避免同一故障两条记录 |
| 脱敏范围 | JSON 递归 + SSE data 行 + 纯文本 key/value + Bearer | 覆盖上游可能返回的三种形态 |
| 脱敏与截断顺序 | 先脱敏、后截断 | 截断后的非法 JSON 会让结构化脱敏失效 |
| 匹配输入是否脱敏 | **否** | 规则匹配必须用未改写的原始输入，否则关键字会被 `***` 打断 |
| 是否改控制流 | **否** | PR1 不新增/不删除任何 return 分支，不接规则，不扣留帧 |

### 为什么否决「给 `ensureForwardErrorResponse` 传 err」

#143 原方案第 5 点要求 `ensureForwardErrorResponse` 接收 `err`，让 `ops_error_logs` 记录真实 cause。这条路要动：

- 2 个同名实现（`internal/handler/gateway_handler.go:1892`、`internal/handler/openai_gateway_handler.go:2716`）
- 8 个生产调用点（`openai_images.go:348`、`openai_chat_completions.go:333`、`gateway_handler.go:510`、`gateway_handler.go:1019`、`gateway_handler_responses.go:304`、`gateway_handler_chat_completions.go:316`、`openai_gateway_handler.go:701`、`openai_gateway_handler.go:2328`）
- 独立的 `ensureAnthropicErrorResponse`（`openai_gateway_handler.go:1357`）
- `recoverResponsesPanic`（`openai_gateway_handler.go:2318`）只有 `recovered` 没有 `err`，改不成必传

一共 11 处 handler 层改动，且每一处都紧挨着客户端可见文案，**每一处都是把内部 cause 泄漏给客户端的机会**。

而 service 层本来就知道真实 cause，`upstream_error_message` / `upstream_errors` 本来就是给内部用的通道，`ops_error_logger` 已经在消费它们（`internal/handler/ops_error_logger.go:1312-1319`）。在 service 层写入是 1 处改动、0 处接触客户端文案。

因此 PR1 走 service 层写入，handler 层**一行不改**。客户端可见文案天然逐字节不变——不是靠测试保证，是靠根本没碰那段代码保证。

### 数据模型：cause 怎么表达

复用 `OpsUpstreamErrorEvent`（`internal/service/ops_upstream_context.go:195`），不加字段：

| 字段 | 取值 |
| --- | --- |
| `Kind` | `stream_failure`（三个终止点）/ `stream_error_unmatched`（第一级未命中） |
| `Reason` | `stream_failure`：`missing_terminal_event` / `read_error` / `interval_timeout`；`stream_error_unmatched`：`http_<上游派生状态码>`（如 `http_500`） |
| `Scope` | `before_first_token` / `after_first_token` |
| `Message` | 真实错误文案（`stream usage incomplete: missing terminal event` 等） |
| `Detail` | 上游原文，脱敏后截断，受 `LogUpstreamErrorBody` 开关控制 |
| `UpstreamStatusCode` | **0**（不写）——两种 Kind 都不写 |
| `Stage` | `client_disconnected`（客户端先走）/ 留空 |
| `Passthrough` | `true` |

`stream_error_unmatched` 同样不写 `UpstreamStatusCode`，原因和 `stream_failure` 一样、但后果更严重：非零状态码会让 `checkSkipMonitoringForUpstreamEvent` 继续往下走并调用 `MatchRule`，而 `MatchRule` 在规则没有关键字时仅凭状态码即可命中。运维只要配了一条 `anthropic`/`500` 且 `skip_monitoring=true` 的规则（压制 Overloaded 噪音的常规做法），`OpsSkipPassthroughKey` 就会被置位，**整条 `ops_error_logs` 行被丢弃**——包括本变更刚加的 `upstream_error_message`。派生状态码因此改放 `Reason`，并保留在结构化 warn 的 `status_code` 字段里。

`Stage` 用于标记客户端先断开的情形。`read_error` 与 `interval_timeout` 两个分支本来就在 `clientDisconnected` 时提前返回，而 `missing_terminal` 分支不会——客户端中止后上游再 EOF，会被记成 `missing_terminal_event` / `after_first_token`。这是占比最大的桶，不打标就无法在 OpenObserve 里把客户端主动断开剔除，PR2 的 go/no-go 分布会被污染。这里只打标，不改控制流。

`Scope` 直接回答「断流发生在首字前还是首字后」：`firstTokenMs != nil` 即首字已发。样本 A（`first_token_ms=844`、有计费）落 `after_first_token`，样本 B（无 usage、无计费）落 `before_first_token`。PR2 能救的只有 `before_first_token` 那部分，这个字段就是它的可行性度量。

注意 `UpstreamStatusCode` 传 0 还有一个副作用上的好处：`checkSkipMonitoringForUpstreamEvent`（`ops_upstream_context.go:270-271`）在 `UpstreamStatusCode == 0` 时直接 return，不会误触发 `skip_monitoring` 规则匹配。

### 脱敏设计

新增 `internal/service/upstream_error_sanitize.go`，`sanitizeUpstreamErrorMessage` 保留原名与签名，内部改为委托新实现，所有既有调用点自动获得增强。

**血缘范围**：该函数有 **104 个非测试调用点**（不是最初估计的 4 个），所以这是一次全局行为变更。保留全局强化是有意选择——#143 补充章节第 3 条是安全要求，为保护对客文案观感而只在新写入点启用，等于撤销该修复。掩码只在列出的凭证型 key 上触发，`\btoken\b` 不会命中 `max_tokens`。匹配路径已核实未被污染：`matchErrorHandlingRule` 收到的始终是原始 body，`safeAnthropicError` 产出的 message 只进 ops 与 Go error。

三层处理，按输入形态分派：

1. **JSON**（`{` / `[` 开头且能 Unmarshal）→ 递归遍历。map key 命中敏感字段名的，整个 value 换成 `***`（不管是 string 还是嵌套对象）；string 叶子再过一遍文本脱敏。重新 Marshal。
2. **SSE**（含 `data:` / `event:`）→ 逐行；`data:` 行的 payload 递归走第 1/3 层，其余行走文本脱敏。
3. **纯文本** → query 参数（保留现有正则）+ `Bearer <token>` + `key=value` / `key: value`。

敏感字段名清单（**不含** `signature`）：`api_key` / `apikey` / `x-api-key` / `authorization` / `access_token` / `refresh_token` / `id_token` / `client_secret` / `secret` / `password` / `passwd` / `token` / `cookie` / `session_id`。

`signature` 故意排除：thinking block 签名错误的排查完全依赖看到 `signature` 相关文案（`isThinkingBlockSignatureError`，`gateway_upstream_response.go:157`），把它打码等于砸掉现有排查手段，而它不是凭证。

JSON 重新 Marshal 会按 key 排序、丢失原始缩进。这只影响日志字段的可读形态，不影响任何匹配逻辑（匹配用原始输入），接受。

## 非目标

- **不改任何控制流**。三个终止点的 return 分支、返回的 error 值、规则评估时机全部不动。
- **不接规则**。`read_error` / `interval_timeout` 仍然没有规则钩子，`:830` 的 `!semanticEventForwarded && !sawAnyErrorEvent` 守卫原样保留。
- **不扣留错误帧**。第一级未命中的 error 帧仍然立即转发给客户端，`TestPassthroughStreamUnmatchedErrorPreservesLegacyFallbackContract` 的断言逐字节不变。
- **不动客户端可见文案**。handler 层零改动。
- **不覆盖非透传路径**。`gateway_forward.go` 的 `handleStreamingResponse`（`:915`）在 attempt 循环（`:368-723`）之外，接入需要重构重试循环，属 PR2 之后的独立议题。1272 次里有 391 次落在这条路径上，PR1 之后它们**依然只有通用文案**。
- **不修既有调用点的截断顺序**。除本变更新增的写入点外，其余 `truncateString(string(body), ...)` 调用点仍是先截断后脱敏，由 `appendOpsUpstreamError` 的兜底脱敏保证不泄漏，顺序修正留作后续清理。

## 验收

1. 三个终止点各自产生一条结构化 warn（`gateway.stream_failure`），带 `cause` / `scope` / `account_id` / `model`。
2. 三个终止点各自写入 `upstream_error_message`（真实文案）与一条 `upstream_errors` 事件（`kind=stream_failure`、`reason=<cause>`），且 `upstream_status_code` 保持不被写入。
3. 第一级规则未命中 → 一条 `gateway.stream_error_event_unmatched` warn，带脱敏后的上游原文。
4. 同一 attempt 内先未命中、后命中 → **只**产生命中记录，无未命中记录。
5. 同一 attempt 内连续多个未命中 error → 最多一条未命中记录。
6. 上游原文含 `api_key` / `authorization` / `token` / `password` → stdout、`upstream_error_message`、`upstream_errors[].detail` 全部脱敏；规则匹配仍使用未改写的原始输入。
7. 后台按 `missing terminal event` 搜索能命中（搜索子句覆盖 `upstream_error_message`）。
8. 客户端 wire 字节与改动前完全一致：`TestPassthroughStreamUnmatchedErrorPreservesLegacyFallbackContract`、`TestPassthroughStreamErrorRuleReturnsCompleteEventOnce`、`TestPassthroughStreamCleanEOFRetriesThroughVirtual502` 等既有用例不改一行即通过。

## 灰度与下一步

上线后观察 1–2 天，从 OpenObserve 取三组数字：

```
gateway.stream_failure          按 cause 分组计数
gateway.stream_failure          按 scope 分组计数
gateway.stream_error_event_unmatched  按上游 message 分组，取 top 10
```

第三组直接给出「运维到底该配什么关键字」的答案——如果 top 10 收敛到少数几种文案，PR2 的两级匹配可能根本不需要，直接补关键字即可；如果发散，PR2 的归一化才是必要的。这个判断放到 PR2 的设计定稿之前做。

**分组键只用 `cause`，不要用 `message`。** `read_error` 的 message 内嵌 `read tcp <本地IP:端口>-><对端IP:端口>`，基数极高，按它分组会把查询打爆。`cause`（3 值）与 `scope`（2 值）才是设计出来的分组维度。
