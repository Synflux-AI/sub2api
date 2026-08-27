package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// ctxLoggingMigratedFiles 是 #103 首阶段已迁移到 ctx 感知日志入口的转发链路文件。
//
// 这些文件里的日志必须走 logger.C(ctx) / logger.CtxPrintf(ctx, ...)，
// 才能自动带上 RequestLogger() 绑定的 trace_id / request_id / client_request_id。
// 本文件的两个测试是基线防回归：没有它，清掉的调用点会重新长回来。
//
// 全仓 forbidigo 等后续批次迁移完再启用；这里只锁住已经迁移完的转发路径。
// 后续 PR 迁移了新文件，把文件名加进这个列表即可。
var ctxLoggingMigratedFiles = []string{
	// Anthropic 转发关键路径
	"gateway_scheduling.go",
	"gateway_forward.go",
	"gateway_upstream_request.go",
	"gateway_upstream_response.go",
	"gateway_anthropic_passthrough.go",
	// 错误处理规则引擎：触发日志是排查/调优这套规则的唯一抓手，必须带关联 ID
	"gateway_error_handling_rule.go",
	"openai_error_handling_rule.go",
	// OpenAI-compat / responses / bedrock 流式路径
	"gateway_forward_as_responses.go",
	"openai_gateway_chat_completions_raw.go",
	"gateway_forward_as_chat_completions.go",
	"bedrock_stream.go",
	// Live/realtime：原先靠 base logger 预绑的 component="http" 兜着，
	// #103 取消预绑后改由 openaiLiveLog 显式带。
	"openai_live.go",
}

// knownCtxLoggingExemptions 是明确豁免的调用点：函数签名里没有 ctx，
// 且作用域内无可达 context，本阶段不改函数签名。
//
// key 是 "文件名:函数名"。后续给这些函数补 ctx 参数后，从这里删掉即可。
var knownCtxLoggingExemptions = map[string]string{
	"gateway_upstream_response.go:isThinkingBlockSignatureError": "该函数无 ctx 参数，4 处 [SignatureCheck] 日志待后续补 ctx 形参后迁移",
	"openai_live.go:openaiLiveLog":                               "component 绑定 helper 本身，它就是本文件其余调用点的正确出口",
}

// TestGatewayForwardPath_UsesCtxAwareLogging 禁止已迁移文件重新出现
// 无 ctx 的日志出口：logger.LegacyPrintf、logger.L()、裸 slog.Info/Warn/Error/Debug。
//
// 也禁止裸 logger.C(ctx)：它虽然带 ctx，但不带 component，日志会落进空 component 桶，
// #103 的验收口径（按 component 统计 trace_id 覆盖率）就看不到 service.* 从 0% 涨上来，
// ops_system_logs 里还会被记成 component="app"。转发链路统一走 gatewayLog / openaiGatewayLog。
//
// 用 AST 而不是文本匹配：注释和字符串字面量里的同名内容不会误报，
// 而 slog.Default().Enabled(ctx, ...) 这类 level 门控（不是日志发射）也能正确放过。
func TestGatewayForwardPath_UsesCtxAwareLogging(t *testing.T) {
	forbidden := map[string]map[string]string{
		"logger": {
			"LegacyPrintf": "改用 logger.CtxPrintf(ctx, component, format, args...)",
			"L":            "改用 gatewayLog(ctx) / openaiGatewayLog(ctx)",
			"C":            "改用 gatewayLog(ctx) / openaiGatewayLog(ctx)，裸 logger.C 不带 component",
			"FromContext":  "改用 gatewayLog(ctx) / openaiGatewayLog(ctx)，裸 FromContext 不带 component",
		},
		"slog": {
			"Info":  "改用 gatewayLog(ctx).Info(...)（注意 slog 的 key-value 变参要转成 zap.Field）",
			"Warn":  "改用 gatewayLog(ctx).Warn(...)（注意 slog 的 key-value 变参要转成 zap.Field）",
			"Error": "改用 gatewayLog(ctx).Error(...)（注意 slog 的 key-value 变参要转成 zap.Field）",
			"Debug": "改用 gatewayLog(ctx).Debug(...)（注意 slog 的 key-value 变参要转成 zap.Field）",
		},
	}

	forEachMigratedFile(t, func(t *testing.T, fset *token.FileSet, file *ast.File, name string) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, fn, ok := packageLevelCall(call)
			if !ok {
				return true
			}
			hint, forbiddenCall := forbidden[pkg][fn]
			if !forbiddenCall {
				return true
			}
			if reason, exempt := knownCtxLoggingExemptions[name+":"+enclosingFuncName(file, call.Pos())]; exempt {
				t.Logf("%s: 跳过豁免调用点 %s.%s —— %s",
					fset.Position(call.Pos()), pkg, fn, reason)
				return true
			}
			t.Errorf("%s: 禁止在已迁移的转发路径里使用 %s.%s —— %s",
				fset.Position(call.Pos()), pkg, fn, hint)
			return true
		})
	})
}

