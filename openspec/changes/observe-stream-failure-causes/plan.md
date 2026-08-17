# 流中断原因可观测化 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Anthropic 透传路径上三种「上游没把流跑完」的真实原因、以及第一级规则未命中的上游错误原文，全部可观测、可搜索、已脱敏——且客户端 wire 字节一字不改。

**Architecture:** 只在 service 层写入。三个终止点（`gateway_anthropic_passthrough.go:843/862/891`）各加一次记录，走「stdout 结构化 warn（计数通道）+ `upstream_error_message`/`upstream_errors`（后台可搜索通道）」双写。第一级规则未命中的 error 原文用 hit-priority 去重后在 attempt 终态落一条。所有写入前先过结构化脱敏、再截断。handler 层零改动，客户端可见文案天然逐字节不变。

**Tech Stack:** Go 1.x + gin + zap（`internal/pkg/logger`）+ PostgreSQL（`ops_error_logs`）。

**Spec:** `openspec/changes/observe-stream-failure-causes/design.md`（唯一事实来源，冲突时以 design.md 为准）

## Global Constraints

- **不改控制流**。不新增、不删除、不调整任何 `return` 分支的条件。`:830` 的 `!semanticEventForwarded && !sawAnyErrorEvent && !clientDisconnected && ctx.Err() == nil` 守卫原样保留。
- **不改 handler 层**。`ensureForwardErrorResponse` / `ensureAnthropicErrorResponse` / `recoverResponsesPanic` 一行不动。
- **不改客户端 wire**。既有用例 `TestPassthroughStreamUnmatchedErrorPreservesLegacyFallbackContract`、`TestPassthroughStreamErrorRuleReturnsCompleteEventOnce`、`TestPassthroughStreamCleanEOFRetriesThroughVirtual502`、`TestPassthroughStreamCleanEOFDoesNotRetryAfterSemanticOutput` 不改一行即须通过。
- **规则匹配用原始输入**。`decideErrorHandlingRule` 的 `respBody` 参数永远传未脱敏的原文；脱敏只作用于日志与 ops 字段。
- **脱敏在前、截断在后**。新增写入点一律 `truncateString(sanitizeUpstreamErrorPayload(x), maxBytes)`。
- **`upstream_status_code` 不写**。`setOpsUpstreamError` 第二参数传 `0`；cause 用 `OpsUpstreamErrorEvent.Reason` 表达。
- **敏感字段名清单不含 `signature`**。thinking block 签名错误的排查依赖看到该文案，且它不是凭证。
- 测试命令：`cd backend && go test ./internal/service/ -run <TestName> -v`；本变更全量回归 `cd backend && go test ./internal/service/ ./internal/repository/`；提交前 `cd backend && make test`（含 golangci-lint）。
- 每个 Task 结束必须 commit，msg 用 `feat(gateway):` / `test(gateway):` / `fix(ops):` 前缀，中文正文。

---

### Task 1: 上游错误内容结构化脱敏

**Files:**
- Create: `backend/internal/service/upstream_error_sanitize.go`
- Modify: `backend/internal/service/gemini_messages_compat_service.go:1767-1777`（移出 `sensitiveQueryParamRegex` 与 `sanitizeUpstreamErrorMessage`，保留 `retryInRegex`）
- Modify: `backend/internal/service/ops_upstream_context.go:245-249`（`Detail` 也脱敏）
- Test: `backend/internal/service/upstream_error_sanitize_test.go`

**Interfaces:**
- Consumes: 无（起点任务）。复用同包已有 `extractAnthropicSSEDataLine`（`gateway_anthropic_passthrough.go:959`）、`truncateString`（`ops_metrics_collector.go:928`）。
- Produces:
  - `func sanitizeUpstreamErrorPayload(payload string) string` — JSON/SSE/纯文本三态脱敏总入口
  - `func sanitizeUpstreamErrorMessage(msg string) string` — 保留原名原签名，委托给上者（**现有 104 个非测试调用点一行不改，全部自动获得增强**；强化是有意的全局行为变更，见 design.md「脱敏设计」）
  - `const upstreamSensitiveMask = "***"`

- [ ] **Step 1: 写失败测试**

`backend/internal/service/upstream_error_sanitize_test.go`：

