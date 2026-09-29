# 请求路径状态的多副本语义

本文记录同一实例跑多个 sub2api 副本（共享同一套 PostgreSQL + Redis，前面是无粘性的负载均衡，如 Swarm routing mesh）时，请求路径上原本存于进程内的状态如何处理。来源：Issue #203 的 2026-09-29 复核第三节。后台周期任务见 [background-jobs-multi-replica.md](background-jobs-multi-replica.md)。

## 已改为跨副本共享

| 状态 | 原先 | 现在 | 不共享的后果 |
|---|---|---|---|
| 管理员添加账号的 OAuth 会话（含 PKCE `code_verifier`），Claude / OpenAI / Gemini / Antigravity | 进程内 map | Redis `oauth:session:<platform>:<session_id>`，TTL 30 分钟（`pkg/redissession.Hybrid`）；Grok 原本就在 Redis | 生成链接与换取授权码打到不同副本时报 "session not found or expired"，约一半失败；滚动发版清空会话 |
| `/v1/messages`→Responses compat 续链（`previous_response_id`、turn state、续链禁用标记） | `sync.Map` | Redis `openai:compat_session:<sha256(账号,APIKey,prompt_cache_key)>`，TTL 同 `gateway.openai_ws.sticky_response_id_ttl_seconds` | A→B→A 时 A 用自己过期的 response_id 续链，而输入已裁剪到最新一轮，上游静默丢失中间轮次 |
| 系统设置缓存（含「信任转发 IP」策略、首页注入的公开设置、CSP frame-src） | 只刷新处理保存请求的那个副本；转发 IP 策略只在启动时读库，首页/CSP 缓存无 TTL | 保存后经 Redis pub/sub `settings_updated` 广播，其他副本从库重建全部设置缓存并触发 `onUpdate` 回调；另每 60 秒重读一次转发 IP 策略兜底 | 各副本 API Key IP 黑白名单判定不一致；在 A 上看后台以为已生效 |

共同约定：

- Redis 是唯一事实来源，不在本地保留副本，避免「本地旧值覆盖他人新值」。
- Redis 读写失败时回退进程内存储（单副本行为不变）；OAuth 会话只有写失败的那一条会留在本地。
- 转发 IP 策略的周期重读失败时保留上次值；库里的值非法时与启动一致按关闭处理。

## 启动断言：`TOTP_ENCRYPTION_KEY` 必须所有副本同值

该密钥除 TOTP 外还加密插件配置、渠道监控 API key、备份/图片存储 S3 secret、支付配置等。显式配置时，启动会在 `security_secrets` 表记录其单向指纹（`totp_encryption_key_fingerprint`，带域分隔的 sha256，不落明文）：首次启动写入，此后指纹不符即拒绝启动。

- 未配置时每个进程随机生成密钥，依赖它的功能本身被禁用，不做断言。但这样多副本（以及单副本重启）之间都无法解密彼此的密文，生产环境应显式配置。
- 有意轮换密钥且已重新加密存量密文时，删除该行后重启即可重新记录。

JWT secret 本来就存在 `security_secrets`，无此问题。

## 仍依赖连接亲和：HTTP 入口走 WSv2 上游时的 `previous_response_id`

`response_id → 上游 WS 连接` 绑定的是一条真实的 TCP 连接，不能跨进程共享。受影响的只有「HTTP 客户端 + `store=false` + 带 `previous_response_id` 续链」的请求：换副本后上游返回 `previous_response_not_found`，客户端收到 400。以下情况不受影响：

- 客户端直接用 WebSocket 接入：整条客户端连接始终在同一副本上。
- `store=true`：上游按 response_id 存储，任意连接都能续链。
- 走 HTTP 上游的请求。

turn state（`x-codex-turn-state`）仍在进程内，但客户端通常会自己回传，丢失只影响上游缓存命中，不影响正确性。

需要在多副本下支持这类请求时，二选一：

1. 负载均衡层按 `session_id` / `conversation_id` / `prompt_cache_key` 做一致性哈希（Swarm VIP 做不到，需要反向代理直接面向各任务，如 Caddy 以 `tasks.<service>` 做动态上游）。
2. 设置 `gateway.openai_ws.force_http: true`，让上游一律走 HTTP，放弃 WSv2 的连接内续链。

## 已知但未处理（有上界或只影响阈值）

- 其余设置缓存（错误处理规则、路由策略、用户分组倍率等）在其他副本上靠 5–60 秒 TTL 收敛；设置保存广播已让它们大多即时刷新，但不在 `refreshCachedSettings` 里的缓存仍按 TTL。
- 定时备份 cron 与 S3 客户端、图片存储设置、联网搜索 provider 管理器只在本机保存时重建。
- 按进程计数、阈值会放大 N 倍的：OpenAI 账号+模型瞬态熔断、OAuth 429 两分钟原地重试窗口、上游 WS 连接池预热数、图片并发限制器、无效 API key 爆破限流器。
- `order: start-first` 滚动期间新旧版本共用 schema：迁移本身有 advisory lock 串行化，但必须只做向后兼容的变更。
