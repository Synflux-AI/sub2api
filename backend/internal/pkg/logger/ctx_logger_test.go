package logger

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// captureStdoutJSON 初始化 json 格式 logger、执行 emit，并返回 stdout 上的原始日志行。
// 返回原始字符串而非 map，便于断言重复 JSON key（zap 会逐字写重复 key，
// 而 json.Unmarshal 会静默保留最后一个）。
func captureStdoutJSON(t *testing.T, emit func()) []string {
	t.Helper()

	origStdout := os.Stdout
	origStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	// warn/error 走 stderr，这里也要接住，否则写满管道会阻塞。
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	os.Stdout = stdoutW
	os.Stderr = stderrW
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		os.Stdout = origStdout
		os.Stderr = origStderr
	}
	t.Cleanup(func() {
		restore()
		_ = stdoutR.Close()
		_ = stderrR.Close()
	})

	if err := Init(InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Caller:      true,
		Output: OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
		Sampling: SamplingOptions{Enabled: false},
	}); err != nil {
		t.Fatalf("Init() error: %v", err)
	}

	emit()

	// 跳过 Sync()：Windows 上对管道 fsync 会死锁。关掉写端即可读出缓冲内容。
	restore()
	_ = stdoutW.Close()
	_ = stderrW.Close()
	stdoutBytes, _ := io.ReadAll(stdoutR)
	stderrBytes, _ := io.ReadAll(stderrR)

	var lines []string
	for _, item := range strings.Split(string(stdoutBytes)+"\n"+string(stderrBytes), "\n") {
		if strings.TrimSpace(item) != "" {
			lines = append(lines, item)
		}
	}
	return lines
}

func findLine(t *testing.T, lines []string, needle string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no log line containing %q; got:\n%s", needle, strings.Join(lines, "\n"))
	return ""
}

func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse log json failed: %v, line=%s", err, line)
	}
	return payload
}

// requestScopedContext 模拟 RequestLogger() 中间件注入的 request-scoped logger。
func requestScopedContext() context.Context {
	l := With(
		zap.String("request_id", "req-abc"),
		zap.String("trace_id", "trace-xyz"),
		zap.String("client_request_id", "client-123"),
		zap.String("path", "/v1/messages"),
		zap.String("method", "POST"),
	)
	return IntoContext(context.Background(), l)
}

func TestCtxPrintf_CallerPointsToCallsite(t *testing.T) {
	lines := captureStdoutJSON(t, func() {
		CtxPrintf(requestScopedContext(), "service.gateway", "ctxprintf-caller-check")
	})

	payload := parseLine(t, findLine(t, lines, "ctxprintf-caller-check"))
	caller, _ := payload["caller"].(string)
	if !strings.Contains(caller, "ctx_logger_test.go:") {
		t.Fatalf("caller should point to the callsite, got: %s", caller)
	}
	if strings.Contains(caller, "logger.go:") {
		t.Fatalf("caller must not point into internal/pkg/logger, got: %s", caller)
	}
}

func TestCtxPrintf_CarriesRequestScopedFields(t *testing.T) {
	lines := captureStdoutJSON(t, func() {
		CtxPrintf(requestScopedContext(), "service.gateway",
			"[Forward] Upstream error (non-retryable): Account=%d(%s) Status=%d", 3, "crs15-max", 400)
	})

	line := findLine(t, lines, "Upstream error (non-retryable)")
	payload := parseLine(t, line)

	for key, want := range map[string]string{
		"request_id":        "req-abc",
		"trace_id":          "trace-xyz",
		"client_request_id": "client-123",
		"path":              "/v1/messages",
		"method":            "POST",
		"component":         "service.gateway",
	} {
		if got, _ := payload[key].(string); got != want {
			t.Errorf("field %s = %q, want %q (line=%s)", key, got, want, line)
		}
	}

	if legacy, _ := payload["legacy_printf"].(bool); !legacy {
		t.Errorf("legacy_printf should stay true as the printf-style marker (line=%s)", line)
	}
	// "error" 关键字应推导为 error 级别，与 LegacyPrintf 保持一致。
	if level, _ := payload["level"].(string); !strings.EqualFold(level, "error") {
		t.Errorf("level = %q, want error (line=%s)", level, line)
	}
	// 每个 key 只能出现一次，否则 OpenObserve 取值依赖解析器实现。
	assertNoDuplicateKeys(t, line, "component", "trace_id", "request_id", "client_request_id", "path", "method")
}