```go
package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeUpstreamErrorPayloadMasksJSONFields(t *testing.T) {
	in := `{"error":{"message":"bad key","api_key":"sk-live-1234567890","nested":{"authorization":"Bearer abcdefghij"}}}`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk-live-1234567890")
	require.NotContains(t, out, "abcdefghij")
	require.Contains(t, out, upstreamSensitiveMask)
	require.Contains(t, out, "bad key")
}

func TestSanitizeUpstreamErrorPayloadMasksSSEDataLines(t *testing.T) {
	in := "event: error\n" + `data: {"type":"error","error":{"message":"denied","password":"hunter2hunter2"}}` + "\n"
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "hunter2hunter2")
	require.Contains(t, out, "event: error")
	require.Contains(t, out, "denied")
}

func TestSanitizeUpstreamErrorPayloadMasksPlainTextSecrets(t *testing.T) {
	in := `upstream rejected: api_key=sk-live-abcdefgh, Authorization: Bearer tok-0123456789 (see https://x.test/v1?access_token=zzzzzzzz)`
	out := sanitizeUpstreamErrorPayload(in)

	require.NotContains(t, out, "sk-live-abcdefgh")
	require.NotContains(t, out, "tok-0123456789")
	require.NotContains(t, out, "zzzzzzzz")
	require.Contains(t, out, "upstream rejected")
}

func TestSanitizeUpstreamErrorPayloadKeepsSignatureText(t *testing.T) {
	in := "Invalid `signature` in `thinking` block"
	require.Equal(t, in, sanitizeUpstreamErrorPayload(in))
}

func TestSanitizeUpstreamErrorPayloadPreservesQuotingInText(t *testing.T) {
	out := sanitizeUpstreamErrorPayload(`token: "abcdefghij" trailing`)
	require.Equal(t, `token: "`+upstreamSensitiveMask+`" trailing`, out)
}

func TestSanitizeUpstreamErrorMessageStillMasksQueryParams(t *testing.T) {
	out := sanitizeUpstreamErrorMessage("call https://x.test/v1?key=secretvalue failed")
	require.NotContains(t, out, "secretvalue")
	require.True(t, strings.Contains(out, upstreamSensitiveMask))
}

