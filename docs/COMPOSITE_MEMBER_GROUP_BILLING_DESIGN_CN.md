# Composite 成员分组路由与用户专属倍率方案

> 状态：提案
>
> 日期：2026-08-13
>
> 关联文档：[当前 Composite Groups 行为](./COMPOSITE_GROUPS.md)

## 1. 决策摘要

在目标组合模式下，Composite 分组只作为客户访问入口和公开模型路由层，不单独维护 Codex、Claude 等平台的价格或执行策略；显式禁用时仍保留旧模式以兼容历史配置。

请求命中 Composite 后，系统先解析实际执行的原分组，再统一使用该执行分组完成账号调度、策略检查和计费。直接使用原分组和通过 Composite 路由到原分组，在计费模型、用量和计费时刻相同时，必须得到完全相同的最终费用 `actual_cost`，而不只是相同倍率。

核心规则：

```text
访问权限由 API Key 所属入口分组决定。
账号调度、执行策略和全部计费配置由最终执行分组决定。
Composite 内部路由不要求用户拥有执行分组的直接访问权限。
```

组合模式使用显式开关。管理员可以先保存成员关系并预览，只有显式启用后才切换线上请求；禁用时保持现有旧模式行为。本方案不新增 Composite 平台倍率表，也不复制用户专属倍率。

## 2. 背景与问题

当前 Composite 是一个独立分组：管理员可以把不同平台的账号直接关联或复制到 Composite，运行时按模型解析目标平台并从 Composite 自身的账号关联中调度。

当前计费仍以 API Key 所属分组为倍率来源。因此，当 Composite 默认倍率为 `1x` 时，同一个 Composite Key 无法自然表达以下客户价格：

| 客户 | Codex 专属倍率 | Claude 专属倍率 |
| --- | ---: | ---: |
| 企业 A | `0.15x` | `0.35x` |
| 企业 B | `0.12x` | `0.40x` |

业务同时要求：

- 企业只获得 Composite 权限时，可以用一个 Key 自动调用 Codex 和 Claude。
- 企业不需要获得 Codex、Claude 原分组的直接访问权限。
- 企业也可以按需获得某个原分组权限并创建单独的 Key。
- 无论从 Composite 还是原分组进入，相同用户、相同执行分组的倍率必须一致。
- 不维护模型固定价格，也不在 Composite 中重复维护一套平台倍率。

## 3. 目标

### 3.1 功能目标

1. Composite 可以组合多个已有的普通分组。
2. 每个请求能够确定唯一的实际执行分组。
3. 计费复用现有 `user_id + group_id` 专属倍率。
4. Composite 权限与成员分组权限彼此独立。
5. 直接访问原分组与经 Composite 访问时计费结果一致。
6. API Key 配额、订阅额度和客户用量归属仍保留在 Composite 入口分组。
7. 用量日志可以同时追溯入口分组和实际执行分组。
8. 成员配置和线上模式切换彼此独立，可以先配置、预览，再原子启用。
9. 组合模式下不存在 Composite 与成员分组混用价格或策略的情况。

### 3.2 非目标

MVP 不实现以下能力：

- 同一个 Composite 在同一平台下组合多个成员分组。
- 根据用户、成本、权重或负载在多个同平台成员分组间二次选组。
- 按单个模型配置用户专属倍率。
- Composite 嵌套 Composite。
- 将历史 Composite 的账号复制关系自动推断为成员关系。
- 新增或维护模型固定价格表。

如果未来确实需要“同一平台多个池”，再为 Composite 增加明确的选组规则；当前不预留推测性配置。

## 4. 领域术语

### 4.1 入口分组（Entry Group）

API Key 直接所属的分组。

入口分组负责：

- 用户是否可以创建该分组的 Key。
- API Key 的归属、状态和额度。
- 订阅计划和订阅用量归属。
- 客户侧用量报表的主分组归属。

对于 Composite Key，入口分组就是 Composite。

### 4.2 执行分组（Execution Group）

请求最终使用的普通分组。

执行分组负责：

- 候选账号池。
- 分组默认倍率。
- 用户在该分组上的专属倍率。
- 与倍率相关的高峰、图片、视频等计费配置。
- 调度过程中的利润控制及适用于该账号池的限制。

普通分组 Key 的入口分组和执行分组相同。Composite Key 的两者不同。

### 4.3 成员分组（Member Group）

被 Composite 组合的普通分组。一次请求选中成员分组后，该成员分组就是本次请求的执行分组。

## 5. 必须保持的不变量

### 5.1 权限与倍率相互独立

现有数据模型已经分别保存：

- `user_allowed_groups`：用户可以直接访问哪些分组。
- `user_group_rate_multipliers`：用户在各分组上的专属倍率。

因此允许存在以下配置：

```text
企业 A 的访问权限：Composite X

企业 A 的专属倍率：
  Codex 分组：0.15x
  Claude 分组：0.35x
```

为用户配置 Codex、Claude 专属倍率不等于授予这两个分组的直接访问权限。

### 5.2 倍率只有一个权威写入点

用户专属倍率始终写入现有原分组记录：

```text
(user_id, codex_group_id)  -> 0.15
(user_id, claude_group_id) -> 0.35
```

不得写入：

```text
(user_id, composite_group_id) -> 某个平台倍率
```

