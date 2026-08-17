# 流中断错误归一化 — 设计文档

日期：2026-08-17
上游 issue：[#143](https://github.com/Synflux-AI/sub2api/issues/143)（本变更是 #143 拆出的 **PR2**）
前置：`openspec/changes/observe-stream-failure-causes`（PR1）**必须先上线并灰度 1–2 天**
基准：`origin/release@3b574b37580631f6c5fee87f9ac682953f9c1af3`

## 状态

**设计已定稿，实施计划待 PR1 数据。** 本文档记录全部已决策项（含 #143 review 提出的 6 个洞的结论），`plan.md` 在 PR1 灰度数据到手、下节的门禁通过之后再写。

## 门禁：动手前必须回答的三个问题

PR1 上线后从 OpenObserve 取：

| 查询 | 用途 | 门禁判据 |
| --- | --- | --- |
| `gateway.stream_error_event_unmatched` 按 message 分组 top 10 | 运维到底该配什么关键字 | **若收敛到 ≤3 种稳定文案**，直接补关键字即可，本 PR 的两级匹配整块砍掉，只保留三终止点接入 |
| `gateway.stream_failure` 按 cause 分组 | 三种 cause 的实际占比 | 占比 <5% 的 cause 不接入，省掉对应的守卫复杂度 |
| `gateway.stream_failure` 按 scope 分组 | 首字前 vs 首字后 | `before_first_token` 占比即本 PR 的**可救上限**；若 <20%，改造收益不足以承担改 wire 的风险，退回只做观测 |

三个判据任一不通过，本 PR 按缩减后的范围重写，而不是照本文档全量实施。

## Context

PR1 已经让流中断可观测、可搜索，但没有改变任何行为：三种 cause 里只有 `missing_terminal_event` 挂着规则钩子，且被 `gateway_anthropic_passthrough.go:830` 的 `!semanticEventForwarded && !sawAnyErrorEvent` 守死。运维配的「流中断」规则（`status_codes=[]`、关键字 `missing terminal event` / `Concurrency limit exceeded for account`、`action=retry`、`retry_count=1`）自配置以来 0 次命中，同期其他规则命中 57 次。

三个死角（行号为当前 release）：

| 死角 | 位置 | 现状 |
| --- | --- | --- |
| 已发首字后不评估规则 | `:830` `!semanticEventForwarded` | **#137 的既定安全设计，本 PR 不改** |
| 上游 error 帧未命中后堵死合成路径 | `:830` `!sawAnyErrorEvent` | 本 PR 处理 |
| `read_error` / `interval_timeout` 无钩子 | `:862` / `:891` | 本 PR 处理 |

第二个死角的具体形态：规则引擎在 `:772` 确实被调用，但喂给它的是上游 error 事件原文，运维配的关键字是 sub2api 自己合成的 `missing terminal event`；随后 `:830` 的 `!sawAnyErrorEvent` 又堵死合成路径。两头都不匹配。

## 决策

### 1. 归一化模型

新增 `internal/service/gateway_stream_failure.go` 的第二部分（PR1 已建此文件）：

```go
const canonicalStreamFailureMessage = "stream usage incomplete: missing terminal event"
```

三种 cause 参与规则匹配时共用同一句 canonical 文案，合成状态码固定 502。

**核心边界：归一只发生在「喂给规则引擎」这一步。** 真实文案在 `forward_failed`、`ops_error_logs`、规则日志中全部保持原样（PR1 已建立这条通道）。

### 2. 三分离（review 洞 #5 的结论）

必须显式拆成三份，不得复用：

| 用途 | 内容 | 去处 |
| --- | --- | --- |
| `matchBody` | canonical 合成体，502 | 只喂 `decideErrorHandlingRule` |
| `clientEvent` / `clientMessage` | 改动前的对客协议与安全文案 | 只写 wire |
| `internalCause` / `internalDetail` | 真实 `read_error` / `interval_timeout` / `missing_terminal_event` | 只进 ops 与 stdout（PR1 的通道） |

两个必须一并覆盖的调用点，#143 正文没点名：

- `gateway_anthropic_passthrough.go:216` 的 `logErrorHandlingRuleDecision` 吃的是 `match.body`。第二级换成 canonical 后这条日志同样失真，会出现 `stream_failure_cause=read_error` 但 body 写着 `missing terminal event` 两个字段打架。
- `writeAnthropicPassthroughStreamRuleError:943-944` 的 `synthetic` 分支今天就是把 canonical 那句话直接吐给客户端的。对 `missing_terminal` 无差别，但 `read_error` / `interval_timeout` 走 passthrough 时会给客户端一句**错误的**「missing terminal event」。这才是「canonical 不得泄漏到 wire」真正在防的东西。

### 3. 两级匹配

先用上游 error 事件原文匹配（保留「上游/账号问题换号」「客户端错误」等精细规则的现有语义），未命中再用归一化形态兜底。第二级的合成体**不含上游原文**，避免被其他规则误抢。

两级状态码来源不同：第一级由 `anthropicPassthroughSSEError`（`:523-557`）从 `error.type` 推导（429/529/500 等）；第二级固定 502。故限了 `status_codes` 的规则在第二级不匹配，只有留空的规则能兜底——「流中断」规则正是如此配置。

502 是硬编码值：若将来有人配 `status_codes=[502]` 的宽泛规则，会把三种 cause 全抢走。这一点须写进运维文档。

### 4. 错误帧扣留（review 洞 #1 的结论 —— 与 #143 正文不同）

#143 正文：「第一级未命中的 error 帧先扣住不发，流终止时按终态决定」。

**改为：扣留只维持到「下一个要写出的事件」为止。** 任何后续 `writeEvent` 之前，先按序 flush 已扣留的帧。

理由：上游完全可能 `error → message_start → … → message_stop`。按正文写法，

- 终止点是 `:845` 的 `return resultWithUsage(), nil, nil`——唯一一条 `err == nil` 的返回。实现漏掉它就是**静默吞掉客户端本该收到的 error 帧**。
- 就算不漏，终止时才 flush 会把 error 帧重排到整段正常输出之后，wire 顺序变了。

改成「扣到下一个事件」之后：只保留 `error → EOF` 这个真正需要重试的场景，顺序问题与丢弃问题一并消失，#143 补充章节第 2 条「所有非 retry/failover 终态都要 flush」退化成只剩三个终止点 + `bufio.ErrTooLong` 出口。客户端已断开或 context 已取消时不再写。

溢出（>4 条 / >`maxEventSize`）时**全部 flush 而非丢弃**——丢弃会破坏客户端语义，flush 只是失去重试机会。

新增 `errorEventForwarded` 标志参与降级，用独立的 `downgrade_reason=error_event_forwarded`，与 `semantic_output_started` 区分。

`gateway_anthropic_passthrough.go:229` 的 `SafeToFailoverAfterWrite = !ruleMatch.semanticEventForwarded` 必须一并算上 `errorEventForwarded`（review 洞 #4）。理论上 `errorEventForwarded` 会先降级成 passthrough 走不到这里，但这是安全兜底，不能只靠降级路径。

### 5. 三个终止点接入 + 语义守卫（review 洞 #2 的结论）

三个终止点全部接入。`bufio.ErrTooLong`（`:860` / `:875`）**故意不接**——协议层违规或超大 payload，原样重发必然复现，只会白烧重试预算。

**第二级必须传 `SemanticEventForwarded`。** 现在 `:832` 没传，因为外层 `!semanticEventForwarded` 已经兜住；接入 `read_error` / `interval_timeout` 后这个前提没了——这两个 cause 大量发生在首字之后（输出到一半上游卡住是超时的典型形态）。不传就会在已发首字后 retry，产生双 `message_start`，正是 #137 要防的事故。`decideErrorHandlingRule:201-206` 已有降级逻辑，只需把 flag 传进去。

**语义变化须写进变更说明**（review 洞 #6）：样本 A 现在是「守卫直接跳过、不评估规则、无日志」，改成「评估后降级 passthrough」会**新增**一条 `error_handling_rule_matched` 日志和 ops 记录。这与 #143 正文「`:828` 是既定安全设计，本 issue 不改」有出入。新增日志是有意为之——它正是可观测性目标之一——但不能当成回归。

### 6. `interval_timeout` 的重试语义（review 洞 #3 的结论）

#143 补充章节第 6 条写「无论规则最终是 retry 还是 passthrough，都要且只能调用一次 `HandleStreamTimeout`」。规则 retry 会走新 attempt、再超时、再调一次，实际语义只能是**每 attempt 恰好一次**，不是每请求一次。措辞必须改，否则实现会加跨 attempt 的 once 标志，反而让账号健康分漏记。

耗时放大是本 PR 最可能在生产上出事的一条：两级都传 `IgnoreRetryElapsed: true`，直接绕开 `maxRetryElapsed` 时间窗。`StreamDataIntervalTimeout=60s` 配 `retry_count=1` 最坏变 120s+，配 2 就是 180s+，客户端多半早已超时，重试纯烧上游额度。

**结论：`interval_timeout` 不吃 `IgnoreRetryElapsed`**，即它的重试仍受 `maxRetryElapsed` 约束。另两种 cause 保持 `IgnoreRetryElapsed: true`（它们在毫秒级发生，不占时间窗）。

### 7. 保留的既有副作用与守卫

- client-disconnect / context-cancel 不评估规则（#5148 回归点）。
- partial usage 结算、Ollama activity、response committed、keepalive / failover 守卫。
- `interval_timeout` 的 `HandleStreamTimeout` 调用位置不变（`:888-890`）。

## 非目标

- **样本 A（首字后断流）仍不重试**。只从静默失败变为可观测 + 明确终态。要救需缓冲首个语义事件，涉及首字延迟与 keepalive 取舍。
- **非透传路径（`gateway_forward.go`）**。规则接入点均在 `resp.StatusCode >= 400` 之后，200 SSE 流中途无规则参与；且 `handleStreamingResponse` 在 `:915`，位于 attempt 循环（`:368-723`）**之外**，接入需重构重试循环。7 天 1272 次里有 391 次（13 个账号）属此类，**本 PR 之后它们依然只有 PR1 提供的观测**。
- **`output_tokens=0` 的不完整流仍向客户计费**。`StreamIncomplete` 目前只用于账号健康分（`gateway_usage_billing.go:595-596`），与计费脱钩。样本 A 中客户被收 $0.058 却拿到 0 内容。

## 验收

沿用 #143 的 1–13 条，并按上述决策修订三条：

- **验收 4（样本 B 端到端）** 拆成两个必须同时覆盖的用例：
  - `event: error → EOF`：允许扣留并干净 retry/failover。
  - `unknown semantic → EOF`：仍**不得** retry/failover，防止双流拼接。
  （`anthropicPassthroughSSEEventIsSemantic:559-573` 把任何非 ping、非 comment、非 error 的未知 SSE 数据视为 semantic，这类事件与样本 B 在观测上无法区分。）
- **新增**：`error → message_start → message_stop`（成功终态）→ 已扣留的 error 帧按序 flush 在 `message_start` **之前**，且不被丢弃；`event: error` 恰好出现一次。
- **验收 12 改写**：`interval_timeout` 在所有规则结果下，**每 attempt 恰好执行一次** `HandleStreamTimeout`；且其重试受 `maxRetryElapsed` 约束（不吃 `IgnoreRetryElapsed`）。

其余（#143 补充章节验收 8–11、13）原样保留，其中验收 9（脱敏）与 13（ops 可搜索内部真实 cause）已由 PR1 实现并测试，本 PR 只需回归。
