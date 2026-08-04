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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type sequencedUpstreamResponse struct {
	status int
	body   string
}

type sequencedHTTPUpstream struct {
	responses []sequencedUpstreamResponse
	calls     int
}

func (u *sequencedHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	idx := u.calls
	if idx >= len(u.responses) {
		idx = len(u.responses) - 1
	}
	u.calls++
	r := u.responses[idx]
	return &http.Response{
		StatusCode: r.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Upstream-Test": []string{"kept-if-allowed"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
	}, nil
}

func (u *sequencedHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

const errorHandlingRuleSuccessBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func newErrorHandlingRuleForwardAccount() *Account {
	return &Account{
		ID: 701, Name: "apikey-error-rule-test", Platform: PlatformAnthropic,
		Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "upstream-anthropic-key", "base_url": "https://api.anthropic.com"},
		Status:      StatusActive, Schedulable: true,
	}
}

func newErrorHandlingRuleTestParsed(t *testing.T) *ParsedRequest {
	t.Helper()
	body := []byte(`{"model":"claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	return parsed
}

func newErrorHandlingRuleForwardService(t *testing.T, upstream *sequencedHTTPUpstream, ruleSettings *ErrorHandlingRuleSettings) *GatewayService {
	t.Helper()
	repo := &gatewayTTLSettingRepo{data: map[string]string{}}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream,
		settingService: NewSettingService(repo, cfg), rateLimitService: &RateLimitService{}, deferredService: &DeferredService{},
	}
	require.NoError(t, svc.settingService.SetErrorHandlingRuleSettings(context.Background(), ruleSettings))
	return svc
}

func newErrorHandlingRuleTestContext() *gin.Context {
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	return c
}

func newErrorHandlingRuleTestContextWithRecorder() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c, rec
}

func TestForwardErrorHandlingRuleRetriesInPlaceThenSucceeds(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 500, body: `{"error":{"message":"Proxy Error"}}`},
		{status: 200, body: errorHandlingRuleSuccessBody},
	}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Keywords: []string{"Proxy Error"}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(2)}},
	})
	result, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 2, upstream.calls)
}

func TestForwardErrorHandlingRuleFailsOverAfterRetryBudgetExhausted(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 500, body: `{"error":{"message":"Proxy Error"}}`}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry, RetryCount: errorHandlingIntPtr(1)}},
	})
	_, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 500, failoverErr.StatusCode)
	require.Equal(t, 2, upstream.calls)
}

func TestForwardErrorHandlingRuleFailoverActionSwitchesImmediately(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 500, body: `{"error":{"message":"proxy failure"}}`}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover}},
	})
	_, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 1, upstream.calls)
}

func TestForwardErrorHandlingRulePassthroughReturnsErrorToClient(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 400, body: `{"error":{"message":"prompt is too long"}}`}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", Keywords: []string{"prompt is too long"}, Action: ErrorHandlingActionPassthrough}},
	})
	_, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, 1, upstream.calls)
}

func TestForwardErrorHandlingRulePassthroughPreservesNon400RawResponse(t *testing.T) {
	rawBody := `{"error":{"type":"upstream_custom","message":"raw proxy failure","details":{"code":17}}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 500, body: rawBody}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough}},
	})
	c, rec := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, rawBody, rec.Body.String())
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, 1, upstream.calls)
}

func TestForwardErrorHandlingRuleDisabledFallsBackToExistingBehavior(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 500, body: `{"error":{"message":"proxy failure"}}`}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{Enabled: false})
	_, err := svc.Forward(context.Background(), newErrorHandlingRuleTestContext(), newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, 1, upstream.calls)
}

// ops_error_logs 的顶层 upstream 字段由 setOpsUpstreamError 写入，所有「已提交响应」
// 的终态出口都要填（handleErrorResponse / handleRetryExhaustedError 都填了）。
// passthrough 会成为这批确定性错误最常见的终态，漏填等于这些请求在 Ops 后台
// 查不到上游状态码和错误消息。
func TestForwardErrorHandlingRulePassthroughRecordsOpsUpstreamError(t *testing.T) {
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 500, body: `{"error":{"message":"raw proxy failure"}}`},
	}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough}},
	})
	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	require.Error(t, err)

	status, ok := c.Get(OpsUpstreamStatusCodeKey)
	require.True(t, ok, "passthrough 终态必须写入 ops 上游状态码")
	require.Equal(t, 500, status)
	message, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok, "passthrough 终态必须写入 ops 上游错误消息")
	require.Contains(t, message, "raw proxy failure")
}

// issue #122 决策 5：内置的 Thinking 签名处理优先于新引擎。规则宽泛匹配 400/422 时
// 不能把签名错误从内置换号逻辑手里抢走——那会让签名错误在同一个账号上原样重发，
// 或者被 passthrough 直接吐回客户端。
func TestForwardBuiltinSignatureFailoverPreemptsErrorHandlingRule(t *testing.T) {
	signatureBody := `{"error":{"message":"Invalid signature in thinking block"}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{{status: 400, body: signatureBody}}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{400}, Action: ErrorHandlingActionPassthrough}},
	})
	require.NoError(t, svc.settingService.SetRectifierSettings(context.Background(), &RectifierSettings{
		Enabled: true, APIKeySignatureFailoverEnabled: true,
	}))

	c, _ := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "签名错误应由内置逻辑换号，而不是被规则 passthrough 掉")
	require.False(t, IsResponseCommitted(c))
}

// budget 整流重试的应答（gateway_forward.go 的 400 分支内）直接 break 出重试循环，
// 绕过了循环内所有规则接入点，也绕过了循环末尾覆盖非 400 的那个分支。
// 结果：整流重试带回来的 5xx 仍走老 failover，正是 issue #122 要根除的空耗账号池。
func TestForwardErrorHandlingRuleCatchesResponseFromBudgetRectifierRetry(t *testing.T) {
	rawBody := `{"error":{"message":"deterministic upstream failure"}}`
	upstream := &sequencedHTTPUpstream{responses: []sequencedUpstreamResponse{
		{status: 400, body: `{"error":{"message":"thinking.budget_tokens: Input should be greater than or equal to 1024"}}`},
		{status: 500, body: rawBody},
	}}
	svc := newErrorHandlingRuleForwardService(t, upstream, &ErrorHandlingRuleSettings{
		Enabled: true,
		Rules:   []ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough}},
	})
	require.NoError(t, svc.settingService.SetRectifierSettings(context.Background(), &RectifierSettings{
		Enabled: true, ThinkingBudgetEnabled: true,
	}))

	c, rec := newErrorHandlingRuleTestContextWithRecorder()
	_, err := svc.Forward(context.Background(), c, newErrorHandlingRuleForwardAccount(), newErrorHandlingRuleTestParsed(t))
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "命中 passthrough 的规则不应换号")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, rawBody, rec.Body.String())
	require.Equal(t, 2, upstream.calls)
}