也不得在 Composite 下复制一份倍率。即使以后在 Composite 页面提供客户倍率矩阵，该页面也只能读写上述原分组记录。

### 5.3 入口不同不改变执行分组价格

给定用户和执行分组后，倍率解析结果不能受入口分组影响：

```text
ResolveRate(user=A, executionGroup=Codex) = 0.15x
```

以下两条路径必须得到相同结果：

```text
Codex Key     -> Codex 执行分组 -> 0.15x
Composite Key -> Codex 执行分组 -> 0.15x
```

### 5.4 最终费用一致

“直连与 Composite 同价”指最终费用一致，不只指 `rate_multiplier` 一致。给定以下相同输入和同一版本的执行分组计费配置快照：

```text
user_id
execution_group_id
billable_model
usage
pricing_at
execution_group_pricing_snapshot
```

两条路径必须解析相同的：

- 用户专属倍率和分组默认倍率。
- 高峰、图片、视频倍率。
- 渠道 token/per-request/image 定价。
- 图片、视频、搜索、Web Search 和语音价格。
- 长上下文及其他参与费用计算的执行组配置。

最终 `actual_cost` 必须相同。Composite 自身的渠道定价、媒体价格或默认倍率在组合模式下不得参与费用计算。

### 5.5 一次请求只有一份执行上下文

执行分组在调度前解析一次，形成请求级 `GroupExecution`，并显式传给后续策略检查、账号调度、利润控制和异步计费。不得让不同调用方从 `apiKey.Group`、账号或 context 分别推断执行分组。

账号级 failover 只能在同一执行分组内换账号，不能改变执行分组。为避免把现有跨分组 fallback 语义带入 MVP，启用组合模式时拒绝关联配置了 `fallback_group_id` 或 `fallback_group_id_on_invalid_request` 的成员分组。

## 6. 业务示例

基础配置：

| 分组 | 类型 | 默认倍率 |
| --- | --- | ---: |
| Codex Pool | OpenAI | `1x` |
| Claude Pool | Anthropic | `1x` |
| Enterprise Composite | Composite | `1x`，组合模式下不参与倍率解析 |

成员关系：

```text
Enterprise Composite
  OpenAI    -> Codex Pool
  Anthropic -> Claude Pool
```

客户配置：

| 客户 | 直接访问权限 | Codex Pool 专属倍率 | Claude Pool 专属倍率 |
| --- | --- | ---: | ---: |
| 企业 A | Enterprise Composite | `0.15x` | `0.35x` |
| 企业 B | Enterprise Composite | `0.12x` | `0.40x` |

最终计费：

| 客户 | Key 所属分组 | 请求模型 | 执行分组 | 最终倍率 |
| --- | --- | --- | --- | ---: |
| 企业 A | Composite | `gpt-*` | Codex Pool | `0.15x` |
| 企业 A | Composite | `claude-*` | Claude Pool | `0.35x` |
| 企业 B | Composite | `gpt-*` | Codex Pool | `0.12x` |
| 企业 B | Composite | `claude-*` | Claude Pool | `0.40x` |

如果之后给企业 A 增加 Codex Pool 直接访问权限：

| Key 所属分组 | 请求模型 | 执行分组 | 最终倍率 |
| --- | --- | --- | ---: |
| Codex Pool | `gpt-*` | Codex Pool | `0.15x` |
| Composite | `gpt-*` | Codex Pool | `0.15x` |

## 7. 目标数据模型

### 7.1 组合模式开关

在 `groups` 新增显式开关：

```sql
ALTER TABLE groups
    ADD COLUMN composite_member_routing_enabled BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE groups
    ADD CONSTRAINT groups_composite_member_routing_enabled_check
    CHECK (platform = 'composite' OR composite_member_routing_enabled = FALSE);
```

该字段是线上行为的唯一模式来源：

- `false`：旧模式，保持现有 Composite 账号调度和 Composite 计费。
- `true`：组合模式，只使用成员分组调度、策略和计费。

成员关系是否为空不能作为模式判断。保存、清空成员关系都不得隐式切换模式；启用和禁用必须是管理员的显式操作。

该字段需要进入 `Group` 和 API Key 认证分组快照。修改后沿用现有分组认证缓存失效链路，使各实例最终读取到新模式；跨实例切换的准确一致性边界见第 14 节。

### 7.2 Composite 成员关系

新增 `composite_group_members`：

```sql
CREATE TABLE composite_group_members (
    composite_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    member_group_id    BIGINT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (composite_group_id, member_group_id)
);

CREATE INDEX idx_composite_group_members_member
    ON composite_group_members(member_group_id);
```

成员完整替换在一次事务中完成，并按以下顺序执行：

1. `SELECT ... FOR UPDATE` 锁定 Composite 分组行，使同一 Composite 的并发替换和模式切换串行化。
2. 按 ID 排序后锁定现有成员和请求中的全部成员分组行。
3. 校验成员并执行完整替换。
4. 如果本次 `PUT` 请求的 `enabled=true`，按启用条件校验替换后的完整配置，再与开关一起提交。

事务内校验：

- `composite_group_id` 必须是 Composite。
- `member_group_id` 必须是普通分组。
- 不允许自引用。
- 一个 Composite 每个具体平台最多一个成员分组。
- 保存草稿时成员分组必须未删除；`enabled=true` 时还必须处于活跃状态。
- 本次 `enabled=true` 时，成员分组不能配置 `fallback_group_id` 或 `fallback_group_id_on_invalid_request`。