func TestCtxPrintf_FallsBackToGlobalLoggerWithoutCtxLogger(t *testing.T) {
	lines := captureStdoutJSON(t, func() {
		// 没有 IntoContext 的裸 ctx：不应 panic，也不应丢日志。
		CtxPrintf(context.Background(), "service.gateway", "ctxprintf-no-ctx-logger")
		// nil ctx 同样要能兜住。
		CtxPrintf(nil, "service.gateway", "ctxprintf-nil-ctx") //nolint:staticcheck // 显式覆盖 nil ctx 兜底路径
	})

	for _, needle := range []string{"ctxprintf-no-ctx-logger", "ctxprintf-nil-ctx"} {
		payload := parseLine(t, findLine(t, lines, needle))
		if got, _ := payload["component"].(string); got != "service.gateway" {
			t.Errorf("%s: component = %q, want service.gateway", needle, got)
		}
		if _, hasTrace := payload["trace_id"]; hasTrace {
			t.Errorf("%s: should not invent a trace_id when ctx carries none", needle)
		}
	}
}

func TestCtxPrintf_EmptyMessageIsDropped(t *testing.T) {
	lines := captureStdoutJSON(t, func() {
		CtxPrintf(requestScopedContext(), "service.gateway", "   ")
		CtxPrintf(requestScopedContext(), "service.gateway", "ctxprintf-sentinel")
	})

	// 只有 sentinel 那条应落盘，空消息与 LegacyPrintf 一样被丢弃。
	count := 0
	for _, line := range lines {
		if strings.Contains(line, "legacy_printf") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 legacy_printf line, got %d:\n%s", count, strings.Join(lines, "\n"))
	}
	findLine(t, lines, "ctxprintf-sentinel")
}

func TestCtxPrintf_EmptyComponentOmitsField(t *testing.T) {
	lines := captureStdoutJSON(t, func() {
		CtxPrintf(requestScopedContext(), "", "ctxprintf-no-component")
	})

	line := findLine(t, lines, "ctxprintf-no-component")
	payload := parseLine(t, line)
	if _, ok := payload["component"]; ok {
		t.Errorf("empty component should not emit the field, line=%s", line)
	}
	if got, _ := payload["trace_id"].(string); got != "trace-xyz" {
		t.Errorf("trace_id = %q, want trace-xyz", got)
	}
}

func TestC_ReturnsRequestScopedLoggerAndKeepsCaller(t *testing.T) {
	lines := captureStdoutJSON(t, func() {
		C(requestScopedContext()).Warn("c-alias-check", zap.String("component", "service.gateway"))
	})

	line := findLine(t, lines, "c-alias-check")
	payload := parseLine(t, line)

	if got, _ := payload["trace_id"].(string); got != "trace-xyz" {
		t.Errorf("trace_id = %q, want trace-xyz (line=%s)", got, line)
	}
	caller, _ := payload["caller"].(string)
	if !strings.Contains(caller, "ctx_logger_test.go:") {
		t.Fatalf("C(ctx) must not shift caller; got: %s", caller)
	}
}

func TestC_MatchesFromContext(t *testing.T) {
	ctx := requestScopedContext()
	if C(ctx) != FromContext(ctx) {
		t.Fatal("C must be a plain alias of FromContext")
	}
	//nolint:staticcheck // 显式覆盖 nil ctx
	if C(nil) != FromContext(nil) {
		t.Fatal("C(nil) must match FromContext(nil)")
	}
}

// assertNoDuplicateKeys 在原始 JSON 行上断言每个 key 最多出现一次。
func assertNoDuplicateKeys(t *testing.T, line string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if n := strings.Count(line, `"`+key+`":`); n > 1 {
			t.Errorf("key %q appears %d times in one log line; zap writes duplicate keys verbatim: %s", key, n, line)
		}
	}
}
