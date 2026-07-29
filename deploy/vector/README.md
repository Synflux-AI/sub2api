# Vector → OpenObserve 业务事件管道

对应 issue #104。「使用明细」和「错误请求」原先只落 Postgres，OO 侧完全查不到（例如按
`user_agent` 检索），因为 Vector 只采集容器 stdout。本目录把配置纳入仓库，并新增两条独立
stream。

`vector.yaml` 是三台生产机 `/opt/vector/vector.yaml` 的实际内容（2026-07-29 抄录时三台
byte 级一致，sha256 `fd6093ad5bd521c85683298aa5e7a3df3a79665040a1d16c7389fe429002448d`）
加上 #104 的改动，新增部分用 `#104` 注释标出。凭据与主机标签仍在各机
`/opt/vector/.env`（`OO_USER` / `OO_TOKEN` / `OO_HOST_LABEL`），不入仓库。

## 数据流

```
app (stdout：普通日志是 zap console 格式，业务事件是整行 JSON)
  → Docker json-file driver
    → Vector docker_logs source（include_containers 白名单 + multiline）
      → remap parse（JSON 分支 / console 分支）
        → route（按 event_kind）
          ├─ usage_log   → default/usage_logs
          ├─ error_log   → default/error_logs
          └─ _unmatched  → default/sub2api（原有应用日志流）
```

应用本身不与 OO 通信，只多打一行 stdout 日志。

## 为什么必须同时改 multiline 和 remap

生产的 `log.format` 是 **console**，不是 json。原配置里：

- `multiline.start_pattern` 只匹配 `^\d{4}-\d{2}-\d{2}T...`，配合 `mode: halt_before`，
  **不匹配的行会被当作续行追加到上一条消息**（这是 Go stack trace 能被聚合的原因）。业务事件
  以 `{"level":...` 开头，若不放宽这个正则，每条事件都会被静默粘到前一条日志尾部，两条一起坏掉。
- `remap parse` 按 `split(head, "\t")` 解析 `ts \t LEVEL \t [logger \t] caller \t msg \t {json}`，
  只 merge **行尾那段** JSON。整行纯 JSON 切出来 `n == 1`，进不了 `if n >= 4`，结果
  `.level="unknown"`、整行落进 `.message`，**`event_kind` 不会成为顶层字段**，route 永远匹配不到。

所以 #104 的 Vector 改动是三处，缺一不可：放宽 `start_pattern`/`condition_pattern`、给 remap
加 JSON 分支、加 `business_route` 并把原 `oo` sink 的 inputs 改成 `_unmatched`。

另外 `del(.stream)` 删的是 docker_logs 的 stdout/stderr 元字段，而业务事件里的 `stream`
（bool，是否流式请求）在其后的 merge 才写入 —— 这两步顺序不能调换。

## 语义边界

Postgres 是唯一主存储。这两条 stream 是 **best-effort 可观测投影，不是事务一致副本**：

- 事件在记录交给写入路径时同步打印，不进数据库事务，也不等落库确认；
- 因此 OO 中可能存在最终未落库的事件（唯一键冲突、队列满被丢弃、批量写失败）；
- 反之，`observability.business_events.enabled` 关闭期间落库的记录在 OO 里没有对应事件。

**不要用这两条 stream 做对账或计费聚合。** 计数时按 id 字段去重，权威数据查 Postgres。

## 事件 envelope

| 字段 | 说明 |
|---|---|
| `event_kind` | `usage_log` / `error_log`，Vector 按此精确路由 |
| `event_schema_version` | 当前为 `1` |
| `event_emitted_at` | emitter 产生事件的 UTC 时间（RFC3339Nano） |
| `db_created_at` | 对应 DB 记录的 `created_at` |
| `trace_id` | 入站链路 trace id（`X-Trace-Id`，缺失时以 request_id 兜底） |
| `request_id` | 本地 request-scoped request id |
| `client_request_id` | 客户端请求关联 id |
| `ops_system_log_skip` | 固定 `true`，防止事件回流进 `ops_system_logs` |

后台写入路径（live settlement、batch image）没有入站链路，`trace_id` 等字段会缺失而不是
输出空串 —— 每个字段在整条 stream 里保持单一类型。这类记录仍保留自身稳定的业务关联 ID
（`usage_request_id`，如 live 会话的 call hash）。

事件不含重复 JSON key：payload 中与 envelope 同名的字段会被丢弃，envelope 优先。

### usage 事件的 `usage_request_id`

`usage_logs.request_id` 实际是**计费/幂等键**（常见值 `client:...` / `local:...`），与链路语义的
`request_id` 不是一回事。因此它在事件里叫 `usage_request_id`，envelope 的 `request_id` 保持
链路语义。error 事件的 `ops_error_logs.request_id` 则与 envelope 同源（handler 直接从请求
上下文取），不再重复输出。

`db_id` 只在记录已有 ID 时出现。usage 的 best-effort 路径在写库前就投影，所以通常没有
`db_id` —— 不伪造。

## 不会出现在 OO 中的字段

error 事件默认 **不输出** 任何响应/请求正文：

- `error_body`
- `upstream_error_message` / `upstream_error_detail`
- `upstream_errors[]`（含 `upstream_response_body`）
- 任何 request / prompt / body / header 原文