MVP 不在关系表中冗余 `platform`。平台取自成员分组，避免两份平台字段发生漂移。同平台唯一性由 Composite 行锁保证并发安全，不新增第二份平台字段只为建立唯一索引。所有 Composite 路由的新增、修改、启用和删除也必须先锁定同一 Composite 行，再读取成员关系并提交，使路由写入、成员替换和模式切换共享同一个串行化边界。

所有修改成员分组平台、fallback 字段或软删除分组的写路径必须在事务内锁定目标分组行并检查成员引用：平台修改或软删除在存在任何引用时拒绝；新增 fallback 在存在已启用 Composite 引用时拒绝。仅被禁用 Composite 引用时可以新增 fallback，但之后必须清除 fallback 才能启用。停用成员分组作为运营紧急开关仍允许；缓存按第 14 节约定收敛，观察到停用状态后的请求必须失败关闭并记录可审计错误。

仓库当前使用 `deleted_at` 软删除分组，因此外键的 `ON DELETE` 不会承担上述业务约束：

- 软删除 Composite 时，在同一事务内删除其 `composite_group_members` 关系行。
- 软删除成员分组时，在同一事务内检查引用并拒绝。
- 修改成员分组平台时执行同样的引用检查。
- 数据库外键只作为硬删除的最后一道完整性保护。

### 7.3 用户专属倍率

继续使用现有 `user_group_rate_multipliers`，不新增 Composite 专属倍率表。

现有主键已经满足需求：

```text
PRIMARY KEY (user_id, group_id)
```

其中 `group_id` 在本方案中始终是实际执行的普通分组 ID。

### 7.4 用量日志

在 `usage_logs` 新增：

```sql
routed_group_id BIGINT NULL REFERENCES groups(id) ON DELETE RESTRICT
```

记录规则：

- `group_id`：入口分组，Composite 请求记录 Composite ID。
- `routed_group_id`：执行分组；普通分组请求可以为空或记录与 `group_id` 相同。MVP 建议普通分组留空，减少历史数据和索引体积。
- `rate_multiplier`：本次请求最终生效倍率快照。

`ON DELETE RESTRICT` 保证即使未来出现硬删除，也不会抹掉历史路由归属；日常软删除不会影响历史日志。这样既保持现有报表归属，又能解释 Composite 请求为什么使用某个倍率。

## 8. 路由与计费流程

```text
验证 API Key 和入口分组权限
  -> 识别入口分组
  -> 普通分组：执行分组 = 入口分组
  -> Composite：按模型解析目标平台
  -> 按目标平台解析唯一成员分组
  -> 使用执行分组进行账号调度
  -> 使用 user_id + execution_group_id 解析倍率
  -> 计算请求费用
  -> 扣减入口分组对应的 Key/订阅额度
  -> usage_logs 同时记录入口分组和执行分组
```

### 8.1 执行分组解析接口

将复杂性集中在调度前的一个 seam：

```go
type GroupExecution struct {
    EntryGroup     *Group
    ExecutionGroup *Group
    Route          *CompositeRouteDecision
}

func ResolveGroupExecution(
    ctx context.Context,
    entryGroup *Group,
    model string,
    endpoint string,
) (*GroupExecution, error)
```

调用方只需要知道本次请求的入口分组和执行分组，不需要自行判断 Composite、平台或成员映射。

普通分组直接返回自身。Composite 旧模式同样返回自身，继续使用现有行为；Composite 组合模式解析现有模型路由或内置模型检测，再将目标平台映射到成员分组。

`EntryGroup` 和 `ExecutionGroup` 都必须是请求开始时加载的可信快照，后续调用方不得原地修改。解析顺序固定为：

```text
入口 Composite 路由：public_model + endpoint -> target_platform + upstream_model
成员映射：target_platform -> execution_group
执行分组渠道映射：upstream_model -> final_upstream_model
```

Composite 路由负责公开别名和目标平台；执行分组渠道负责成员池内部模型映射、模型限制和定价。组合模式不得再读取 Composite 渠道定价。若同一请求同时命中 Composite 路由和成员渠道映射，按上面顺序串联并在 `model_mapping_chain` 中完整记录。

### 8.2 调度结果

账号调度函数接收已解析的 `GroupExecution`，并始终使用 `ExecutionGroup.ID` 查询账号池：

```go
selection, err := SelectAccount(ctx, execution.ExecutionGroup, model, exclusions)
```

不在 `AccountSelectionResult` 中重复保存另一个执行分组来源。`GroupExecution` 由 handler 持有，并从路由一路显式传入策略、调度和用量记录。异步 worker 使用新的 context，`RecordUsageInput` 必须携带本次 `EntryGroup` 与 `ExecutionGroup` 快照，不能依赖请求 context，也不能按账号反推。

### 8.3 倍率解析

倍率优先级保持现有规则：

```text
用户在执行分组上的专属倍率
> 执行分组默认倍率
> 系统默认倍率
```

伪代码：

```go
multiplier := ResolveUserGroupRateMultiplier(
    ctx,
    user.ID,
    executionGroup.ID,
    executionGroup.RateMultiplier,
)
```

高峰倍率、图片独立倍率、视频独立倍率等读取 `executionGroup`，不能继续读取 `apiKey.Group`。否则 token 倍率和媒体倍率可能来自不同分组。

