package handler

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 模型维度 RPM 限流从 request context 里的 ctxkey.Model 取公开模型名（issue #206 方案 A）。
// 这条链路有两个隐性前提，靠 grep「有没有调过 setOpsRequestContext」都发现不了：
//
//  1. 资格检查之前必须调过 setOpsRequestContext(c, <模型名>, ...)；
//  2. 传给 CheckBillingEligibility 的 ctx 必须是在那之后取的快照——
//     setOpsRequestContext 替换的是 c.Request，早于它取的 ctx 变量拿不到 ctxkey.Model。
//
// ResponsesWebSocket 曾经正是踩了第 2 条（ctx 在 setOpsRequestContext 之前快照），
// 于是模型限流静默失效。本测试把两条前提都钉住。

var modelContextCheckBillingArgRe = regexp.MustCompile(`CheckBillingEligibility\(\s*([A-Za-z_][\w.()]*)`)

type modelRPMContextCase struct {
	file     string
	function string
}

func TestModelRPMContextIsPopulatedBeforeBillingCheck(t *testing.T) {
	cases := []modelRPMContextCase{
		{file: "gateway_handler.go", function: "Messages"},
		{file: "gateway_handler.go", function: "CountTokens"},
		{file: "gateway_handler_chat_completions.go", function: "ChatCompletions"},
		{file: "gateway_handler_responses.go", function: "Responses"},
		{file: "gemini_v1beta_handler.go", function: "GeminiV1BetaModels"},
		{file: "grok_media.go", function: "handleGrokMedia"},
		{file: "grok_audio.go", function: "GrokRealtime"},
		{file: "openai_chat_completions.go", function: "ChatCompletions"},
		{file: "openai_alpha_search.go", function: "AlphaSearch"},
		{file: "openai_embeddings.go", function: "Embeddings"},
		{file: "openai_gateway_count_tokens.go", function: "ResponsesInputTokens"},
		{file: "openai_gateway_count_tokens.go", function: "CountTokens"},
		{file: "openai_gateway_handler.go", function: "Responses"},
		{file: "openai_gateway_handler.go", function: "Messages"},
		{file: "openai_gateway_handler.go", function: "ResponsesWebSocket"},
		{file: "openai_live.go", function: "Live"},
		{file: "openai_images.go", function: "Images"},
	}

	for _, tc := range cases {
		t.Run(tc.file+"/"+tc.function, func(t *testing.T) {
			source := stripGoComments(goFunctionSource(t, tc.file, tc.function))

			// 只看第一处资格检查（主闸门）。gateway_handler.Messages 里 prompt-too-long
			// 兜底分支的第二处是刻意用 service.WithoutModelRPMLimit 跳过模型限流的。
			checkIndex := strings.Index(source, "CheckBillingEligibility(")
			require.NotEqual(t, -1, checkIndex, "coverage case must contain a billing eligibility check")

			modelIndex := indexOfModelBearingOpsContext(source)
			require.NotEqual(t, -1, modelIndex,
				"资格检查前必须 setOpsRequestContext(c, <模型名>, ...)，否则模型维度 RPM 静默失效")
			require.Less(t, modelIndex, checkIndex,
				"setOpsRequestContext 必须排在 CheckBillingEligibility 之前")

			match := modelContextCheckBillingArgRe.FindStringSubmatch(source[checkIndex:])
			require.Len(t, match, 2, "failed to parse the ctx argument of CheckBillingEligibility")
			ctxArg := match[1]
			if ctxArg == "c.Request.Context()" {
				return
			}

			// 传的是 ctx 变量：它的最后一次快照必须晚于 setOpsRequestContext。
			snapshotRe := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(ctxArg) + `\s*:?=\s*[\w.]*\(?c\.Request\.Context\(\)`)
			var snapshotIndex = -1
			for _, loc := range snapshotRe.FindAllStringIndex(source[:checkIndex], -1) {
				snapshotIndex = loc[0]
			}
			require.NotEqual(t, -1, snapshotIndex,
				"%s 必须来自 c.Request.Context() 快照，否则拿不到 ctxkey.Model", ctxArg)
			require.Greater(t, snapshotIndex, modelIndex,
				"%s 的快照取在 setOpsRequestContext 之前，模型维度 RPM 会静默失效", ctxArg)
		})
	}
}

// TestModelRPMNotApplicableEndpointsStayRegistered 锁住「本就无模型概念」的名单。
//
// 这两个端点没有客户端公开模型名：web search 的模型由服务端 resolveGrokStandaloneSearchModel()
// 决定，tts/stt 压根没有模型概念。它们会计入 service.ModelRPMSkippedNoModelCount()，
// 属于预期跳过而非漏网。若哪天它们真的开始接收客户端模型名，这个测试会提醒把它们移进上面的覆盖名单。
func TestModelRPMNotApplicableEndpointsStayRegistered(t *testing.T) {
	notApplicable := []modelRPMContextCase{
		{file: "gateway_web_search.go", function: "WebSearch"},
		{file: "grok_audio.go", function: "GrokVoice"},
	}

	for _, tc := range notApplicable {
		t.Run(tc.file+"/"+tc.function, func(t *testing.T) {
			raw := goFunctionSource(t, tc.file, tc.function)
			require.Contains(t, raw, "模型维度 RPM 限流不适用",
				"不适用的端点必须在代码注释中显式登记原因")
			require.Equal(t, -1, indexOfModelBearingOpsContext(stripGoComments(raw)),
				"一旦这里开始写入模型名，就应移进 TestModelRPMContextIsPopulatedBeforeBillingCheck 的覆盖名单")
		})
	}
}

// indexOfModelBearingOpsContext 返回第一处「写入了非空模型名」的 setOpsRequestContext 位置。
// setOpsRequestContext(c, "", ...) 只清 ops 字段、不写 ctxkey.Model，不算数。
func indexOfModelBearingOpsContext(source string) int {
	re := regexp.MustCompile(`setOpsRequestContext\(\s*c\s*,\s*([^,]+),`)
	for _, match := range re.FindAllStringSubmatchIndex(source, -1) {
		arg := strings.TrimSpace(source[match[2]:match[3]])
		if arg == `""` {
			continue
		}
		return match[0]
	}
	return -1
}
