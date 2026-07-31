package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traceid"
	"github.com/gin-gonic/gin"
)

// captureMiddlewareLogLines 用真实的 json encoder 把日志写到管道并返回原始行。
//
// 必须断言原始 JSON 而不是 testLogSink 的 Fields map：zap 会把重复 key 逐字写进
// JSON，而 map 只留最后一个，重复问题在 map 视角下完全不可见。
func captureMiddlewareLogLines(t *testing.T, run func()) []string {
	t.Helper()

	origStdout := os.Stdout
	origStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
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

	if err := logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Caller:      true,
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
		Sampling: logger.SamplingOptions{Enabled: false},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	run()

	// 跳过 Sync()：Windows 上对管道 fsync 会死锁。
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

func findLogLine(t *testing.T, lines []string, needle string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no log line containing %q; got:\n%s", needle, strings.Join(lines, "\n"))
	return ""
}

// assertKeyAppearsOnce 在原始 JSON 行上断言每个 key 最多出现一次。
func assertKeyAppearsOnce(t *testing.T, line string, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if n := strings.Count(line, `"`+key+`":`); n > 1 {
			t.Errorf("key %q appears %d times in a single log line (zap writes duplicate keys verbatim): %s", key, n, line)
		}
	}
}

// TestAccessLog_NoDuplicateJSONKeys 锁住 #103 的结构性前提：
// request base logger 与 access 日志字段不得互相重复。
func TestAccessLog_NoDuplicateJSONKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lines := captureMiddlewareLogLines(t, func() {
		r := gin.New()
		r.Use(RequestLogger())
		r.Use(Logger())
		r.POST("/v1/messages", func(c *gin.Context) { c.Status(http.StatusOK) })

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d", w.Code)
		}
	})

	line := findLogLine(t, lines, "http request completed")
	assertKeyAppearsOnce(t, line,
		"component", "request_id", "client_request_id", "trace_id", "path", "method")

	payload := map[string]any{}
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse access log json: %v, line=%s", err, line)
	}
	// component 必须仍是 http.access —— ops_system_log_sink.shouldIndex 依赖它。
	if got, _ := payload["component"].(string); got != "http.access" {
		t.Errorf("access log component = %q, want http.access", got)
	}
	if got, _ := payload["path"].(string); got != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", got)
	}
	if got, _ := payload["method"].(string); got != http.MethodPost {
		t.Errorf("method = %q, want POST", got)
	}
	if got, _ := payload["trace_id"].(string); got == "" {
		t.Error("access log lost trace_id")
	}
}

// TestRequestScopedLogger_HasNoPreboundComponent 是 CtxPrintf 迁移的前提：
// base logger 不能预绑 component，否则业务侧再追加 component 就会产生重复 key。
func TestRequestScopedLogger_HasNoPreboundComponent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lines := captureMiddlewareLogLines(t, func() {
		r := gin.New()
		r.Use(RequestLogger())
		r.POST("/v1/messages", func(c *gin.Context) {
			// 模拟迁移后的 service 层调用。
			logger.CtxPrintf(c.Request.Context(), "service.gateway",
				"[Forward] Upstream error (non-retryable): Account=%d Status=%d", 3, 400)
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		r.ServeHTTP(w, req)
	})

	line := findLogLine(t, lines, "Upstream error (non-retryable)")
	assertKeyAppearsOnce(t, line,
		"component", "request_id", "client_request_id", "trace_id", "path", "method")

	payload := map[string]any{}
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse json: %v, line=%s", err, line)
	}
	if got, _ := payload["component"].(string); got != "service.gateway" {
		t.Errorf("component = %q, want service.gateway", got)
	}
	for _, key := range []string{"trace_id", "request_id", "path", "method"} {
		if got, _ := payload[key].(string); got == "" {
			t.Errorf("service log missing %s (line=%s)", key, line)
		}
	}
	// caller 必须指向业务调用点，而不是 internal/pkg/logger。
	if caller, _ := payload["caller"].(string); !strings.Contains(caller, "request_logger_fields_test.go:") {
		t.Errorf("caller = %q, want the callsite in this test file", caller)
	}
}

