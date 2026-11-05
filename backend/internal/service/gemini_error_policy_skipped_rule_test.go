//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// #325：ErrorPolicySkipped（池模式 / 自定义错误码未命中）在透传或内置换号之前必须先问
// 错误处理规则。此前 Forward / ForwardNative 在 Skipped 分支直接 return，规则引擎压根
// 没被调用，ops upstream_errors 里只有 http_error、没有 error_handling_rule_*。
// ---------------------------------------------------------------------------

const geminiSkippedRuleKeyword = "invalid gemini function call history"

func geminiSkippedKeywordFailoverRule() ErrorHandlingRule {
	return ErrorHandlingRule{
		ID: "gemini-skipped-400", StatusCodes: []int{http.StatusBadRequest},
		Keywords: []string{geminiSkippedRuleKeyword}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformGemini},
	}
}

func newGeminiSkippedRuleService(t *testing.T, status int, body string, rules ...ErrorHandlingRule) (*GeminiMessagesCompatService, *geminiCompatHTTPUpstreamStub, *errorPolicyRepoStub) {
	t.Helper()
	httpStub := &geminiCompatHTTPUpstreamStub{
		response: &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		},
	}
	repo := &errorPolicyRepoStub{}
	svc := &GeminiMessagesCompatService{
		httpUpstream:     httpStub,
		cfg:              &config.Config{},
		rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
		settingService:   newGeminiRuleSettingService(t, rules...),
	}
	return svc, httpStub, repo
}

func newGeminiClaudeCompatTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	return c, rec
}

