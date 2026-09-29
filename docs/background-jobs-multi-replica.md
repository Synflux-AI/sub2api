# 后台周期任务的多副本语义

本文记录同一实例跑多个 sub2api 副本（共享同一套 PostgreSQL + Redis）时，各周期性后台任务如何避免重复执行。来源：Issue #203。

> 本文只覆盖后台任务。开多副本还有请求路径上的硬前置（管理员 OAuth 会话、compat 续链、WSv2 `previous_response_id`、转发 IP 设置、`TOTP_ENCRYPTION_KEY` 必须同值等），见 #203 的 2026-09-29 复核评论，另行跟踪。

## 两种锁语义

| 语义 | 实现 | 适用 |
|---|---|---|
| 互不重叠 | `tryAcquireSingletonLeaderLock`：每轮开头抢锁、结束立即释放 | 「本轮该不该做」由 DB 状态/水位决定的任务，重复执行幂等 |
| **全局每周期一次** | `tryAcquireCycleLeaderLock` / `releaseLeaderLockAfter`：锁持有到「抢锁时刻 + 0.9 × 周期」，本轮更久则结束即释放 | 重复执行有副作用（扣分、告警、上游请求、写指标行）的任务 |

「每轮即释放」在多副本下会退化成每周期执行 N 次：各副本计时器相位错开，锁一释放下一个副本就能拿到。持有时长取周期的 90%，保证单副本下一次 tick 一定能重新拿到锁、不会漏跑。

共同约定：

- 优先 Redis（`SETNX` + 按 owner 比较删除），Redis 报错时回退 PostgreSQL advisory lock，两者都没有时不设闸门（单实例/单测）。
- advisory lock 回退路径一律本轮结束即释放，避免为持锁长期占用连接池；此时语义退化为「互不重叠」。
- 锁 TTL 只是崩溃兜底：持有者崩溃后最迟 TTL 到期由其他副本接管，TTL 会被自动抬到不小于持有时长。
- 锁加在单轮入口，不加在 `Start()`，否则一个副本长期独占、故障时无人接管。

## Leader-only 任务（全局每周期一次）

| 任务 | 锁 key | 持有时长 / 崩溃 TTL | 不加锁的后果 |
|---|---|---|---|
| `BatchImageCleanupService` | `leader:lock:batch_image:cleanup:leader` | 0.9 × 清理周期 / 2 × 周期（单轮以此为超时） | 重复删除上游对象、审计事件 ×N、`retry_count` 重复累加；Vertex 404 提前返回会让 GCS 残留孤儿 |
| `CNProviderBalanceCheckService` | `leader:lock:cn_provider:balance_check:leader` | 0.9 × 检测周期 / 6 分钟 | 探测 ×N；各副本按不同时刻结果暂停/恢复账号互相抖动 |
| `ScheduledTestRunnerService` | `leader:lock:scheduled_test:runner:leader` | 54s / 6 分钟 | `ListDue` 不认领，同一计划被执行 N 次（上游请求、结果行、auto-recover）；同时防止单副本上一轮未完成时下一分钟重扫 |
| V1 `ChannelMonitorRunner` | `leader:lock:channel_monitor:v1:<monitor_id>:leader`（按 monitor） | 0.9 × (间隔 − 抖动) / 单次检测超时 | 每个 monitor 探测 ×N（消耗真实 key）、历史行 ×N |
| `AccountTTFTMonitorService` | `leader:lock:sched:ttft:monitor:leader` | 0.9 × 巡检周期 / 周期 + 45s | 健康分每周期扣 N 次，账号更快掉熔断层 |
| `AccountErrorRateMonitorService` | `ops:account_errrate:monitor:leader` | 54s / 运维锁配置 TTL（不足时抬高） | 冷却与剥离状态在进程内，同一次破阈值告警 ×N、剥离截止被顺延 |
| `OpsMetricsCollector` | `ops:metrics:collector:leader` | 0.9 × 采集间隔 / max(90s, 持有时长) | 每周期 N 行指标，面板 `LIMIT 1` 读到随机副本的 CPU/内存 |

补充说明：

- V1 渠道监控的 CRUD 钩子只会通知处理请求的那个副本。runner 每 60 秒从 DB 对账一次任务表（新建/间隔或抖动变化则重建，删除/停用/解密失败则取消），其余副本最迟一分钟内感知变化。
- 错误率监控的连续破阈值计数仍在进程内。持锁到周期末尾后 leader 基本固定在同一副本，但 leader 切换时计数会从头累计（最多推迟一次告警，不会误报）。
- 指标采集写入的是 leader 副本自己的 CPU/内存，不是全体副本的汇总。

## 已有互斥、无需改造的任务

`upstream_billing_probe`（本身即按周期持锁）、`subscription_expiry`、`dashboard_aggregation`、`backup_service`、`payment_order_expiry`、`ops_aggregation`、`ops_scheduled_report`、`ops_alert_evaluator`、`ollama_cloud_usage`、`openai_quota_auto_reset`、`opencode_go_usage`（库内 `fetchedAt` 间隔下限）。

## 重复执行幂等、不加 leader lock 的任务

| 任务 | 理由 |
|---|---|
| `TokenRefreshService` | 按账号 Redis 锁（`oauth:refresh_lock:`）+ 调用侧处理 `LockHeld`；多副本只是重复扫描与抢锁空转 |
| `AccountExpiryService` | `UPDATE ... WHERE schedulable = TRUE ... RETURNING`，第二个副本更新 0 行 |
| `ProxyExpiryService` | 事务内条件 UPDATE，第二个副本 `changed=0` |
| `IdempotencyCleanupService` | 重复删除，后到者删 0 行 |
| `OpenAICodexVersionSyncService` | 只多几次 GitHub 请求，写入值相同 |
| `SchedulerSnapshotService` | 桶锁 + epoch + 版本 CAS 防旧快照覆盖；outbox 清理已有 advisory lock；重复消费幂等 |
| `auth_cache_invalidation_outbox`、`usage_cleanup`、prompt audit | `SKIP LOCKED` 领取 |
| `batch_image_worker` | Redis 可靠队列 + 按任务锁 + 行锁状态流转 |
| `channel_monitor_v2_aggregator` | 先删后插幂等 + `LEAST` 水位 |