// TestRequestScopedLogger_EmptyClientRequestIDIsOmitted：空 client_request_id
// 不应提前绑定，否则后续 middleware 再补真值就成了重复 key。
func TestRequestScopedLogger_EmptyClientRequestIDIsOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lines := captureMiddlewareLogLines(t, func() {
		r := gin.New()
		r.Use(RequestLogger())
		r.GET("/t", func(c *gin.Context) {
			logger.C(c.Request.Context()).Info("no-client-request-id-probe")
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t", nil))
	})

	line := findLogLine(t, lines, "no-client-request-id-probe")
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse json: %v, line=%s", err, line)
	}
	if _, ok := payload["client_request_id"]; ok {
		t.Errorf("empty client_request_id should be omitted, got line=%s", line)
	}
	if got, _ := payload["trace_id"].(string); got == "" {
		t.Errorf("trace_id must still be present, line=%s", line)
	}
}

// TestRequestScopedLogger_NonEmptyClientRequestIDIsBound 用生产的中间件顺序：
// 全局 RequestLogger() 在前，按路由挂载的 ClientRequestID() 在后
// （internal/server/router.go:59 vs internal/server/routes/gateway.go:38）。
// 这正是重复 client_request_id 的现场，也要保证真值不被省掉。
func TestRequestScopedLogger_NonEmptyClientRequestIDIsBound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lines := captureMiddlewareLogLines(t, func() {
		r := gin.New()
		r.Use(RequestLogger())
		r.Use(ClientRequestID())
		r.GET("/t", func(c *gin.Context) {
			logger.C(c.Request.Context()).Info("client-request-id-probe")
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t", nil))
	})

	line := findLogLine(t, lines, "client-request-id-probe")
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse json: %v, line=%s", err, line)
	}
	if got, _ := payload["client_request_id"].(string); got == "" {
		t.Errorf("client_request_id should be bound when present, line=%s", line)
	}
	assertKeyAppearsOnce(t, line, "client_request_id", "trace_id", "request_id")
}

// TestRequestScopedLogger_PreexistingClientRequestIDIsBound 覆盖 ClientRequestID()
// 的"ctx 已有非空值"分支。该分支原先不绑 logger，靠 RequestLogger() 的空值预绑兜着；
// 预绑取消后它必须自己绑，否则这条路径的日志会丢 client_request_id。
//
// 注意：入站 X-Client-Request-ID 头从不被读进 ctx（#60 的实例自生成语义），
// 所以这里用中间件显式注入来触发该分支，而不是设置请求头。
func TestRequestScopedLogger_PreexistingClientRequestIDIsBound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const injected = "11111111-2222-3333-4444-555555555555"
	lines := captureMiddlewareLogLines(t, func() {
		r := gin.New()
		r.Use(RequestLogger())
		r.Use(func(c *gin.Context) {
			ctx := context.WithValue(c.Request.Context(), ctxkey.ClientRequestID, injected)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
		r.Use(ClientRequestID())
		r.GET("/t", func(c *gin.Context) {
			logger.C(c.Request.Context()).Info("preexisting-client-request-id-probe")
			c.Status(http.StatusOK)
		})

		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t", nil))
	})

	line := findLogLine(t, lines, "preexisting-client-request-id-probe")
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse json: %v, line=%s", err, line)
	}
	if got, _ := payload["client_request_id"].(string); got != injected {
		t.Errorf("client_request_id = %q, want %q (line=%s)", got, injected, line)
	}
	assertKeyAppearsOnce(t, line, "client_request_id")
}

// TestRequestLogger_RejectedTraceWarnCarriesHTTPComponent：入口拒绝日志
// 自己派生 component=http，不再依赖 base logger 预绑。
func TestRequestLogger_RejectedTraceWarnCarriesHTTPComponent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	lines := captureMiddlewareLogLines(t, func() {
		r := gin.New()
		r.Use(RequestLogger())
		r.GET("/t", func(c *gin.Context) { c.Status(http.StatusOK) })

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/t", nil)
		req.Header.Set(traceid.Header, strings.Repeat("x", 4096))
		r.ServeHTTP(w, req)
	})

	line := findLogLine(t, lines, "inbound X-Trace-Id rejected")
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("parse json: %v, line=%s", err, line)
	}
	if got, _ := payload["component"].(string); got != "http" {
		t.Errorf("component = %q, want http", got)
	}
	assertKeyAppearsOnce(t, line, "component", "trace_id", "request_id")
}