// TestGatewayForwardPath_NoBaseLoggerFieldCollision 禁止业务日志重新绑定
// RequestLogger() 已经绑在 request-scoped logger 上的字段。
//
// zap 的 JSON encoder 会把重复 key 逐字写出，取值依赖下游解析器实现。
// 迁移时踩到的真实例子：上游的 x-request-id 原本用 zap.String("request_id", ...) 打，
// 改走 ctx 后与本实例的 request_id 同名不同值挤在一个 key 上，
// 已按 #94 的三层 ID 分工改名为 upstream_request_id。
func TestGatewayForwardPath_NoBaseLoggerFieldCollision(t *testing.T) {
	// 与 internal/server/middleware/request_logger.go 里 base logger 绑定的字段保持一致。
	// component 不在此列：base logger 不再预绑它，业务侧显式带 component 是正确用法。
	baseLoggerFields := map[string]string{
		"request_id":        "本实例内部 request id 由 RequestLogger() 绑定；上游的 x-request-id 请用 upstream_request_id",
		"trace_id":          "由 RequestLogger() 绑定，跨 hop 串联用，不要在业务侧重复绑",
		"client_request_id": "由 ClientRequestID() 中间件绑定，计费幂等语义，不要在业务侧重复绑",
		"path":              "由 RequestLogger() 绑定；上游路径请换个字段名，如 upstream_path",
		"method":            "由 RequestLogger() 绑定；上游方法请换个字段名，如 upstream_method",
	}

	forEachMigratedFile(t, func(t *testing.T, fset *token.FileSet, file *ast.File, name string) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, _, ok := packageLevelCall(call)
			if !ok || pkg != "zap" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			key, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if hint, collides := baseLoggerFields[key]; collides {
				t.Errorf("%s: 字段 %q 与 request-scoped base logger 冲突，会产生重复 JSON key —— %s",
					fset.Position(lit.Pos()), key, hint)
			}
			return true
		})
	})
}

// forEachMigratedFile 解析每个已迁移文件并跑 check，同时保证列表本身不腐坏。
func forEachMigratedFile(t *testing.T, check func(*testing.T, *token.FileSet, *ast.File, string)) {
	t.Helper()
	for _, name := range ctxLoggingMigratedFiles {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			// 文件被改名或删除时直接失败，避免基线静默失效。
			t.Fatalf("解析 %s 失败（文件是否被改名或删除？若已重命名请同步更新 ctxLoggingMigratedFiles）: %v", name, err)
		}
		check(t, fset, file, name)
	}
}

// packageLevelCall 把 `pkg.Fn(...)` 形式的调用拆成包名和函数名。
// 只匹配直接的包级调用，`logger.C(ctx).Warn(...)` 这种链式调用不会被误判
// （它的 Fun.X 是 CallExpr 而不是 Ident）。
func packageLevelCall(call *ast.CallExpr) (pkg, fn string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	ident, isIdent := sel.X.(*ast.Ident)
	if !isIdent {
		return "", "", false
	}
	return ident.Name, sel.Sel.Name, true
}

// enclosingFuncName 返回包含 pos 的最内层函数声明名，用于匹配豁免表。
func enclosingFuncName(file *ast.File, pos token.Pos) string {
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

// TestCtxLoggingMigratedFilesListIsSane 防止列表退化成空或出现重复项，
// 那样上面两个基线测试会变成静默通过。
func TestCtxLoggingMigratedFilesListIsSane(t *testing.T) {
	if len(ctxLoggingMigratedFiles) < 10 {
		t.Fatalf("已迁移文件列表只剩 %d 项，#103 首阶段有 10 个文件；只增不减",
			len(ctxLoggingMigratedFiles))
	}
	seen := map[string]bool{}
	var dups []string
	for _, name := range ctxLoggingMigratedFiles {
		if seen[name] {
			dups = append(dups, name)
		}
		seen[name] = true
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		t.Errorf("ctxLoggingMigratedFiles 有重复项: %v", dups)
	}
}
