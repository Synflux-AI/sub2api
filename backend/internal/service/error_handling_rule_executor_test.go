//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newExecTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

// execTestSettings 造一条只匹配 statusCode 的启用规则。
func execTestSettings(action string, retryCount int, exhausted string, platforms []string) ErrorHandlingRuleSettings {
	enabled := true
	rc := retryCount
	return ErrorHandlingRuleSettings{
		Enabled:           true,
		DefaultRetryCount: 1,
		Rules: []ErrorHandlingRule{{
			ID:              "rule-exec-1",
			Name:            "exec test",
			Enabled:         &enabled,
			Priority:        1,
			StatusCodes:     []int{500},
			Action:          action,
			RetryCount:      &rc,
			ExhaustedAction: exhausted,
			Platforms:       platforms,
		}},
	}
}

func TestExecuteErrorHandlingRuleFailoverAction(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Name: "acct", Platform: PlatformGemini}
	var loggedAction string
	accountingCalls := 0

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            execTestSettings(ErrorHandlingActionFailover, 0, "", []string{PlatformGemini}),
		Account:             account,
		StatusCode:          http.StatusInternalServerError,
		Header:              http.Header{},
		Body:                []byte(`{"error":{"type":"server_error","message":"boom"}}`),
		BuiltinWillFailover: true,
		SafeError:           func([]byte) (string, string) { return "server_error", "boom" },
		AccountAccounting:   func() { accountingCalls++ },
		LogDecision: func(_ errorHandlingRuleDecision, effectiveAction string) {
			loggedAction = effectiveAction
		},
	})

	if !handled || failoverErr == nil {
		t.Fatalf("expected rule to handle the error, got handled=%v err=%v", handled, failoverErr)
	}
	if failoverErr.NextAccountAction != NextAccountRetry {
		t.Fatalf("failover action must switch account, got %v", failoverErr.NextAccountAction)
	}
	if failoverErr.RetryableOnSameAccount {
		t.Fatal("failover action must not retry on the same account")
	}
	// SafeError 三个动作都要无条件填：exhausted_action=passthrough 的消费点要求非空。
	if failoverErr.SafeErrorType != "server_error" || failoverErr.SafeErrorMessage != "boom" {
		t.Fatalf("SafeError* must be filled unconditionally, got %q/%q", failoverErr.SafeErrorType, failoverErr.SafeErrorMessage)
	}
	if loggedAction != ErrorHandlingActionFailover {
		t.Fatalf("expected logged effective action %q, got %q", ErrorHandlingActionFailover, loggedAction)
	}
	// 内置已决定换号 → 执行层不得替它补记账（会重复扣账号健康分）。
	if accountingCalls != 0 {
		t.Fatalf("AccountAccounting must not run when builtin already failed over, got %d calls", accountingCalls)
	}
}

func TestExecuteErrorHandlingRuleRetryCarriesRuleRetryLimit(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            execTestSettings(ErrorHandlingActionRetry, 3, "", []string{PlatformGemini}),
		Account:             account,
		StatusCode:          http.StatusInternalServerError,
		Header:              http.Header{},
		Body:                []byte(`{"error":{"message":"boom"}}`),
		BuiltinWillFailover: true,
		SafeError:           func([]byte) (string, string) { return "upstream_error", "boom" },
		LogDecision:         func(errorHandlingRuleDecision, string) {},
	})

	if !handled || failoverErr == nil {
		t.Fatal("expected rule to handle the error")
	}
	if !failoverErr.RetryableOnSameAccount {
		t.Fatal("retry action must set RetryableOnSameAccount")
	}
	// RuleRetryLimit 必须显式带出：effectiveSameAccountRetryLimit 不带它时会
	// 裸用 account.GetPoolModeRetryCount() 兜底，非 pool-mode 或未显式配置时
	// 这个基数是默认值 3，跟这里规则配的重试次数未必一致，不显式带出就会被
	// 账号基数顶掉而错。
	if failoverErr.RuleRetryLimit == nil || *failoverErr.RuleRetryLimit != 3 {
		t.Fatalf("expected RuleRetryLimit=3, got %v", failoverErr.RuleRetryLimit)
	}
}

func TestExecuteErrorHandlingRulePassthroughStopsAndSetsExhaustedAction(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            execTestSettings(ErrorHandlingActionPassthrough, 0, "", []string{PlatformGemini}),
		Account:             account,
		StatusCode:          http.StatusInternalServerError,
		Header:              http.Header{},
		Body:                []byte(`{"error":{"type":"server_error","message":"boom"}}`),
		BuiltinWillFailover: true,
		SafeError:           func([]byte) (string, string) { return "server_error", "boom" },
		LogDecision:         func(errorHandlingRuleDecision, string) {},
	})

	if !handled || failoverErr == nil {
		t.Fatal("expected rule to handle the error")
	}
	if failoverErr.NextAccountAction != NextAccountStop {
		t.Fatalf("passthrough must stop failover, got %v", failoverErr.NextAccountAction)
	}
	if failoverErr.ExhaustedAction != ErrorHandlingExhaustedActionPassthrough {
		t.Fatalf("passthrough is landed via ExhaustedAction, got %q", failoverErr.ExhaustedAction)
	}
}

