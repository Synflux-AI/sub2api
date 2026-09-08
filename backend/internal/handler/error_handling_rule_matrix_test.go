//go:build unit

package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #228 task-12：错误处理规则跨平台验收矩阵（handler 层）。
//
// 覆盖 issue #228 §七 6×7 矩阵里落在 internal/handler 包的 2 个维度：retry budget、
// exhausted passthrough。另外 5 个维度（HTTP 非failover/failover 错误、transport
// 错误、流内错误、错误透传让路）落在 internal/service 包，见同名文件
// error_handling_rule_matrix_test.go（位于 internal/service）。
//
// retry budget 与 exhausted passthrough 的底层机制（effectiveSameAccountRetryLimit /
// sameAccountRetryAllowed / FailoverState.HandleFailoverError / handleFailoverExhausted）
// 是所有平台共用的同一套 handler 层函数，只是各平台的 exhausted 兜底文案/事件构造
// 不同（handleCCFailoverExhausted / handleGeminiFailoverExhausted /
// handleAnthropicFailoverExhausted / handleResponsesFailoverExhausted /
// h.handleFailoverExhausted 供 Grok media 调用）。因此 retry budget 维度 6 行引用的
// 是同一批共享机制测试；exhausted passthrough 维度按平台各自的兜底函数分别引用。
//
// retry budget 维度里"非 pool-mode 账号，规则预算覆盖账号默认值"这条要求单独钉住：
// issue 原文说非 pool-mode 账号的默认重试预算是 0，这是不准确的——
// Account.GetPoolModeRetryCount() 对非 pool-mode 账号（IsPoolMode() 恒为 false）
// 兜底走 defaultPoolModeRetryCount，实测值是 3，不是 0；0 只在管理员显式把某个
// pool-mode 账号的 pool_mode_retry_count 配置成 ≤0 时才会出现。这里用真实值 3
// （TestEffectiveSameAccountRetryLimitRuleOverridesNonPoolAccount 里对此有一条
// precondition 断言）而不是 issue 原文的 0。

func matrixAssertTestExists(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("matrix self-check: cannot read dir %q: %v", dir, err)
	}
	var source strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("matrix self-check: cannot read %q: %v", e.Name(), err)
		}
		source.Write(data)
		source.WriteByte('\n')
	}
	combined := source.String()
	for _, name := range names {
		if !strings.Contains(combined, "func "+name+"(") {
			t.Errorf("matrix gap: referenced test %q not found in %q — the coverage claim for this cell is stale; "+
				"the test was renamed or deleted and this cell must be re-verified (re-covered, referenced under its "+
				"new name, or reclassified as not-applicable with a documented reason)", name, dir)
		}
	}
}

// retryBudgetSharedRefs 是 retry budget 维度所有 6 行共用的引用集合：
// effectiveSameAccountRetryLimit / FailoverState.HandleFailoverError 是平台无关的
// 共享函数，不因入口不同而有不同实现。
func retryBudgetSharedRefs(t *testing.T) {
	t.Helper()
	matrixAssertTestExists(t, ".",
		"TestEffectiveSameAccountRetryLimitRuleOverridesNonPoolAccount", // 非 pool-mode 账号，规则预算覆盖默认值 3（不是 0）
		"TestHandleFailoverErrorUsesRuleRetryBudget",                    // 拿到规则预算后真的同账号重试
		"TestEffectiveSameAccountRetryLimit_RuleBudgetOverridesPoolMode",
		"TestSameAccountRetryAllowed_RuleBudget",
		"TestEffectiveSameAccountRetryLimitHonorsErrorCapAndDisabledAccount", // miss：无规则预算时退回既有行为
	)
}

// ==================== 行 1/6：Anthropic Messages ====================

func TestErrorHandlingRuleMatrix_AnthropicMessages(t *testing.T) {
	t.Run("retry_budget_shared_mechanism", func(t *testing.T) {
		retryBudgetSharedRefs(t)
	})
	t.Run("exhausted_passthrough", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestAnthropicFailoverExhausted_RulePassthroughReturnsUpstreamError",
			"TestAnthropicFailoverExhausted_RulePassthroughWithoutSafeErrorFallsBack",
			"TestHandleFailoverExhaustedUsesRuleSafePassthroughJSON",
			"TestHandleFailoverExhaustedUsesRuleSafePassthroughSSEAfterPing",
			"TestStreamRuleSingleAccountExhaustionUsesSafeError",
			"TestStreamRuleFailoverSwitchesAccountsWithoutPoolModeRetryAndUsesLastSafeError",
			"TestHandleAnthropicFailoverExhaustedWithoutRuleKeepsGenericError", // miss：#228 task-12 修复轮1 补
		)
	})
}

// ==================== 行 2/6：Gemini Messages ====================

func TestErrorHandlingRuleMatrix_GeminiMessages(t *testing.T) {
	t.Run("retry_budget_shared_mechanism", func(t *testing.T) {
		retryBudgetSharedRefs(t)
	})
	t.Run("exhausted_passthrough", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestHandleGeminiFailoverExhaustedHonorsRulePassthrough",
			"TestHandleGeminiFailoverExhaustedWithoutRuleKeepsGenericError", // miss：#228 task-12 修复轮1 补
		)
	})
}