func TestSanitizeUpstreamErrorPayloadEmptyInputUnchanged(t *testing.T) {
	require.Equal(t, "", sanitizeUpstreamErrorPayload(""))
	require.Equal(t, "   ", sanitizeUpstreamErrorPayload("   "))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd backend && go test ./internal/service/ -run TestSanitizeUpstreamErrorPayload -v`
Expected: FAIL，`undefined: sanitizeUpstreamErrorPayload`、`undefined: upstreamSensitiveMask`

- [ ] **Step 3: 实现脱敏**

新建 `backend/internal/service/upstream_error_sanitize.go`：

```go
package service

import (
	"encoding/json"
	"regexp"
	"strings"
)

// upstreamSensitiveMask 是所有被打码内容的统一替换值。
const upstreamSensitiveMask = "***"

var (
	// sensitiveQueryParamRegex 覆盖 URL query 里的凭证参数。
	// 由 gemini_messages_compat_service.go 迁入，行为保持不变。
	sensitiveQueryParamRegex = regexp.MustCompile(`(?i)([?&](?:key|client_secret|access_token|refresh_token)=)[^&"\s]+`)

	// upstreamSensitiveFieldRegex 匹配 JSON key / header 名。
	// 故意不含 signature：thinking block 签名错误的排查完全依赖看到该文案
	// （isThinkingBlockSignatureError，gateway_upstream_response.go:157），
	// 且它不是凭证。
	upstreamSensitiveFieldRegex = regexp.MustCompile(`(?i)^(x-)?(api[_-]?key|apikey|authorization|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|secret|password|passwd|token|cookie|session[_-]?id)$`)

	// upstreamSensitiveKVRegex 匹配纯文本里的 key=value / key: value。
	// 第 3、5 组捕获可选的开闭引号：必须把闭引号一起吃进匹配范围，否则替换后
	// 会留下一个多余的引号（`token: "abc"` → `token: "***""`）。
	upstreamSensitiveKVRegex = regexp.MustCompile(`(?i)\b(x-api-key|api[_-]?key|apikey|authorization|access[_-]?token|refresh[_-]?token|client[_-]?secret|secret|password|passwd|token)\b(\s*[:=]\s*)("?)([^"\s,;)}\]]+)("?)`)

	// upstreamBearerRegex 匹配 "Bearer <token>" 形态。
	upstreamBearerRegex = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`)
)

// sanitizeUpstreamErrorMessage 保留原名与签名，供既有调用点继续使用。
func sanitizeUpstreamErrorMessage(msg string) string {
	if msg == "" {
		return msg
	}
	return sanitizeUpstreamErrorPayload(msg)
}

// sanitizeUpstreamErrorPayload 按输入形态分派脱敏：JSON 递归、SSE 逐行、纯文本正则。
//
// 只用于日志与 ops 字段。规则匹配必须使用未改写的原始输入，否则关键字会被
// 掩码打断。
func sanitizeUpstreamErrorPayload(payload string) string {
	if strings.TrimSpace(payload) == "" {
		return payload
	}
	if isLikelySSEPayload(payload) {
		return sanitizeSSEPayload(payload)
	}
	return sanitizeUpstreamErrorPayloadValue(payload)
}

func isLikelySSEPayload(payload string) bool {
	return strings.Contains(payload, "data:") || strings.Contains(payload, "event:")
}

// sanitizeUpstreamErrorPayloadValue 处理「JSON 或纯文本」，不再识别 SSE，
// 避免 data 行内容本身含 "data:" 时递归。
func sanitizeUpstreamErrorPayloadValue(payload string) string {
	trimmed := strings.TrimSpace(payload)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var decoded any
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			if out, err := json.Marshal(maskJSONSecrets(decoded)); err == nil {
				return string(out)
			}
		}
	}
	return maskTextSecrets(payload)
}

func sanitizeSSEPayload(payload string) string {
	lines := strings.Split(payload, "\n")
	for i, line := range lines {
		if data, ok := extractAnthropicSSEDataLine(line); ok {
			lines[i] = "data: " + sanitizeUpstreamErrorPayloadValue(data)
			continue
		}
		lines[i] = maskTextSecrets(line)
	}
	return strings.Join(lines, "\n")
}

func maskJSONSecrets(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for k, v := range typed {
			if upstreamSensitiveFieldRegex.MatchString(strings.TrimSpace(k)) {
				typed[k] = upstreamSensitiveMask
				continue
			}
			typed[k] = maskJSONSecrets(v)
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = maskJSONSecrets(typed[i])
		}
		return typed
	case string:
		return maskTextSecrets(typed)
	default:
		return value
	}
}

func maskTextSecrets(text string) string {
	if text == "" {
		return text
	}
	text = sensitiveQueryParamRegex.ReplaceAllString(text, `$1`+upstreamSensitiveMask)
	text = upstreamBearerRegex.ReplaceAllString(text, "Bearer "+upstreamSensitiveMask)
	return upstreamSensitiveKVRegex.ReplaceAllStringFunc(text, func(match string) string {
		groups := upstreamSensitiveKVRegex.FindStringSubmatch(match)
		if len(groups) < 6 {
			return match
		}
		return groups[1] + groups[2] + groups[3] + upstreamSensitiveMask + groups[5]
	})
}
```

- [ ] **Step 4: 删除旧实现**

`backend/internal/service/gemini_messages_compat_service.go` 把 `1767-1777` 这一段：

```go
var (
	sensitiveQueryParamRegex = regexp.MustCompile(`(?i)([?&](?:key|client_secret|access_token|refresh_token)=)[^&"\s]+`)
	retryInRegex             = regexp.MustCompile(`Please retry in ([0-9.]+)s`)
)

func sanitizeUpstreamErrorMessage(msg string) string {
	if msg == "" {
		return msg
	}
	return sensitiveQueryParamRegex.ReplaceAllString(msg, `$1***`)
}
```

替换为：

```go
// sensitiveQueryParamRegex 与 sanitizeUpstreamErrorMessage 已迁至
// upstream_error_sanitize.go（同包）。
var retryInRegex = regexp.MustCompile(`Please retry in ([0-9.]+)s`)
```

`regexp` 导入仍被 `retryInRegex` 使用，不要删。

- [ ] **Step 5: 让 `Detail` 也脱敏**

`backend/internal/service/ops_upstream_context.go` 把 `245-249`：

```go
	ev.Message = strings.TrimSpace(ev.Message)
	ev.Detail = strings.TrimSpace(ev.Detail)
	if ev.Message != "" {
		ev.Message = sanitizeUpstreamErrorMessage(ev.Message)
	}
```

改为：

```go
	ev.Message = strings.TrimSpace(ev.Message)
	ev.Detail = strings.TrimSpace(ev.Detail)
	if ev.Message != "" {
		ev.Message = sanitizeUpstreamErrorMessage(ev.Message)
	}
	// Detail 承载上游响应体原文，直接写进 ops_error_logs.upstream_errors。
	// 这里是所有调用点的唯一汇聚处，作为兜底脱敏。
	if ev.Detail != "" {
		ev.Detail = sanitizeUpstreamErrorPayload(ev.Detail)
	}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `cd backend && go test ./internal/service/ -run 'TestSanitizeUpstreamError' -v`
Expected: 全部 PASS

Run: `cd backend && go test ./internal/service/`
Expected: PASS（既有用例不得回归）

- [ ] **Step 7: Commit**

```bash
cd backend && git add internal/service/upstream_error_sanitize.go internal/service/upstream_error_sanitize_test.go internal/service/gemini_messages_compat_service.go internal/service/ops_upstream_context.go
git commit -m "fix(ops): 上游错误内容改为结构化脱敏，Detail 字段一并覆盖

原 sanitizeUpstreamErrorMessage 只处理 URL query 里的 4 个参数，JSON/SSE
里的 api_key、authorization、token、password 一律原样落库。appendOpsUpstreamError
也只脱敏 Message、不脱敏承载上游响应体原文的 Detail。

改为 JSON 递归 + SSE 逐行 + 纯文本三态分派，并在 appendOpsUpstreamError
汇聚处兜底脱敏 Detail。敏感字段清单故意不含 signature：thinking block
签名错误的排查依赖看到该文案，且它不是凭证。"
```

---

### Task 2: 三个终止点记录真实 cause

**Files:**
- Create: `backend/internal/service/gateway_stream_failure.go`
- Modify: `backend/internal/service/gateway_anthropic_passthrough.go:843`、`:862`、`:891`
- Test: `backend/internal/service/gateway_stream_failure_test.go`

**Interfaces:**
- Consumes: Task 1 的 `sanitizeUpstreamErrorPayload`；同包已有 `setOpsUpstreamError`、`appendOpsUpstreamError`、`OpsUpstreamErrorEvent`（`ops_upstream_context.go`）。
- Produces:
  - `type streamFailureCause string` 及三个常量 `streamFailureMissingTerminal` / `streamFailureReadError` / `streamFailureIntervalTimeout`
  - `const opsUpstreamErrorKindStreamFailure = "stream_failure"`
  - `const streamFailureScopeBeforeFirstToken = "before_first_token"` / `streamFailureScopeAfterFirstToken = "after_first_token"`
  - `func (s *GatewayService) recordStreamFailureCause(ctx context.Context, c *gin.Context, account *Account, model string, cause streamFailureCause, message string, firstTokenSeen bool)`

- [ ] **Step 1: 写失败测试**

`backend/internal/service/gateway_stream_failure_test.go`：

```go
package service

import (
	"context"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func opsUpstreamEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok, "expected ops upstream errors on context")
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func TestPassthroughMissingTerminalRecordsOpsCause(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: ""}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{Enabled: false})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "missing terminal event")

	msg, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok)
	require.Equal(t, "stream usage incomplete: missing terminal event", msg)

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, opsUpstreamErrorKindStreamFailure, events[0].Kind)
	require.Equal(t, string(streamFailureMissingTerminal), events[0].Reason)
	require.Equal(t, streamFailureScopeBeforeFirstToken, events[0].Scope)
	require.True(t, events[0].Passthrough)
	// upstream_status_code 必须保持不写：wire 上游状态确实是 200，
	// 合成一个 5xx 会污染上游状态维度。
	require.Zero(t, events[0].UpstreamStatusCode)
}

func TestPassthroughMissingTerminalAfterFirstTokenRecordsScope(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_x","model":"claude-sonnet-4-5","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{Enabled: false})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.ErrorContains(t, err, "missing terminal event")

	events := opsUpstreamEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, streamFailureScopeAfterFirstToken, events[0].Scope)
}

func TestRecordStreamFailureCauseIsNilSafe(t *testing.T) {
	svc := &GatewayService{}
	require.NotPanics(t, func() {
		svc.recordStreamFailureCause(context.Background(), nil, nil, "m", streamFailureReadError, "boom", false)
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	require.NotPanics(t, func() {
		svc.recordStreamFailureCause(context.Background(), c, nil, "m", streamFailureReadError, "boom", false)
	})
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok, "account 缺失时不应写入 ops")
}
```

**实现者注意**：`TestPassthroughMissingTerminalRecordsOpsCause` 断言 `OpsUpstreamErrorMessageKey` 严格等于真实文案。若该断言失败，说明 `Forward` 返回路径上还有别处（如 `partialStreamUsageResult` 或 handler 之前的某处）覆盖了这个 key —— **不要改断言绕过**，先查清覆盖点并在报告里写明，由控制方裁决。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd backend && go test ./internal/service/ -run 'TestPassthroughMissingTerminal|TestRecordStreamFailureCause' -v`
Expected: FAIL，`undefined: opsUpstreamErrorKindStreamFailure` 等

- [ ] **Step 3: 实现记录函数**

新建 `backend/internal/service/gateway_stream_failure.go`：

```go
package service

import (
	"context"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// streamFailureCause 表达「上游没把流跑完」的结构性原因。
// 取值由代码位置决定，不靠解析文案——文案会随上游变化，代码位置不会。
type streamFailureCause string

const (
	// streamFailureMissingTerminal：流干净关闭但没有 message_stop 等终止事件。
	streamFailureMissingTerminal streamFailureCause = "missing_terminal_event"
	// streamFailureReadError：读取上游流时报错。
	streamFailureReadError streamFailureCause = "read_error"
	// streamFailureIntervalTimeout：两次数据之间超过配置的间隔上限。
	streamFailureIntervalTimeout streamFailureCause = "interval_timeout"
)

const opsUpstreamErrorKindStreamFailure = "stream_failure"

const (
	// 断流发生在首个语义事件之前 —— 客户端零字节，理论上可干净重试。
	streamFailureScopeBeforeFirstToken = "before_first_token"
	// 断流发生在首字之后 —— 重试会腐化流，只能观测。
	streamFailureScopeAfterFirstToken = "after_first_token"
)

// recordStreamFailureCause 把流中断的真实原因写进两个通道，不改变对客响应：
//
//   - stdout 结构化 warn：计数通道。请求经规则重试后最终成功时不会写
//     ops_error_logs 行，只有这里能拿到完整计数。
//   - upstream_error_message + upstream_errors：后台可搜索通道。
//
// upstream_status_code 故意不写（传 0）：wire 上游状态确实是 200，合成一个
// 5xx 会污染上游状态维度；cause 由 OpsUpstreamErrorEvent.Reason 表达。传 0
// 同时让 checkSkipMonitoringForUpstreamEvent 提前返回，不会误触发
// skip_monitoring 规则匹配。
func (s *GatewayService) recordStreamFailureCause(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	model string,
	cause streamFailureCause,
	message string,
	firstTokenSeen bool,
) {
	if account == nil {
		return
	}
	scope := streamFailureScopeBeforeFirstToken
	if firstTokenSeen {
		scope = streamFailureScopeAfterFirstToken
	}

	logger.FromContext(ctx).Warn("gateway.stream_failure",
		zap.String("cause", string(cause)),
		zap.String("scope", scope),
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("model", model),
		zap.Bool("passthrough", true),
		zap.String("message", sanitizeUpstreamErrorPayload(message)),
	)

	if c == nil {
		return
	}
	setOpsUpstreamError(c, 0, message, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:    account.Platform,
		AccountID:   account.ID,
		AccountName: account.Name,
		Passthrough: true,
		Kind:        opsUpstreamErrorKindStreamFailure,
		Reason:      string(cause),
		Scope:       scope,
		Message:     message,
	})
}
```

- [ ] **Step 4: 接入三个终止点**

`backend/internal/service/gateway_anthropic_passthrough.go`。三处都只在既有 `return` 之前插入一次调用，**不改任何条件判断**。

`:843`（缺 terminal 事件），把：

```go
					return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete: missing terminal event")
```

改为：

```go
					s.recordStreamFailureCause(ctx, c, account, model,
						streamFailureMissingTerminal, "stream usage incomplete: missing terminal event", firstTokenMs != nil)
					return resultWithUsage(), nil, fmt.Errorf("stream usage incomplete: missing terminal event")
```

`:862`（上游读错误），把：

```go
				return resultWithUsage(), nil, fmt.Errorf("stream read error: %w", ev.err)
```

改为：

```go
				s.recordStreamFailureCause(ctx, c, account, model,
					streamFailureReadError, fmt.Sprintf("stream read error: %v", ev.err), firstTokenMs != nil)
				return resultWithUsage(), nil, fmt.Errorf("stream read error: %w", ev.err)
```

`:891`（数据间隔超时），把：

```go
			return resultWithUsage(), nil, fmt.Errorf("stream data interval timeout")
```

改为：

```go
			s.recordStreamFailureCause(ctx, c, account, model,
				streamFailureIntervalTimeout, "stream data interval timeout", firstTokenMs != nil)
			return resultWithUsage(), nil, fmt.Errorf("stream data interval timeout")
```

注意 `:891` 上方 `:888-890` 的 `HandleStreamTimeout` 调用保持原位不动。

- [ ] **Step 5: 运行测试确认通过**

Run: `cd backend && go test ./internal/service/ -run 'TestPassthroughMissingTerminal|TestRecordStreamFailureCause' -v`
Expected: 全部 PASS

Run: `cd backend && go test ./internal/service/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
cd backend && git add internal/service/gateway_stream_failure.go internal/service/gateway_stream_failure_test.go internal/service/gateway_anthropic_passthrough.go
git commit -m "feat(gateway): 透传流中断的三个终止点记录真实 cause

missing_terminal_event / read_error / interval_timeout 三个出口此前都不写
ops，真实原因一路被 handler 的 ensureForwardErrorResponse 换成通用文案
Upstream request failed，落库后 upstream_error_message 为空。

改为在 service 层双写：stdout 结构化 warn 作计数通道（规则重试后成功的请求
不会写 ops_error_logs，只有这里拿得到完整计数），upstream_error_message +
upstream_errors 作后台可搜索通道。upstream_status_code 不写，避免用合成
5xx 污染上游状态维度。Scope 字段区分首字前/后断流，是后续可重试性改造的
可行性度量。

控制流零改动，handler 层零改动，客户端 wire 字节不变。"
```

---

### Task 3: 第一级规则未命中时记录上游错误原文

**Files:**
- Modify: `backend/internal/service/gateway_stream_failure.go`（追加类型与 flush 函数）
- Modify: `backend/internal/service/gateway_anthropic_passthrough.go:596-606`（改具名返回值 + defer）、`:761-783`（捕获未命中）
- Test: `backend/internal/service/gateway_stream_failure_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 的 `recordStreamFailureCause` 所在文件与常量。
- Produces:
  - `type unmatchedStreamErrorEvent struct { statusCode int; errType string; message string; detail string }`
  - `const opsUpstreamErrorKindStreamErrorUnmatched = "stream_error_unmatched"`
  - `func (s *GatewayService) flushUnmatchedStreamError(ctx context.Context, c *gin.Context, account *Account, model string, attempt int, ev *unmatchedStreamErrorEvent)`

- [ ] **Step 1: 写失败测试**

追加到 `backend/internal/service/gateway_stream_failure_test.go`：

```go
func TestPassthroughUnmatchedStreamErrorIsRecordedOnce(t *testing.T) {
	body := "event: error\ndata: " + streamRuleConcurrencyError + "\n\n" +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 200, body: body}}}
	// 规则限定 502，第一级用的是 429（rate_limit_error），必然未命中。
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "other", StatusCodes: []int{502}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.Error(t, err)

	events := opsUpstreamEvents(t, c)
	unmatched := 0
	for _, ev := range events {
		if ev.Kind == opsUpstreamErrorKindStreamErrorUnmatched {
			unmatched++
			require.Contains(t, ev.Message, "Concurrency limit exceeded")
		}
	}
	require.Equal(t, 1, unmatched, "同一 attempt 内连续多个未命中只记一条")
}

func TestPassthroughMatchedStreamErrorSuppressesUnmatchedRecord(t *testing.T) {
	body := "event: error\ndata: " + `{"type":"error","error":{"type":"api_error","message":"transient blip"}}` + "\n\n" +
		"event: error\ndata: " + streamRuleConcurrencyError + "\n\n"
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: body},
		{status: 200, body: streamRuleSuccess},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{streamErrorRule(ErrorHandlingActionRetry, 1, ErrorHandlingExhaustedActionDefault)},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.NoError(t, err)

	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		events, _ := v.([]*OpsUpstreamErrorEvent)
		for _, ev := range events {
			require.NotEqual(t, opsUpstreamErrorKindStreamErrorUnmatched, ev.Kind,
				"同一 attempt 内后续 error 命中规则时，未命中记录必须被抑制")
		}
	}
}

func TestPassthroughUnmatchedStreamErrorIsSanitized(t *testing.T) {
	payload := `{"type":"error","error":{"type":"api_error","message":"denied","api_key":"sk-live-abcdefgh"}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 200, body: "event: error\ndata: " + payload + "\n\n"},
	}}
	svc := newErrorHandlingRulePassthroughService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "other", StatusCodes: []int{502}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	// 必须打开 LogUpstreamErrorBody，否则 detail 恒为空，这条用例会空跑通过
	// 而完全没有验证脱敏。
	svc.cfg.Gateway.LogUpstreamErrorBody = true
	c, _ := newErrorHandlingRuleTestContextWithRecorder()

	_, err := svc.Forward(context.Background(), c, newErrorHandlingRulePassthroughAccount(), newErrorHandlingRuleStreamParsed(t))
	require.Error(t, err)

	events := opsUpstreamEvents(t, c)
	unmatchedSeen := false
	for _, ev := range events {
		require.NotContains(t, ev.Message, "sk-live-abcdefgh")
		require.NotContains(t, ev.Detail, "sk-live-abcdefgh")
		if ev.Kind == opsUpstreamErrorKindStreamErrorUnmatched {
			unmatchedSeen = true
			// detail 必须真的带上了原文（脱敏后），否则等于没测。
			require.Contains(t, ev.Detail, upstreamSensitiveMask)
			require.Contains(t, ev.Detail, "denied")
		}
	}
	require.True(t, unmatchedSeen)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd backend && go test ./internal/service/ -run 'TestPassthroughUnmatchedStreamError|TestPassthroughMatchedStreamErrorSuppresses' -v`
Expected: FAIL，`undefined: opsUpstreamErrorKindStreamErrorUnmatched`

- [ ] **Step 3: 追加类型与 flush 函数**

追加到 `backend/internal/service/gateway_stream_failure.go`：

```go
const opsUpstreamErrorKindStreamErrorUnmatched = "stream_error_unmatched"