应用侧的 sanitizer 只去除凭据，不去除 prompt、用户输入或 PII，所以「已经 sanitize」不等于
可以跨存储复制正文。若以后确需正文，另开 issue：独立高权限 stream + 更短留存 + 单独开关。

API Key 只输出 `api_key_id` 与已有的 8 位脱敏前缀，不输出明文。UA 沿用入口的合法 UTF-8
归一化与 512 bytes 上限。

usage 事件以 `usage_logs` 的 57 个持久化列为基线，两个字段刻意不投影：

- `image_size_breakdown`：per-size 计数的 map，是明细 blob 不是检索维度，投影成嵌套对象会
  让该字段的形状逐条不同；
- `UsageLog` 上的 `MediaType`：根本不是 `usage_logs` 的列。

`UsageLog` 的关联对象（User / APIKey / Account / Group / Subscription）一律不展开，否则会把
邮箱和 key material 带进 stream。两侧的字段集合都有 schema 测试锁定，新增 DB 列不会自动
泄露到 OO。

## OO stream 设置

两条新 stream 各自独立设置 retention。建议索引字段（`user_agent` **不**建索引，它基数高且
只用于点查）：

- `usage_logs`：`model`、`requested_model`、`upstream_model`、`account_id`、`user_id`、
  `api_key_id`、`group_id`
- `error_logs`：`model`、`status_code`、`account_id`、`user_id`、`api_key_id`、`error_type`、
  `error_phase`

## 部署顺序

必须先改 Vector，再发应用 —— 反过来会让业务事件在新 sink 存在之前落进 `default/sub2api`。
本仓库的 `vector.yaml` 已经是改好的完整版本，部署时先与目标机现状 diff 再覆盖：

```bash
scp deploy/vector/vector.yaml root@<HOST>:/tmp/vector-new.yaml
ssh root@<HOST> 'diff -u /opt/vector/vector.yaml /tmp/vector-new.yaml'
```

1. 三台机各自 diff 确认只有 `#104` 标注的改动，然后校验语法与 VRL —— 用一次性容器，不影响
   运行中的实例：

   ```bash
   ssh root@<HOST> 'docker run --rm \
     -v /tmp/vector-new.yaml:/etc/vector/candidate.yaml:ro \
     --env-file /opt/vector/.env \
     timberio/vector:0.57.0-alpine validate --no-environment /etc/vector/candidate.yaml'
   ```

   替换后重载 Vector，确认 `default/sub2api` 仍在正常 ingest、`level` 不是 `unknown`、
   stack trace 仍能聚合进 `stack_trace` 字段。
2. 在 OO 侧创建两条 stream 的 retention 与 index_fields。
3. 发布应用。`observability.business_events.enabled` 默认 `false`，此时行为与发布前一致。
4. 逐节点开启开关（配置或 `OBSERVABILITY_BUSINESS_EVENTS_ENABLED=true`），每台观察：
   - Docker 日志文件增长与轮转是否仍在预期内；
   - OO 两条新 stream 的 ingest 是否出现；
   - `default/sub2api` 是否**没有**混入 `event_kind` 事件；
   - 业务请求延迟、usage/error DB 写入吞吐无回归。
5. 三节点稳定后再调大 retention。

### 已完成的离线验证（2026-07-29）

用 `timberio/vector:0.57.0-alpine` 一次性容器，对**生产那份 remap** 做过：

- `vector validate --no-environment`：配置与 VRL 编译通过；
- `vector vrl` 灌入一条真实 emitter 输出的事件行：`event_kind` 成为顶层字段，
  `user_agent` / `model` / `status_code` / `trace_id` 均提升，`stream=true` 在 `del(.stream)`
  之后存活，`_timestamp` 取自事件自带时间，共 44 字段；
- 同一次灌入一条 zap console 行：`component=http.access`、`message`、`_timestamp` 与改动前
  一致，console 分支无回归；
- multiline 正则：JSON 行与 console 行都开启新消息，stack trace 续行不开启。

## 验收查询

开启后在 OO 上确认：

- `error_logs` 可按 `user_agent`、`model`、`account_id`、`status_code`、`error_type`、
  `trace_id` 查询；
- 选一组固定测试请求，用 `trace_id` 关联 OO 事件与 Postgres 行，核对字段值与 null/类型语义；
- `default/sub2api` 中 `event_kind` 存在的文档数为 0。

## 一致性检查

三台机器的配置必须一致（改动前后都应三台同值）：

```bash
for h in 144.126.209.169 134.199.209.111 129.212.166.106; do
  ssh root@$h 'sha256sum /opt/vector/vector.yaml'
done
```

`/opt/vector/.env` 的 `OO_HOST_LABEL` 三台**不同**，分别是 `linkyrouter-144` / `crs15-134` /
`aihezu-129`；三台都往同一个 OO 实例（`https://oo.aihezu.dev`）打，靠 `.host` 区分。

## 容量

改动前 OO 约 114 万 docs/24h。error 事件量级与 `ops_error_logs` 写入量一致；usage 事件
（PR2）预计新增约 16 万 docs/24h，文档数约 +15%。上线后除文档数外还要测量单条事件 bytes 的
P50/P95/P99、每日 ingest bytes、retention 后总存储，以及 256MB disk buffer 能承受的 OO
中断时长。

注意 Vector 的 buffer 与重试只保护**已经被 Vector 接收**的事件。应用侧的写入是同步的
stdout 写，与请求日志同一类，不设队列也不做关闭时 flush。