func TestExecuteErrorHandlingRuleBuiltinOwnsWins(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}
	logged := false

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            execTestSettings(ErrorHandlingActionPassthrough, 0, "", []string{PlatformGemini}),
		Account:             account,
		StatusCode:          http.StatusInternalServerError,
		Header:              http.Header{},
		Body:                []byte(`{"error":{"message":"boom"}}`),
		BuiltinOwns:         true,
		BuiltinWillFailover: true,
		SafeError:           func([]byte) (string, string) { return "upstream_error", "boom" },
		LogDecision:         func(errorHandlingRuleDecision, string) { logged = true },
	})

	if handled || failoverErr != nil {
		t.Fatal("BuiltinOwns must keep the builtin path; the rule engine must not take over")
	}
	if logged {
		t.Fatal("no decision log when the builtin owns the error")
	}
}

func TestExecuteErrorHandlingRuleAccountingRunsWhenBuiltinDoesNotFailover(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}
	accountingCalls := 0

	_, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            execTestSettings(ErrorHandlingActionFailover, 0, "", []string{PlatformGemini}),
		Account:             account,
		StatusCode:          http.StatusInternalServerError,
		Header:              http.Header{},
		Body:                []byte(`{"error":{"message":"boom"}}`),
		BuiltinWillFailover: false,
		SafeError:           func([]byte) (string, string) { return "upstream_error", "boom" },
		AccountAccounting:   func() { accountingCalls++ },
		LogDecision:         func(errorHandlingRuleDecision, string) {},
	})

	if !handled {
		t.Fatal("expected rule to handle the error")
	}
	// 内置不换号时那条记账在被跳过的内置错误处理链里，执行层必须补跑，
	// 否则稳定报错的账号永远不进冷却、会被一直调度。
	if accountingCalls != 1 {
		t.Fatalf("expected AccountAccounting to run exactly once, got %d", accountingCalls)
	}
}

func TestExecuteErrorHandlingRulePlatformFilterExcludesOtherPlatforms(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}

	_, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:            execTestSettings(ErrorHandlingActionFailover, 0, "", []string{PlatformAnthropic}),
		Account:             account,
		StatusCode:          http.StatusInternalServerError,
		Header:              http.Header{},
		Body:                []byte(`{"error":{"message":"boom"}}`),
		BuiltinWillFailover: true,
		SafeError:           func([]byte) (string, string) { return "upstream_error", "boom" },
		LogDecision:         func(errorHandlingRuleDecision, string) {},
	})

	if handled {
		t.Fatal("a rule scoped to anthropic must not match a gemini account")
	}
}

// 下面三个测试驱动 SemanticEventForwarded 在执行层的行为（#228 task-11）。
//
// 这是对 exec 层的**隔离**测试：Task 11 的调研没有在 Gemini/Antigravity 的任何
// 流式转发路径上找到会调用 executeErrorHandlingRule（或它的两个平台包装）的
// 既有接线点——两个平台的 SSE 循环里，读错误/行超长/数据间隔超时全部直接
// `return nil, err`，从未构造过 UpstreamFailoverError，也没有 Anthropic 侧
// anthropicPassthroughSSEError 那种「流内内嵌错误事件」识别。新增这样一个接线点
// 属于新架构（要跟 SafeToFailoverAfterWrite、各流式函数自己的 sendErrorEvent /
// errorEventSent 去重机制对齐），超出本任务「加一个字段」的范围，所以这里只覆盖
// 执行层本身：确认 SemanticEventForwarded=true 时 retry/failover 被决策层降级后，
// 执行层不会像 Tracker=nil 触发的 retry_tracker_missing 降级一样被忽略——那种
// 降级刻意只影响 errorHandlingRuleExecEffectiveAction 的**日志**语义，
// semantic_output_started 这种降级必须连 UpstreamFailoverError 的字段构造都改变。

func TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesRetryToPassthrough(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}
	var loggedAction string

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:               execTestSettings(ErrorHandlingActionRetry, 3, "", []string{PlatformGemini}),
		Account:                account,
		StatusCode:             http.StatusInternalServerError,
		Header:                 http.Header{},
		Body:                   []byte(`{"error":{"type":"server_error","message":"boom"}}`),
		BuiltinWillFailover:    true,
		SemanticEventForwarded: true,
		SafeError:              func([]byte) (string, string) { return "server_error", "boom" },
		LogDecision: func(_ errorHandlingRuleDecision, effectiveAction string) {
			loggedAction = effectiveAction
		},
	})

	if !handled || failoverErr == nil {
		t.Fatal("expected rule to still match and hand back a decision")
	}
	// 已提交的流上不能再拼第二条流：配置的 retry 必须落地成 passthrough——
	// 不重试、不换号，把已经进行到一半的流安全终止。
	if failoverErr.RetryableOnSameAccount {
		t.Fatal("semantic output already forwarded: must not retry on the same account")
	}
	if failoverErr.NextAccountAction != NextAccountStop {
		t.Fatalf("semantic output already forwarded: must stop, got %v", failoverErr.NextAccountAction)
	}
	if failoverErr.ExhaustedAction != ErrorHandlingExhaustedActionPassthrough {
		t.Fatalf("expected downgraded passthrough exhausted action, got %q", failoverErr.ExhaustedAction)
	}
	// Kind 落地用的是**生效**动作：日志必须如实写 passthrough，不能还是配置的 retry，
	// 否则 upstream_errors 里的 outcome 和这里实际执行的动作自相矛盾。
	if loggedAction != ErrorHandlingActionPassthrough {
		t.Fatalf("expected logged effective action %q, got %q", ErrorHandlingActionPassthrough, loggedAction)
	}
}

func TestExecuteErrorHandlingRuleSemanticEventForwardedDowngradesFailoverToPassthrough(t *testing.T) {
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}
	var loggedAction string

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:               execTestSettings(ErrorHandlingActionFailover, 0, "", []string{PlatformGemini}),
		Account:                account,
		StatusCode:             http.StatusInternalServerError,
		Header:                 http.Header{},
		Body:                   []byte(`{"error":{"type":"server_error","message":"boom"}}`),
		BuiltinWillFailover:    true,
		SemanticEventForwarded: true,
		SafeError:              func([]byte) (string, string) { return "server_error", "boom" },
		LogDecision: func(_ errorHandlingRuleDecision, effectiveAction string) {
			loggedAction = effectiveAction
		},
	})

	if !handled || failoverErr == nil {
		t.Fatal("expected rule to still match and hand back a decision")
	}
	if failoverErr.NextAccountAction != NextAccountStop {
		t.Fatalf("semantic output already forwarded: configured failover must stop, not switch account, got %v", failoverErr.NextAccountAction)
	}
	if failoverErr.ExhaustedAction != ErrorHandlingExhaustedActionPassthrough {
		t.Fatalf("expected downgraded passthrough exhausted action, got %q", failoverErr.ExhaustedAction)
	}
	if loggedAction != ErrorHandlingActionPassthrough {
		t.Fatalf("expected logged effective action %q, got %q", ErrorHandlingActionPassthrough, loggedAction)
	}
}

func TestExecuteErrorHandlingRuleWithoutSemanticEventForwardedStillFailoversCleanly(t *testing.T) {
	// 对照组：只写了 keepalive（这里用 SemanticEventForwarded 的零值 false 代表
	// 「还没写语义内容」），配置的 failover 必须原样生效、不能被误判降级——
	// 这是 #204 要求覆盖的另一半：裸比较 Writer.Size() 会把这种情况错判成
	// 「已交付内容」，而显式标志在零值上天然不会。
	c := newExecTestContext()
	account := &Account{ID: 7, Platform: PlatformGemini}
	var loggedAction string

	failoverErr, handled := executeErrorHandlingRule(c, errorHandlingRuleExecInput{
		Settings:               execTestSettings(ErrorHandlingActionFailover, 0, "", []string{PlatformGemini}),
		Account:                account,
		StatusCode:             http.StatusInternalServerError,
		Header:                 http.Header{},
		Body:                   []byte(`{"error":{"type":"server_error","message":"boom"}}`),
		BuiltinWillFailover:    true,
		SemanticEventForwarded: false,
		SafeError:              func([]byte) (string, string) { return "server_error", "boom" },
		LogDecision: func(_ errorHandlingRuleDecision, effectiveAction string) {
			loggedAction = effectiveAction
		},
	})

	if !handled || failoverErr == nil {
		t.Fatal("expected rule to handle the error")
	}
	if failoverErr.NextAccountAction != NextAccountRetry {
		t.Fatalf("no semantic output yet: failover must still switch account, got %v", failoverErr.NextAccountAction)
	}
	if failoverErr.RetryableOnSameAccount {
		t.Fatal("failover action must not retry on the same account")
	}
	if loggedAction != ErrorHandlingActionFailover {
		t.Fatalf("expected logged effective action %q, got %q", ErrorHandlingActionFailover, loggedAction)
	}
}
