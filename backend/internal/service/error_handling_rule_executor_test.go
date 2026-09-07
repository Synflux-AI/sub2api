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
	// RuleRetryLimit 必须显式带出：effectiveSameAccountRetryLimit 的基数是
	// account.GetPoolModeRetryCount()，非 pool-mode 账号是 0，不带就静默退化成换号。
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