现有计费函数中所有通过 `apiKey.Group` 或 `apiKey.GroupID` 读取价格的 helper，都必须改为显式接收 `billingGroup = execution.ExecutionGroup`。`apiKey` 仍用于 API Key ID、入口归属和额度扣减，不能再兼任计费分组参数。

### 8.4 配额和扣费归属

计费金额按执行分组倍率计算，但资金和额度归属保持入口语义：

| 项目 | 使用的分组 |
| --- | --- |
| 用户专属倍率 | 执行分组 |
| 分组默认倍率 | 执行分组 |
| 高峰/图片/视频倍率 | 执行分组 |
| 账号池与利润控制 | 执行分组 |
| API Key 配额 | 入口分组的 Key |
| 订阅额度 | 入口分组对应订阅 |
| 余额扣减 | 当前用户钱包 |
| 用量报表主归属 | 入口分组 |
| 路由审计归属 | 执行分组 |

MVP 不改变现有 RPM 语义。入口分组 RPM、用户全局 RPM 和平台配额继续按现有链路执行；成员分组上的用户 RPM override 不因 Composite 路由自动继承。若业务需要共享成员分组 RPM，应单独设计，避免把价格改造扩大为限流改造。

### 8.5 分组配置来源矩阵

组合模式按下表选择配置来源。未列出的新增分组字段必须先归类，不能默认读取 `apiKey.Group`：

| 配置或行为 | 来源 | 说明 |
| --- | --- | --- |
| Key 创建权限、状态、过期和 IP 限制 | 入口分组 / API Key | 保持现有认证语义 |
| Key 配额、订阅额度、余额扣减 | 入口分组 / 用户 | 资金归属不变 |
| 入口分组 RPM、用户全局 RPM、平台配额 | 入口分组 / 用户 | MVP 不继承成员 RPM override |
| Composite 显式路由、公开模型别名 | 入口分组 | 只负责目标平台和第一段模型映射 |
| 账号池、账号优先级、粘性会话 | 执行分组 | 粘性 key 必须包含执行分组 ID |
| 渠道模型映射、模型限制、渠道定价 | 执行分组 | Composite 渠道价格在组合模式下忽略 |
| 用户专属倍率、默认倍率、高峰倍率 | 执行分组 | 直连与 Composite 共用同一解析函数 |
| 图片/视频倍率和价格、搜索/Web Search/语音价格 | 执行分组 | 禁止从 `apiKey.Group` 读取 |
| 图片/Live/Messages 等能力开关 | 执行分组 | 必须在执行分组解析后检查 |
| Claude Code/Codex CLI、OAuth/privacy、reasoning effort 等限制 | 执行分组 | 约束实际账号池 |
| 利润控制开关、margin、buffer 和下游倍率 | 执行分组 | 利润门和最终计费使用同一执行组 |
| `fallback_group_id`、无效请求 fallback | 不支持 | 启用组合模式前拒绝此类成员 |
| 用量主报表和订阅归属 | 入口分组 | `usage_logs.group_id` 保持入口语义 |
| 路由审计 | 执行分组 | 写入 `routed_group_id` |

入口和执行策略都需要生效时，顺序是入口认证/额度预检、解析执行分组、执行策略检查、账号调度、按入口扣减。任何执行策略失败都不得回退到 Composite 自身配置。

## 9. Composite 路由规则的处理

保留现有 `composite_model_routes`：

- `public_model`、`match_type`、`endpoint` 和 `priority` 继续决定规则匹配。
- `target_platform` 继续决定目标平台。
- `upstream_model` 继续决定实际转发模型。
- 新的成员关系负责把 `target_platform` 映射成唯一执行分组。

不需要在每条模型路由上重复填写 `target_group_id`。例如几十条 OpenAI 模型规则都可以复用同一个 Codex 成员分组。

内置模型检测同样适用：`gpt-*` 解析为 OpenAI 后查找 OpenAI 成员分组，`claude-*` 解析为 Anthropic 后查找 Anthropic 成员分组。

组合模式下，Composite 关联渠道中的模型定价和普通渠道模型映射不再参与请求。需要公开别名时使用 `composite_model_routes`；成员分组自己的渠道映射和定价在解析到成员后生效。启用组合模式前的预览必须同时展示完整映射链、执行分组和最终定价来源，避免管理员误以为旧 Composite 渠道价格仍会生效。

## 10. 管理端配置流程

### 10.1 分组配置

1. 创建普通 Codex 分组并关联 OpenAI/Codex 账号。
2. 创建普通 Claude 分组并关联 Anthropic/Claude 账号。
3. 创建 Composite 分组。
4. 在 Composite 成员配置中选择：

```text
OpenAI    -> Codex 分组
Anthropic -> Claude 分组
```

5. 需要公开别名时，继续配置现有 Composite 模型路由。
6. 预览各公开模型的目标平台、成员分组、最终上游模型和定价来源。
7. 显式启用组合模式。

进入组合模式后，不再需要把成员账号复制到 Composite。

### 10.2 客户权限配置

只需要给客户开放希望其直接使用的入口：

```text
仅自动调度：Composite
自动调度 + 直连 Codex：Composite、Codex
自动调度 + 直连 Codex/Claude：Composite、Codex、Claude
```

Composite 内部调用成员分组不检查用户是否拥有成员分组的直接访问权限。

### 10.3 客户倍率配置

