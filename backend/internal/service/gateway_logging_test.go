package service

import (
	"context"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type gatewayLogSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *gatewayLogSink) WriteLogEvent(event *logger.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *gatewayLogSink) find(t *testing.T, message string) *logger.LogEvent {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event != nil && event.Message == message {
			return event
		}
	}
	t.Fatalf("no log event with message %q", message)
	return nil
}

// captureGatewayLogEvents 走 logger.SetSink，也就是 OpsSystemLogSink 在生产里挂的同一个口子，
// 所以断言的就是最终写进 ops_system_logs 的那份数据。
func captureGatewayLogEvents(t *testing.T) *gatewayLogSink {
	t.Helper()
	if err := logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output:      logger.OutputOptions{ToStdout: false, ToFile: false},
		Sampling:    logger.SamplingOptions{Enabled: false},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}
	sink := &gatewayLogSink{}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	return sink
}

// TestGatewayLog_BindsComponentAndCorrelationFields 锁住 #103 的两条验收前提：
// 转发链路的日志必须带 component（否则按 component 统计覆盖率看不到 service.gateway），
// 也必须带 trace_id 等关联字段。
func TestGatewayLog_BindsComponentAndCorrelationFields(t *testing.T) {
	sink := captureGatewayLogEvents(t)

	for _, tc := range []struct {
		name      string
		message   string
		component string
		emit      func(context.Context, string)
	}{
		{
			name:      "gatewayLog",
			message:   "gateway-log-probe",
			component: componentGateway,
			emit:      func(ctx context.Context, msg string) { gatewayLog(ctx).Warn(msg) },
		},
		{
			name:      "openaiGatewayLog",
			message:   "openai-gateway-log-probe",
			component: componentOpenAIGateway,
			emit:      func(ctx context.Context, msg string) { openaiGatewayLog(ctx).Warn(msg) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// ctx 里只有 ctxkey，没有中间件塞的 logger —— 走 FromContext 的兜底重建，
			// 这也是后台 goroutine 的真实形态。
			bgCtx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-gw")
			bgCtx = context.WithValue(bgCtx, ctxkey.RequestID, "req-gw")
			tc.emit(bgCtx, tc.message)

			event := sink.find(t, tc.message)
			if got, _ := event.Fields["component"].(string); got != tc.component {
				t.Errorf("component = %q, want %q", got, tc.component)
			}
			if got, _ := event.Fields["trace_id"].(string); got != "trace-gw" {
				t.Errorf("trace_id = %q, want trace-gw", got)
			}
			if got, _ := event.Fields["request_id"].(string); got != "req-gw" {
				t.Errorf("request_id = %q, want req-gw", got)
			}
		})
	}
}

// TestGatewayLog_ComponentMatchesCtxPrintfLiteral 保证同一条转发链路上
// 结构化日志和 printf 风格日志用的是同一个 component，不会在 OpenObserve 里分裂成两个桶。
func TestGatewayLog_ComponentMatchesCtxPrintfLiteral(t *testing.T) {
	sink := captureGatewayLogEvents(t)

	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-mix")
	gatewayLog(ctx).Warn("component-consistency-probe")
	logger.CtxPrintf(ctx, componentGateway, "component-consistency-printf failed")

	structured := sink.find(t, "component-consistency-probe")
	printf := sink.find(t, "component-consistency-printf failed")

	structuredComponent, _ := structured.Fields["component"].(string)
	printfComponent, _ := printf.Fields["component"].(string)
	if structuredComponent != printfComponent {
		t.Errorf("component mismatch: gatewayLog=%q CtxPrintf=%q", structuredComponent, printfComponent)
	}
	if structuredComponent != "service.gateway" {
		t.Errorf("component = %q, want service.gateway (与全仓 CtxPrintf 首参字面量一致)", structuredComponent)
	}
	// printf 风格的行要打 ctx_printf 而不是 legacy_printf，后者是「未迁移」的度量口径。
	if v, _ := printf.Fields[logger.CtxPrintfField].(bool); !v {
		t.Errorf("CtxPrintf line missing ctx_printf marker: %v", printf.Fields)
	}
	if _, ok := printf.Fields[logger.LegacyPrintfField]; ok {
		t.Errorf("CtxPrintf line must not carry legacy_printf: %v", printf.Fields)
	}
}