var (
	geminiSkippedNativeBody = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	geminiSkippedClaudeBody = []byte(`{"model":"gemini-2.5-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
)

func requireGeminiSkippedRuleFailover(t *testing.T, err error, c *gin.Context, rec *httptest.ResponseRecorder, repo *errorPolicyRepoStub) {
	t.Helper()
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "规则命中后应返回换号错误，实际: %v", err)
	require.Equal(t, "gemini-skipped-400", failoverErr.ErrorRuleID)
	require.Equal(t, http.StatusBadRequest, failoverErr.StatusCode)
	require.True(t, failoverErr.ShouldRetryNextAccount())
	require.Zero(t, rec.Body.Len(), "换号场景不应写客户端响应")

	kinds := make([]string, 0)
	for _, ev := range opsUpstreamErrorEvents(t, c) {
		kinds = append(kinds, ev.Kind)
	}
	require.Contains(t, kinds, "error_handling_rule_failover")

	// Skipped 只豁免账号状态标记：规则接管也不得补记账。
	require.Zero(t, repo.setErrCalls)
	require.Zero(t, repo.tempCalls)
}

func requireNoGeminiRuleEvents(t *testing.T, c *gin.Context) {
	t.Helper()
	for _, ev := range opsUpstreamErrorEvents(t, c) {
		require.False(t, strings.HasPrefix(ev.Kind, "error_handling_rule_"), "不应出现规则事件: %s", ev.Kind)
	}
}

func TestGeminiForwardNative_PoolModeSkipped400RuleFailover(t *testing.T) {
	svc, httpStub, repo := newGeminiSkippedRuleService(t, http.StatusBadRequest, geminiSkippedTestUpstreamBody(), geminiSkippedKeywordFailoverRule())
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(),
		"gemini-2.5-flash", "generateContent", false, geminiSkippedNativeBody)

	require.Nil(t, result)
	require.Equal(t, 1, httpStub.calls)
	requireGeminiSkippedRuleFailover(t, err, c, rec, repo)
}

func TestGeminiForward_PoolModeSkipped400RuleFailover(t *testing.T) {
	svc, httpStub, repo := newGeminiSkippedRuleService(t, http.StatusBadRequest, geminiSkippedTestUpstreamBody(), geminiSkippedKeywordFailoverRule())
	c, rec := newGeminiClaudeCompatTestContext()

	result, err := svc.Forward(context.Background(), c, geminiPoolModeAPIKeyAccount(), geminiSkippedClaudeBody)

	require.Nil(t, result)
	require.Equal(t, 1, httpStub.calls)
	requireGeminiSkippedRuleFailover(t, err, c, rec, repo)
}

// 自定义错误码未命中同样是 Skipped：规则优先，与 Anthropic / OpenAI 以及本服务的
// ForwardAsChatCompletions 口径一致。
func TestGeminiForwardNative_CustomCodesMiss400RuleFailover(t *testing.T) {
	svc, _, repo := newGeminiSkippedRuleService(t, http.StatusBadRequest, geminiSkippedTestUpstreamBody(), geminiSkippedKeywordFailoverRule())
	c, rec := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, geminiCustomCodesAPIKeyAccount(),
		"gemini-2.5-flash", "generateContent", false, geminiSkippedNativeBody)

	requireGeminiSkippedRuleFailover(t, err, c, rec, repo)
}

func TestGeminiForward_CustomCodesMiss400RuleFailover(t *testing.T) {
	svc, _, repo := newGeminiSkippedRuleService(t, http.StatusBadRequest, geminiSkippedTestUpstreamBody(), geminiSkippedKeywordFailoverRule())
	c, rec := newGeminiClaudeCompatTestContext()

	_, err := svc.Forward(context.Background(), c, geminiCustomCodesAPIKeyAccount(), geminiSkippedClaudeBody)

	requireGeminiSkippedRuleFailover(t, err, c, rec, repo)
}

// 规则未命中（关键字对不上）：与没有规则时逐字一致——400 原样透传、不换号、无规则事件。
func TestGeminiForwardNative_PoolModeSkipped400RuleMissUnchanged(t *testing.T) {
	upstreamBody := geminiSkippedTestUpstreamBody()
	rule := geminiSkippedKeywordFailoverRule()
	rule.Keywords = []string{"some other message"}
	svc, _, _ := newGeminiSkippedRuleService(t, http.StatusBadRequest, upstreamBody, rule)
	c, rec := newGeminiNativeTestContext(t)

	result, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(),
		"gemini-2.5-flash", "generateContent", false, geminiSkippedNativeBody)

	require.Nil(t, result)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "规则未命中时池模式 400 不应换号")
	require.Contains(t, err.Error(), "gemini upstream error: 400")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, upstreamBody, rec.Body.String())
	requireNoGeminiRuleEvents(t, c)
}

func TestGeminiForward_PoolModeSkipped400RuleMissUnchanged(t *testing.T) {
	rule := geminiSkippedKeywordFailoverRule()
	rule.Keywords = []string{"some other message"}
	svcWithRule, _, _ := newGeminiSkippedRuleService(t, http.StatusBadRequest, geminiSkippedTestUpstreamBody(), rule)
	cWith, recWith := newGeminiClaudeCompatTestContext()
	_, errWith := svcWithRule.Forward(context.Background(), cWith, geminiPoolModeAPIKeyAccount(), geminiSkippedClaudeBody)

	svcNoRule, _ := newGeminiSkippedWriteService(http.StatusBadRequest, geminiSkippedTestUpstreamBody())
	cWithout, recWithout := newGeminiClaudeCompatTestContext()
	_, errWithout := svcNoRule.Forward(context.Background(), cWithout, geminiPoolModeAPIKeyAccount(), geminiSkippedClaudeBody)

	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(errWith, &failoverErr))
	require.Equal(t, errWithout.Error(), errWith.Error())
	require.Equal(t, recWithout.Code, recWith.Code)
	require.Equal(t, recWithout.Body.String(), recWith.Body.String())
	requireNoGeminiRuleEvents(t, cWith)
}

// Google 项目配置类 400 是确定性错误，换号必复现：Skipped 接线点上也必须由内置独占，
// 一条宽泛的「400 → failover」规则不得抢走。
func TestGeminiForwardNative_PoolModeSkippedGoogleProjectConfigNotTakenByRule(t *testing.T) {
	svc, _, _ := newGeminiSkippedRuleService(t, http.StatusBadRequest,
		`{"error":{"status":"INVALID_ARGUMENT","message":"Invalid project resource name (project 123)"}}`,
		ErrorHandlingRule{
			ID: "broad-400", StatusCodes: []int{http.StatusBadRequest}, Action: ErrorHandlingActionFailover,
			Platforms: []string{PlatformGemini},
		})
	c, rec := newGeminiNativeTestContext(t)

	_, err := svc.ForwardNative(context.Background(), c, geminiPoolModeAPIKeyAccount(),
		"gemini-2.5-flash", "generateContent", false, geminiSkippedNativeBody)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "确定性的 Google 项目配置 400 不得被规则接管换号")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	requireNoGeminiRuleEvents(t, c)
}