MVP 只保留一个写入入口：在普通成员分组的现有“用户专属倍率”页面配置。

例如：

```text
Codex 分组：
  企业 A -> 0.15x
  企业 B -> 0.12x

Claude 分组：
  企业 A -> 0.35x
  企业 B -> 0.40x
```

倍率配置不应按用户的 `allowed_groups` 过滤；管理员必须可以为只有 Composite 权限的用户设置成员分组倍率。

未来如果操作量证明有必要，可以在 Composite 页面增加客户倍率矩阵：

| 客户 | Codex 成员 | Claude 成员 |
| --- | ---: | ---: |
| 企业 A | `0.15x` | `0.35x` |
| 企业 B | `0.12x` | `0.40x` |

该矩阵只是现有成员分组倍率记录的投影视图，不能新增数据表或第二套倍率优先级。

## 11. 管理接口建议

新增成员关系接口：

```text
GET /api/v1/admin/groups/:id/composite-members
PUT /api/v1/admin/groups/:id/composite-members
```

`PUT` 使用完整替换语义，并在一个事务中校验后写入：

```json
{
  "member_group_ids": [101, 102],
  "enabled": false
}
```

管理员先以 `enabled=false` 保存草稿并预览；确认后使用相同成员列表提交 `enabled=true`。成员关系和模式开关在同一数据库事务中提交，不存在只更新其中一项的持久化中间状态。同一 Composite 的并发请求串行执行，后提交者覆盖先提交者的完整配置。

响应应展开成员分组的只读信息：

```json
{
  "enabled": false,
  "items": [
    {
      "group_id": 101,
      "name": "Codex Pool",
      "platform": "openai",
      "status": "active",
      "rate_multiplier": 1.0
    }
  ]
}
```

当本次 `PUT` 的 `enabled=true` 时，在锁定 Composite 行的事务中校验替换后的配置：

- 至少存在一个成员分组。
- 所有启用的 Composite 显式路由所指平台都有成员。
- 成员均为活跃普通分组，且没有跨分组 fallback 配置。

内置模型检测允许解析到未配置的平台；这类请求在运行时失败关闭。启用预览按当前全部成员平台各验证至少一个代表模型，不引入第二份“开放平台”配置。

禁用只切回旧模式，不删除成员关系。本次 `PUT` 请求的 `enabled=true` 时，如果替换后的成员列表缺少某个启用路由引用的平台，必须拒绝；管理员需在同一次请求中提交 `enabled=false`，或先调整路由。反过来，Composite 已启用时，新增或修改启用路由也必须检查目标平台已有成员。

复用并扩展现有路由预览接口，不新增第二个预览体系：

```text
POST /api/v1/admin/groups/:id/composite-routes/preview
```

预览始终模拟组合模式，不读取线上 `enabled` 来决定走旧模式，也不修改任何配置、选择真实账号或扣减额度。请求可选携带 `member_group_ids` 以预览尚未保存的替换方案；未携带时使用当前已保存的成员关系。这样已启用的 Composite 也能先预览改动，再通过一次 `PUT enabled=true` 原子替换，不需要先切回旧模式。请求还可携带 `user_id`、固定的 `pricing_at` 和一组用量；缺少 `user_id` 时只解析分组默认倍率，缺少用量时不计算金额。

```json
{
  "model": "gpt-enterprise",
  "endpoint": "responses",
  "member_group_ids": [101, 102],
  "user_id": 501,
  "pricing_at": "2026-08-13T10:00:00Z",
  "usage": {
    "input_tokens": 1000,
    "output_tokens": 500
  }
}
```

响应至少返回以下可核对信息：

```json
{
  "target_platform": "openai",
  "execution_group": {
    "id": 101,
    "name": "Codex Pool"
  },
  "mapping_chain": [
    {
      "source": "composite_route",
      "from": "gpt-enterprise",
      "to": "gpt-5"
    },
    {
      "source": "execution_group_channel",
      "from": "gpt-5",
      "to": "gpt-5-mini"
    }
  ],
  "final_upstream_model": "gpt-5-mini",
  "pricing_source": {
    "group_id": 101,
    "kind": "execution_group_channel",
    "billing_model": "gpt-5-mini"
  },
  "resolved_billing_inputs": {
    "rate_source": "user_group",
    "base_rate_multiplier": 0.15,
    "peak_rate_multiplier": 1.0,
    "effective_rate_multiplier": 0.15,
    "pricing_at": "2026-08-13T10:00:00Z",
    "usage": {
      "input_tokens": 1000,
      "output_tokens": 500
    },
    "components": []
  },
  "actual_cost_preview": 0.00123
}
```

`components` 使用现有计费组件的类型、数量、单价和来源，不另造一套价格公式。预览必须调用生产路由、渠道映射、倍率和无副作用的计费计算 resolver，不能调用扣款或写用量链路；如果账号选择前无法唯一确定上游模型或价格，则返回 `actual_cost_preview=null` 和明确的 `unavailable_reason`，不能猜测价格或回退到 Composite 定价。

用户专属倍率继续使用现有原分组接口。若未来增加 Composite 客户倍率矩阵，后端必须按指定成员分组做局部 upsert/clear，不能用“同步用户全部倍率”的接口保存局部矩阵，否则会误删用户在其他分组上的倍率。

## 12. 校验与错误处理

### 12.1 配置时校验