// unmatchedStreamErrorEvent 是一次「上游发了 error 帧、但错误处理规则没匹配上」
// 的现场。运维配关键字全靠这份原文——现状是它被完全丢弃，只能靠持续监控猜。
type unmatchedStreamErrorEvent struct {
	statusCode int
	errType    string
	message    string
	detail     string
}

// flushUnmatchedStreamError 在 attempt 终态落一条未命中记录。
//
// 采用 hit-priority 去重：调用方保证同一 attempt 内只保留首条未命中，且一旦
// 后续 error 命中规则就整体丢弃——同一次故障只应产生一条记录，命中记录
// （error_handling_rule_matched）优先级更高。
func (s *GatewayService) flushUnmatchedStreamError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	model string,
	attempt int,
	ev *unmatchedStreamErrorEvent,
) {
	if ev == nil || account == nil {
		return
	}
	message := sanitizeUpstreamErrorPayload(ev.message)
	detail := sanitizeUpstreamErrorPayload(ev.detail)
	if s != nil && s.cfg != nil {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		if !s.cfg.Gateway.LogUpstreamErrorBody {
			detail = ""
		} else {
			detail = truncateString(detail, maxBytes)
		}
	}

	logger.FromContext(ctx).Warn("gateway.stream_error_event_unmatched",
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("model", model),
		zap.Int("attempt", attempt),
		zap.Int("status_code", ev.statusCode),
		zap.String("error_type", ev.errType),
		zap.String("message", message),
	)

	if c == nil {
		return
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: ev.statusCode,
		Passthrough:        true,
		Kind:               opsUpstreamErrorKindStreamErrorUnmatched,
		Message:            message,
		Detail:             detail,
	})
}
```

- [ ] **Step 4: 改具名返回值并加 defer**

`backend/internal/service/gateway_anthropic_passthrough.go:596-606`，把函数签名的返回值改为具名，其余参数不动：

```go
) (result *streamingResult, ruleMatch *anthropicPassthroughStreamRuleMatch, err error) {
```

（原为 `) (*streamingResult, *anthropicPassthroughStreamRuleMatch, error) {`）

在 `:607` 的 `if streamState == nil {` 之前插入：

```go
	var unmatchedStreamError *unmatchedStreamErrorEvent
	// hit-priority：只要这次 attempt 最终返回了规则命中，未命中记录一律丢弃。
	// 用 defer 覆盖所有 return 分支，包括后续新增的。
	defer func() {
		if ruleMatch != nil {
			return
		}
		s.flushUnmatchedStreamError(ctx, c, account, model, attempt, unmatchedStreamError)
	}()
```

具名返回值只用于 defer 判定，函数体内所有 `return resultWithUsage(), nil, ...` 写法保持原样，**不要改成裸 return**。

- [ ] **Step 5: 捕获未命中**

`backend/internal/service/gateway_anthropic_passthrough.go` 的 `processEvent` 里，把 `:775-781`：

```go
				if decision.Matched {
					return &anthropicPassthroughStreamRuleMatch{
						decision: decision, statusCode: statusCode, body: append([]byte(nil), event.data...),
						errType: errType, errMessage: errMessage, rawEvent: append([]byte(nil), event.raw...),
						semanticEventForwarded: semanticEventForwarded,
					}, nil
				}
```

改为：

```go
				if decision.Matched {
					unmatchedStreamError = nil
					return &anthropicPassthroughStreamRuleMatch{
						decision: decision, statusCode: statusCode, body: append([]byte(nil), event.data...),
						errType: errType, errMessage: errMessage, rawEvent: append([]byte(nil), event.raw...),
						semanticEventForwarded: semanticEventForwarded,
					}, nil
				}
				if unmatchedStreamError == nil {
					unmatchedStreamError = &unmatchedStreamErrorEvent{
						statusCode: statusCode,
						errType:    errType,
						message:    errMessage,
						detail:     string(event.data),
					}
				}
```

`detail` 存的是**未脱敏的原文**，脱敏发生在 `flushUnmatchedStreamError` 里（Step 3）。捕获点不脱敏是有意的：这份 `event.data` 与规则匹配用的是同一份数据，提前改写会让后续排查看到的东西与实际匹配输入不一致。

- [ ] **Step 6: 运行测试确认通过**

Run: `cd backend && go test ./internal/service/ -run 'TestPassthroughUnmatchedStreamError|TestPassthroughMatchedStreamErrorSuppresses' -v`
Expected: 全部 PASS

Run: `cd backend && go test ./internal/service/`
Expected: PASS。特别确认 `TestPassthroughStreamUnmatchedErrorPreservesLegacyFallbackContract` 未改一行且通过——它断言客户端收到的字节等于上游原始 error 帧。

- [ ] **Step 7: Commit**

```bash
cd backend && git add internal/service/gateway_stream_failure.go internal/service/gateway_stream_failure_test.go internal/service/gateway_anthropic_passthrough.go
git commit -m "feat(gateway): 第一级规则未命中时记录上游错误原文

规则引擎在 :772 确实被调用了，但未命中时上游 error 事件的原文既不落日志也
不落 ops，运维无从得知该配什么关键字，只能持续监控、不断补关键字。

改为 hit-priority 去重记录：同一 attempt 内只保留首条未命中，一旦后续 error
命中规则就整体丢弃（命中记录优先级更高），每 attempt 最多一条。用具名返回值
+ defer 覆盖全部 return 分支。原文先脱敏后截断，raw detail 受
LogUpstreamErrorBody 开关控制。

规则匹配仍使用未改写的原始输入。客户端 wire 字节不变。"
```

---

### Task 4: 后台错误日志搜索覆盖 `upstream_error_message`

**Files:**
- Modify: `backend/internal/repository/ops_repo.go:1016`
- Test: `backend/internal/repository/ops_repo_search_test.go`

**Interfaces:**
- Consumes: 无代码依赖；本任务让 Task 2/3 写入的 `upstream_error_message` 在后台可被搜到。
- Produces: 无导出符号，只改 SQL 过滤子句。

- [ ] **Step 1: 写失败测试**

`backend/internal/repository/ops_repo_search_test.go`（若文件已存在则追加）：

```go
package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 搜索子句必须覆盖 upstream_error_message，否则「流中断」类错误的真实 cause
// 写进该列后，后台按 "missing terminal event" 仍然搜不到。
func TestOpsErrorLogSearchClauseCoversUpstreamErrorMessage(t *testing.T) {
	clause := opsErrorLogSearchClause("$1")
	require.Contains(t, clause, "e.error_message ILIKE $1")
	require.Contains(t, clause, "e.upstream_error_message ILIKE $1")
	require.True(t, strings.HasPrefix(clause, "("))
	require.True(t, strings.HasSuffix(clause, ")"))
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd backend && go test ./internal/repository/ -run TestOpsErrorLogSearchClause -v`
Expected: FAIL，`undefined: opsErrorLogSearchClause`

- [ ] **Step 3: 抽出子句并补上该列**

`backend/internal/repository/ops_repo.go`，把 `:1016`：

```go
		clauses = append(clauses, "(e.request_id ILIKE $"+n+" OR e.client_request_id ILIKE $"+n+" OR e.trace_id ILIKE $"+n+" OR e.error_message ILIKE $"+n+")")
```

改为：

```go
		clauses = append(clauses, opsErrorLogSearchClause("$"+n))
```

并在同文件 `escapeLikePattern`（`:898`）附近新增：

```go
// opsErrorLogSearchClause 拼后台错误日志的关键字过滤子句。
//
// upstream_error_message 必须在内：流中断类错误（missing terminal event /
// read error / interval timeout）的真实 cause 只落在该列，error_message 记的
// 是 handler 补写的通用文案 "Upstream request failed"。少了这一列，后台按真实
// 文案搜索恒为 0 条。
func opsErrorLogSearchClause(placeholder string) string {
	cols := []string{
		"e.request_id",
		"e.client_request_id",
		"e.trace_id",
		"e.error_message",
		"e.upstream_error_message",
	}
	parts := make([]string, 0, len(cols))
	for _, col := range cols {
		parts = append(parts, col+" ILIKE "+placeholder)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}
```

确认 `ops_repo.go` 已导入 `strings`（该文件已有 `escapeLikePattern` 使用 `strings`，无需新增导入）。

- [ ] **Step 4: 运行测试确认通过**

Run: `cd backend && go test ./internal/repository/ -run TestOpsErrorLogSearchClause -v`
Expected: PASS

Run: `cd backend && go test ./internal/repository/`
Expected: PASS

- [ ] **Step 5: 全量回归**

Run: `cd backend && make test`
Expected: PASS（含 golangci-lint）

- [ ] **Step 6: Commit**

```bash
cd backend && git add internal/repository/ops_repo.go internal/repository/ops_repo_search_test.go
git commit -m "fix(ops): 后台错误日志关键字搜索覆盖 upstream_error_message

流中断类错误的真实 cause 只落在 upstream_error_message 列，error_message 记
的是 handler 补写的通用文案 Upstream request failed。搜索子句少了这一列，
后台按 missing terminal event 搜索恒为 0 条——实际 7 天发生 1272 次。"
```

---

## 上线后的观测动作

灰度 1–2 天后从 OpenObserve 取三组数字，作为 PR2（`normalize-stream-failure-rules`）设计定稿的输入：

```
gateway.stream_failure           按 cause 分组计数
gateway.stream_failure           按 scope 分组计数
gateway.stream_error_event_unmatched  按 message 分组，取 top 10
```

第三组直接回答「运维到底该配什么关键字」。若 top 10 收敛到少数几种文案，PR2 的两级匹配可能不必要，补关键字即可；若发散，归一化才是必要的。这个判断在 PR2 动手之前做。
