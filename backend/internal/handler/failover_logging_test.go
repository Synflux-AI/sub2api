package handler

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type failoverLogSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *failoverLogSink) WriteLogEvent(event *logger.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *failoverLogSink) find(t *testing.T, message string) *logger.LogEvent {
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

// TestFailoverLog_CarriesComponentAndCorrelation 锁住 #103 的一处回归风险：
// 这些日志原先靠 RequestLogger() 预绑的 component="http" 兜着，取消预绑后
// 必须自己带 component，否则 ops_system_logs 会把它们记成 component="app"。
func TestFailoverLog_CarriesComponentAndCorrelation(t *testing.T) {
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
	sink := &failoverLogSink{}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })

	ctx := context.WithValue(context.Background(), ctxkey.TraceID, "trace-failover")
	failoverLog(ctx).Warn("gateway.failover_switch_account")

	event := sink.find(t, "gateway.failover_switch_account")
	if got, _ := event.Fields["component"].(string); got != componentGatewayFailover {
		t.Errorf("component = %q, want %q", got, componentGatewayFailover)
	}
	if got, _ := event.Fields["trace_id"].(string); got != "trace-failover" {
		t.Errorf("trace_id = %q, want trace-failover", got)
	}
}

// TestFailoverLoop_NoBareCtxLogger 禁止本文件重新出现裸的 logger.FromContext /
// logger.C —— 它们不带 component。统一走 failoverLog(ctx)，helper 自身除外。
func TestFailoverLoop_NoBareCtxLogger(t *testing.T) {
	const name = "failover_loop.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 %s 失败（文件是否被改名或删除？）: %v", name, err)
	}

	// failoverLog 就是正确出口，它内部必然要调 logger.FromContext。
	const exemptFunc = "failoverLog"

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "logger" {
			return true
		}
		switch sel.Sel.Name {
		case "FromContext", "C", "L", "LegacyPrintf":
		default:
			return true
		}
		if enclosingFunc(file, call.Pos()) == exemptFunc {
			return true
		}
		t.Errorf("%s: 禁止裸 logger.%s —— 改用 failoverLog(ctx)，它会绑上 component",
			fset.Position(call.Pos()), sel.Sel.Name)
		return true
	})
}

// enclosingFunc 返回包含 pos 的最内层函数声明名。
func enclosingFunc(file *ast.File, pos token.Pos) string {
	name := ""
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil {
			continue
		}
		if pos >= fn.Pos() && pos <= fn.End() {
			name = fn.Name.Name
		}
	}
	return name
}