- 所有保存都校验结构不变量：目标分组是 Composite，成员是未删除的普通分组，不自引用、不嵌套、不重复，同一具体平台最多一个成员。
- 选中的成员必须未删除。
- `enabled=false` 允许空成员列表、停用或带 fallback 的成员，以及未覆盖全部路由平台，用于保存不完整草稿或紧急切回旧模式；它不改变线上旧模式行为。
- `enabled=true` 额外要求至少一个成员、所有成员均活跃且没有 `fallback_group_id` 或 `fallback_group_id_on_invalid_request`，并要求每条启用的显式路由都有对应平台成员。
- 已启用 Composite 的路由新增、启用或目标平台修改，必须执行相同的成员覆盖校验。
- 成员被任何 Composite 引用时，不允许软删除或修改平台；被已启用 Composite 引用时不允许新增 fallback。成员状态允许停用，作为紧急关闭入口。
- Composite 有成员关系时，不允许改成普通平台；应先以 `enabled=false` 清空成员。

任何校验失败都回滚成员列表和 `enabled`，不能提交半套配置。结构或请求参数错误返回 `400`；被引用、并发后的最终配置不满足启用条件等状态冲突返回 `409`。错误响应需要指出具体 `group_id`、平台和原因，供管理端直接定位。

### 12.2 请求时失败关闭

组合模式下，以下情况都在账号调度前失败：

- 模型无法解析目标平台。
- 目标平台没有成员分组。
- 成员已删除、停用或不再是普通分组。
- 最终模型映射或执行分组策略不允许该请求。

缺少成员时使用稳定错误码：

```text
COMPOSITE_MEMBER_GROUP_NOT_CONFIGURED
```

成员不可用时使用：

```text
COMPOSITE_MEMBER_GROUP_UNAVAILABLE
```

不得回退到：

- Composite 直接关联的账号。
- 其他平台的成员分组。
- 全局未分组账号。
- Composite 自身倍率。

外部响应不暴露内部组名、账号或价格细节；内部日志记录 Composite ID、目标平台、成员 ID（如已解析）、路由来源和失败原因。失败请求不得产生成功用量记录或任何额度扣减。这样可以避免请求成功但按错误倍率计费。

## 13. 历史兼容与迁移

历史 Composite 只保存了“账号属于 Composite”的关系，没有保存账号来自哪个原分组，因此不能可靠自动迁移。

迁移新增字段默认 `false`，不回填成员关系，也不根据账号关系推断模式。模式只由 `composite_member_routing_enabled` 决定。

### 13.1 旧模式

`composite_member_routing_enabled=false` 时，无论是否已保存成员草稿：

- 保持当前账号调度逻辑。
- 保持当前 Composite 倍率逻辑。
- 现有客户和 Key 不受影响。

### 13.2 组合模式

`composite_member_routing_enabled=true` 时：

- 所有显式路由的平台都必须存在对应成员分组。
- 调度只查询执行分组账号池。
- 倍率只查询执行分组。
- 不再读取 Composite 直接关联账号作为兜底。

管理端需要明确显示当前模式，并在启用组合模式前提示管理员补齐将要开放的平台成员。

部署和迁移顺序：

1. 先执行只新增表/字段且默认关闭的向后兼容 schema migration，确认旧版本仍可运行，再部署读取新 schema 的后端。
2. 创建或确认 Codex、Claude 等普通分组及账号关联。
3. 配置企业用户在普通分组上的专属倍率。
4. 以 `enabled=false` 保存目标 Composite 的成员草稿。
5. 使用预览和测试 Key 验证路由链、定价来源、倍率和费用输入。
6. 以相同成员列表提交 `enabled=true`。
7. 等待第 14 节缓存链路收敛后，验证 `actual_cost` 及用量日志中的 `group_id`、`routed_group_id` 和 `rate_multiplier`。
8. 观察稳定后再移除 Composite 上冗余的直接账号关联；在此之前可通过提交 `enabled=false` 快速回到旧模式。

已启用后回滚应用版本前，必须先禁用所有组合模式 Composite。旧版本不知道成员关系，不能把新模式请求等价地还原。

## 14. 缓存与一致性

本方案保证单个请求内快照一致，不承诺数据库提交与所有实例在同一时刻切换。`ResolveGroupExecution` 完成后，该请求及其异步计费始终使用同一份入口组、执行组和计费输入快照；配置在请求中途变化，只影响后续新请求。

MVP 复用现有机制：

- `enabled` 进入 API Key 认证快照。开关变化所在事务由扩展后的现有分组触发器写入 `auth_cache_invalidation_outbox`；提交后再调用现有 `InvalidateAuthCacheByGroupID` 做即时失效，由 Redis pub/sub 通知各实例。分组触发器需把新开关纳入字段判断。
- MVP 不新增独立成员关系缓存。`target_platform -> member_group` 使用关系表索引查询，因此成员替换本身不需要另一套失效协议。
- 普通成员分组的状态、账号池和调度字段更新继续产生现有 `scheduler_outbox.group_changed`；账号关系更新继续走现有账号事件。成员替换不复制账号，也不要求全量重建 scheduler snapshot。
- 普通成员分组的计费字段更新继续失效其直接 Key 的认证缓存；Composite 请求不从入口 Key 快照复制成员计费字段，而是在解析执行组时读取成员的可信快照。
- 异步计费输入必须携带请求时解析的执行分组 ID，以及计算费用所需的不可变分组/映射快照，不能在 worker 中按账号反推或重新读取当前 Composite 配置。