// ==================== 行 3/6：Gemini Native ====================

func TestErrorHandlingRuleMatrix_GeminiNative(t *testing.T) {
	t.Run("retry_budget_shared_mechanism", func(t *testing.T) {
		retryBudgetSharedRefs(t)
	})
	t.Run("exhausted_passthrough_shared_with_gemini_messages", func(t *testing.T) {
		// handleGeminiFailoverExhausted 是 Gemini Messages / Gemini Native 共用的同一个
		// handler 层兜底函数（两个入口最终都调用它），没有找到 Native 专属的另一份
		// exhausted 兜底实现，这里引用同一个测试并明确记录这个共享假设。
		matrixAssertTestExists(t, ".",
			"TestHandleGeminiFailoverExhaustedHonorsRulePassthrough",
			"TestHandleGeminiFailoverExhaustedWithoutRuleKeepsGenericError", // miss：#228 task-12 修复轮1 补，与上面同一个共享假设
		)
	})
}

// ==================== 行 4/6：Chat Completions ====================

func TestErrorHandlingRuleMatrix_ChatCompletions(t *testing.T) {
	t.Run("retry_budget_shared_mechanism", func(t *testing.T) {
		retryBudgetSharedRefs(t)
	})
	t.Run("exhausted_passthrough", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestHandleCCFailoverExhaustedHonorsRulePassthrough",
			"TestHandleCCFailoverExhaustedWithoutRuleKeepsGenericError", // miss
			// 2026-09-08 项目所有者反转 #228 非目标：两个机制同时命中时错误处理
			// 规则引擎胜出（旧名 …ErrorPassthroughRuleWinsOverRuleEngine，旧语义
			// 正相反——那时是透传规则赢）。CC 这一行现在与其它平台的接线次序
			// 一致，不再是唯一的反向优先级实现。
			"TestHandleCCFailoverExhaustedRuleEngineWinsOverErrorPassthroughRule",
			// 同时命中之外，"只有透传规则命中"这条独立分支必须继续被覆盖，
			// 否则上面的反转会连带丢掉"透传规则单独生效"的回归保护。
			"TestHandleCCFailoverExhaustedErrorPassthroughRuleAloneStillApplies",
		)
	})
}

// ==================== 行 5/6：Responses ====================

func TestErrorHandlingRuleMatrix_Responses(t *testing.T) {
	t.Run("retry_budget_shared_mechanism", func(t *testing.T) {
		retryBudgetSharedRefs(t)
		matrixAssertTestExists(t, ".", "TestOpenAIWSSameAccountRetryAllowed_RuleBudget")
	})
	t.Run("exhausted_passthrough", func(t *testing.T) {
		matrixAssertTestExists(t, ".",
			"TestOpenAIFailoverExhausted_RulePassthroughReturnsUpstreamError",
			"TestOpenAIFailoverExhausted_RulePassthroughWithoutSafeErrorFallsBack",
			"TestHandleFailoverExhaustedUsesResponsesFailedEventForResponsesRequest",
			"TestHandleResponsesFailoverExhaustedUsesRuleSafeResponseFailedAfterStreamStart",
			"TestHandleResponsesFailoverExhaustedWithoutRuleKeepsGenericError", // miss：#228 task-12 修复轮1 补
			"TestOpenAIWSFailoverExhausted_RulePassthroughPreservesSyntheticStatus",
		)
	})
}

// ==================== 行 6/6：Grok Media generation ====================

func TestErrorHandlingRuleMatrix_GrokMediaGeneration(t *testing.T) {
	t.Run("retry_budget_shared_mechanism", func(t *testing.T) {
		retryBudgetSharedRefs(t)
		// Grok media 生成路径的 429 有界换号回归测试，证明共享的 FailoverState 循环
		// 在这个入口确实被真实驱动到（而不是只有理论上的共享）。
		matrixAssertTestExists(t, ".", "TestGrokMedia429FailoverIsBounded")
	})
	t.Run("exhausted_passthrough_shared_mechanism", func(t *testing.T) {
		// h.handleFailoverExhausted 在 grok_media.go 里的接收者是 *OpenAIGatewayHandler
		// （见该文件 GrokImages/GrokVideoGeneration 等方法的接收者类型），调用的是
		// OpenAIGatewayHandler.handleFailoverExhausted（openai_gateway_handler.go，
		// 3 个参数：c/failoverErr/streamStarted）。这与 GatewayHandler.handleFailoverExhausted
		// （gateway_handler.go，4 个参数：多一个 platform）是两个不同结构体上的同名
		// 不同方法——不能共用。此前这里误引用了 handleCCFailoverExhausted（属于
		// GatewayHandler 的另一个方法，同样对不上）。正确的共享机制证明是
		// TestOpenAIFailoverExhausted_RulePassthroughReturnsUpstreamError：它直接调用
		// (&OpenAIGatewayHandler{}).handleFailoverExhausted，与 Grok media 生成路径的
		// 调用点完全一致。
		matrixAssertTestExists(t, ".",
			"TestOpenAIFailoverExhausted_RulePassthroughReturnsUpstreamError",
			"TestGrokMedia429FailoverIsBounded",
		)
	})
}