认证缓存失效链路是跨实例最终一致：健康状态下先经 outbox worker 删除 Redis 并广播，本地订阅者再清除缓存，随后还有延迟安全重试。启用接口成功只表示数据库事务和失效事件已提交，不表示所有旧请求瞬时消失；切换期间已经拿到旧认证快照的请求按旧模式完成，新请求一旦观察到新快照就全程按新模式完成，禁止一次请求混用两种模式。

用户倍率缓存目前是各 Gateway/OpenAI 服务实例内独立缓存，没有跨实例失效，默认 TTL 为 30 秒。因此 MVP 明确接受倍率修改后最多一个配置 TTL 的收敛窗口，并在管理端提示“价格修改可能延迟生效”。验收最终费用一致性应在缓存收敛后或使用冷缓存执行。如果业务要求倍率保存后立即全局生效，再把 `user_id + group_id` 作为新消息类型接入现有 Redis 失效发布/订阅；不要为本功能另建消息系统。

账号可能同时属于多个普通分组，因此按账号反查执行分组没有确定答案，不能作为兜底实现。

## 15. 测试与验收标准

### 15.1 核心计费验收

给定：

```text
所有分组默认倍率 = 1x
企业 A + Codex = 0.15x
企业 A + Claude = 0.35x
企业 B + Codex = 0.12x
企业 B + Claude = 0.40x
```

对 Gateway 与 OpenAI 两条计费主干，使用相同 `user_id`、`execution_group_id`、执行分组配置快照、`billable_model`、`usage` 和固定 `pricing_at` 分别走直连与 Composite，必须逐项断言 `rate_multiplier`、所有费用组件和最终 `actual_cost` 完全一致，而不只验证倍率。至少验证：

- A 的 Composite Key 调用 Codex 使用 `0.15x`。
- A 的 Composite Key 调用 Claude 使用 `0.35x`。
- B 的 Composite Key 调用 Codex 使用 `0.12x`。
- B 的 Composite Key 调用 Claude 使用 `0.40x`。
- A 获得 Codex 直接权限后，用 Codex Key 仍使用 `0.15x`。
- 用户未配置成员专属倍率时，回退成员分组默认 `1x`，不能回退 Composite 专属倍率。
- Composite 和成员故意配置不同的默认、高峰、媒体及渠道价格时，组合模式只使用成员配置。
- token、per-request、图片、视频、搜索、Web Search、语音和长上下文各至少有一个直连/Composite 最终费用对照用例。

### 15.2 权限验收

- 只有 Composite 权限的用户可以创建 Composite Key。
- 该用户不能创建 Codex 或 Claude Key。
- Composite 内部仍可以路由到 Codex、Claude 成员分组。
- 配置成员分组倍率不会自动增加用户的成员分组权限。

### 15.3 调度验收

- Codex 请求只从 Codex 成员分组选择账号。
- Claude 请求只从 Claude 成员分组选择账号。
- 同一账号属于多个分组时，仍以已解析的成员分组作为执行分组。
- 未配置目标平台成员时失败关闭。
- 成员停用后失败关闭且可审计；被引用成员的软删除、平台修改被拒绝，被已启用 Composite 引用时新增 fallback 被拒绝。
- 账号 failover 只在同一执行分组内发生，不能切换成员分组。
- Composite 路由映射先执行，随后只执行一次成员渠道映射，并记录完整 `model_mapping_chain`。

### 15.4 账务与日志验收

- Composite 订阅额度按最终 `actual_cost` 扣减。
- API Key 配额仍更新 Composite Key。
- `usage_logs.group_id` 为 Composite ID。
- `usage_logs.routed_group_id` 为成员分组 ID。
- `usage_logs.rate_multiplier` 为成员分组最终生效倍率。
- 余额、订阅、API Key 配额不会重复扣减。

### 15.5 回归范围

按第 8.5 节来源矩阵逐行建立表驱动或集成验收，至少覆盖：

- 入口来源：Key 创建权限、状态/IP 限制、Key 配额、订阅额度、余额、入口 RPM、用量主归属。
- 执行来源：账号池、优先级、粘性会话、渠道映射/限制/定价、用户与默认/高峰倍率、媒体/搜索/语音/长上下文定价。
- 执行策略：图片/Live/Messages 能力、Claude Code/Codex CLI、OAuth/privacy、reasoning effort、利润控制。
- 日志来源：入口 `group_id`、执行 `routed_group_id`、倍率和映射链快照。
- 失败语义：无成员、成员停用、能力拒绝、fallback 配置和禁止的跨组 failover 均不回退 Composite。
- 请求形态：Anthropic/Gemini/Gateway 与 OpenAI/Codex/Grok 主干，以及流式、非流式和异步 worker。

### 15.6 配置、预览与兼容性验收

- 保存非空或不完整的 `enabled=false` 草稿不改变线上旧模式；清空成员也不隐式切换模式。
- 同一 `PUT` 原子替换成员与 `enabled`；校验失败全部回滚，并发替换串行且无半套配置可见。
- `enabled=true` 拒绝空成员、停用成员、重复平台、Composite 成员、fallback 成员和未覆盖的启用显式路由；`enabled=false` 可以保留停用或带 fallback 的成员草稿。
- 已启用路由写入不能制造无成员平台；引用保护覆盖软删除、平台修改和 fallback 修改路径。
- 预览可使用尚未保存的 `member_group_ids`，并返回目标平台、执行分组、最终模型、完整映射链、价格来源、倍率与费用输入；无法确定费用时明确返回不可用原因。
- schema 升级后全部历史 Composite 保持 `enabled=false` 的旧行为；显式禁用后仍保留成员草稿并恢复旧行为。
- 模式开关提交产生认证缓存失效记录；纯成员替换不产生无意义的认证失效，成员账号和调度字段修改继续产生 scheduler outbox 事件。
- 缓存切换期间单请求不混用模式；用户倍率修改在配置 TTL 内收敛，超过 TTL 不得继续返回旧值。

## 16. 实施拆分

建议按以下顺序实施，每一步都保持可测试：

1. 新增显式开关、成员关系和 `routed_group_id` schema/migration；默认关闭并补齐软删除、平台/fallback 修改保护。
2. 实现原子成员 `GET/PUT`、启用校验、现有 outbox 接入及扩展后的预览接口。
3. 引入统一 `GroupExecution`，先让普通分组和 Composite 旧模式通过它运行，验证无行为变化。
4. 接入组合模式解析，让两套调度主干及全部执行策略显式使用执行分组。
5. 将两套计费主干、利润控制和异步 worker 改为消费同一执行组快照，完成 `routed_group_id` 双写。
6. 增加管理端成员草稿、预览和显式启停；不在 MVP 增加客户倍率矩阵。
7. 完成来源矩阵、最终费用对照、缓存收敛及历史兼容回归后，再对单个 Composite 灰度启用。

## 17. 预计改动范围

这是跨两套网关主干的中大型改造。以下仅用于排期，Ent 生成代码不作为复杂度指标：

| 范围 | 预计 |
| --- | ---: |
| 手写生产代码 | `1,200-1,900` 行 |
| 测试代码 | `900-1,500` 行 |
| Ent 生成代码 | `2,500-4,500` 行 |
| 总 diff | `4,600-7,900` 行 |
| 手工涉及文件 | `30-45` 个 |

主要模块：

- Ent schema 和 SQL migration。
- Composite 成员仓储及管理接口。
- Composite 路由解析。
- Gateway 和 OpenAI 两套账号调度。
- Gateway 和 OpenAI 两套用量计费。
- 异步用量记录参数。
- 用量日志及查询 DTO。
- 管理端 Composite 成员草稿、预览和启停。
- 认证缓存失效触发器及现有 scheduler outbox 接入。
- 单元测试和集成测试。

不建议一期增加 Composite 客户倍率矩阵。现有普通分组倍率页面已经能为任意用户配置倍率，先用它完成 MVP；只有实际操作量证明需要批量视图时再做投影 UI。

## 18. 风险与控制

| 风险 | 控制措施 |
| --- | --- |
| 隐蔽 helper 仍误用 Composite 分组 | 配置来源矩阵逐行审计；直连/Composite 最终费用对照测试锁定 |
| 同一请求不同阶段解析出不同执行组 | 请求开始只创建一个 `GroupExecution`，后续显式传递且不可变 |
| 异步 worker 丢失或重算执行分组 | 不依赖请求 context，任务参数携带执行组和计费输入快照 |
| 两段模型映射顺序错误或重复应用 | 固定 Composite 路由后接成员渠道映射，并记录/测试完整映射链 |
| 部分平台未配置导致错误兜底 | 组合模式失败关闭，不使用 Composite 账号或倍率兜底 |
| 权限配置意外暴露成员分组 | 成员关系不写 `user_allowed_groups` |
| Composite 页面产生第二份倍率 | 所有倍率只写现有 `user_group_rate_multipliers` |
| 历史 Composite 无法判断账号来源 | 不自动迁移，保留旧模式并由管理员显式配置 |
| 软删除绕过数据库外键 | 在所有软删除/平台/fallback 更新路径检查引用，硬删除外键只作兜底 |
| 启用时多实例短暂观察到不同模式 | 复用 durable auth outbox + Redis 广播；保证单请求快照一致，灰度时等待健康链路收敛 |
| 用户倍率保存后短暂不一致 | MVP 明示默认最多 30 秒 TTL；严格实时要求出现时扩展现有 Redis 失效消息 |
| 成员停用或调度缓存滞后 | 复用 `scheduler_outbox`，观察到不可用后失败关闭并监控 outbox lag |

## 19. 最终结论

采用“Composite 只路由、原分组统一定价”的方案：

```text
Composite = 客户入口 + 自动选择执行分组
普通分组 = 账号池 + 用户专属倍率 + 默认倍率
```

该方案能够直接满足不同企业的 Codex/Claude 专属倍率，并通过唯一执行组和同一计费函数保证 Composite 与原分组直连的最终费用一致。权限和倍率保持解耦，客户不需要获得成员分组权限，系统也不维护第二份 Composite 平台倍率、价格表或缓存系统。

MVP 的明确边界是：每个平台一个成员、无跨组 fallback、显式启停、失败关闭、单请求快照一致；跨实例模式切换沿用现有缓存失效链路，用户倍率修改沿用当前默认 30 秒 TTL。只有这些边界无法满足实际运营要求时，再分别扩展同平台选组或倍率实时失效。
