//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// #228 task-9：错误处理规则接入 Grok media（图片/视频生成）。ForwardGrokMedia（生成
// 路径）与 forwardGrokMediaVideoContent（video_content 查询，内部还会先查一次
// video_status）共用同一个 handleGrokMediaErrorResponse，但只有生成路径接线——
// video_status / video_content 查询按 #228 §六是明确例外（owner binding，不能换号）。

func newGrokMediaRuleSettingService(t *testing.T, rules ...ErrorHandlingRule) *SettingService {
	t.Helper()
	payload, err := json.Marshal(ErrorHandlingRuleSettings{
		Enabled: true, DefaultRetryCount: 1, Rules: rules,
	})
	require.NoError(t, err)
	repo := &gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyErrorHandlingRules: string(payload),
	}}
	return NewSettingService(repo, &config.Config{})
}

func newGrokMediaRuleService(t *testing.T, upstream HTTPUpstream, rules ...ErrorHandlingRule) *OpenAIGatewayService {
	t.Helper()
	return &OpenAIGatewayService{
		httpUpstream:   upstream,
		accountRepo:    &grokQuotaAccountRepo{},
		settingService: newGrokMediaRuleSettingService(t, rules...),
		cfg:            &config.Config{},
	}
}

func grokMediaRuleAccount() *Account {
	return &Account{
		ID: 90, Name: "grok-media-rule-account", Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://xai.test/v1"},
	}
}

func newGrokMediaRuleTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	return c, rec
}

// ==================== 三种动作：failover / retry / passthrough ====================

func TestGrokMediaErrorHandlingRuleActions(t *testing.T) {
	t.Run("failover", func(t *testing.T) {
		svc := newGrokMediaRuleService(t, nil, ErrorHandlingRule{
			ID: "grok-media-failover", StatusCodes: []int{429}, Action: ErrorHandlingActionFailover,
			Platforms: []string{PlatformGrok},
		})
		c, _ := newGrokMediaRuleTestContext()

		failoverErr, handled := svc.grokMediaErrorHandlingRuleOverride(context.Background(), c, grokMediaErrorHandlingRuleInput{
			Account: grokMediaRuleAccount(), StatusCode: http.StatusTooManyRequests,
			Body:     []byte(`{"error":{"code":"rate_limited","message":"rate limited"}}`),
			ReqModel: "grok-imagine", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.Equal(t, "grok-media-failover", failoverErr.ErrorRuleID)
		require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
		require.True(t, failoverErr.ShouldRetryNextAccount())
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)
	})

	t.Run("retry", func(t *testing.T) {
		retry := 3
		svc := newGrokMediaRuleService(t, nil, ErrorHandlingRule{
			ID: "grok-media-retry", StatusCodes: []int{500}, Action: ErrorHandlingActionRetry,
			RetryCount: &retry, Platforms: []string{PlatformGrok},
		})
		c, _ := newGrokMediaRuleTestContext()

		failoverErr, handled := svc.grokMediaErrorHandlingRuleOverride(context.Background(), c, grokMediaErrorHandlingRuleInput{
			Account: grokMediaRuleAccount(), StatusCode: http.StatusInternalServerError,
			Body:     []byte(`{"error":{"code":"internal","message":"boom"}}`),
			ReqModel: "grok-imagine", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.True(t, failoverErr.RetryableOnSameAccount)
		require.NotNil(t, failoverErr.RuleRetryLimit, "规则驱动的重试预算必须显式带出来")
		require.Equal(t, 3, *failoverErr.RuleRetryLimit)
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)
	})

	t.Run("passthrough", func(t *testing.T) {
		svc := newGrokMediaRuleService(t, nil, ErrorHandlingRule{
			ID: "grok-media-passthrough", StatusCodes: []int{503}, Action: ErrorHandlingActionPassthrough,
			Platforms: []string{PlatformGrok},
		})
		c, _ := newGrokMediaRuleTestContext()

		failoverErr, handled := svc.grokMediaErrorHandlingRuleOverride(context.Background(), c, grokMediaErrorHandlingRuleInput{
			Account: grokMediaRuleAccount(), StatusCode: http.StatusServiceUnavailable,
			Body:     []byte(`{"error":{"code":"overloaded","message":"overloaded"}}`),
			ReqModel: "grok-imagine", BuiltinWillFailover: true,
		})

		require.True(t, handled)
		require.NotNil(t, failoverErr)
		require.False(t, failoverErr.ShouldRetryNextAccount(), "passthrough 必须立刻停止换号")
		require.Equal(t, NextAccountStop, failoverErr.NextAccountAction)
		require.Equal(t, ErrorHandlingExhaustedActionPassthrough, failoverErr.ExhaustedAction)
		require.NotEmpty(t, failoverErr.SafeErrorType)
		require.NotEmpty(t, failoverErr.SafeErrorMessage)

		events := opsUpstreamErrorEvents(t, c)
		require.NotEmpty(t, events)
		require.Equal(t, "error_handling_rule_passthrough", events[len(events)-1].Kind)
	})
}

// ==================== 内置独占：内容策略拒绝规则抢不走 ====================

// grokMediaBuiltinOwnsError 本身的判定：只认 isGrokContentPolicyRejection，其余一律
// 交给规则引擎。
func TestGrokMediaBuiltinOwnsContentPolicyRejection(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name: "403 + 内容策略拒绝 归内置", statusCode: http.StatusForbidden,
			body: []byte(`{"error":{"code":"content_filter","message":"prohibited content"}}`), want: true,
		},
		{
			name: "同样的 body 但状态码不是内容策略拒绝状态码时不归内置", statusCode: http.StatusInternalServerError,
			body: []byte(`{"error":{"code":"content_filter","message":"prohibited content"}}`), want: false,
		},
		{
			name: "403 但不是内容策略拒绝的 body 时规则可覆盖", statusCode: http.StatusForbidden,
			body: []byte(`{"error":{"code":"forbidden","message":"some other reason"}}`), want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, grokMediaBuiltinOwnsError(tt.statusCode, "", tt.body))
		})
	}
}

// isGrokContentPolicyRejection 在 handleGrokMediaErrorResponse 内部先于规则引擎被
// 问到、并物理 return（见该函数体：先问 isGrokContentPolicyRejection 再问
// applyErrorPassthroughRule，规则引擎接线点在后面）。这里直接驱动
// handleGrokMediaErrorResponse（而不是单独调用 grokMediaBuiltinOwnsError 或
// grokMediaErrorHandlingRuleOverride）来证明：即便配了一条广泛匹配 403 的 failover
// 规则，这条错误最终返回的仍然是内置的内容策略拒绝文案，不是规则引擎产出的
// *UpstreamFailoverError。
func TestGrokMediaContentPolicyRejectionBypassesErrorHandlingRule(t *testing.T) {
	svc := newGrokMediaRuleService(t, nil, ErrorHandlingRule{
		ID: "broad-403", StatusCodes: []int{403}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformGrok},
	})
	c, rec := newGrokMediaRuleTestContext()
	account := grokMediaRuleAccount()
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"new_sensitive","message":"image is sensitive"}}`)),
	}

	_, err := svc.handleGrokMediaErrorResponse(context.Background(), resp, c, account, GrokMediaEndpointImagesGenerations, "req-1", "grok-imagine")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr),
		"规则引擎产出的错误是 *UpstreamFailoverError；这里必须不是，否则说明规则抢走了"+
			"确定性的内容策略拒绝错误")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid_request_error")

	events := opsUpstreamErrorEvents(t, c)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind,
			"不该出现规则接管的事件——内容策略拒绝必须由内置兜底处理")
	}
}

// ==================== 错误透传规则优先 ====================

func newGrokMediaMatchAllErrorPassthroughService(t *testing.T) *ErrorPassthroughService {
	t.Helper()
	respCode := http.StatusTeapot
	customMessage := "上游请求失败"
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{{
		ID: 1, Name: "grok-media-500", Enabled: true, Priority: 1,
		ErrorCodes:   []int{http.StatusInternalServerError},
		MatchMode:    model.MatchModeAll,
		Platforms:    []string{PlatformGrok},
		ResponseCode: &respCode, CustomMessage: &customMessage,
		SkipMonitoring: true,
	}})
	return svc
}

// 与 Gemini/Antigravity 不同：handleGrokMediaErrorResponse 里 applyErrorPassthroughRule
// 的查询排在错误处理规则接线点**之前**（brief 明确要求接线点在"透传规则查询"与
// "ShouldHandleErrorCode"之间），所以透传规则一旦匹配，函数在到达规则引擎接线点之前
// 就已经物理写完响应并 return——规则引擎在这条路径上连被问到的机会都没有，这是比
// Gemini 那种"规则引擎主动检测到透传规则命中后让路"更强的保证（结构上不可能被规则
// 抢走，而不是靠一次运行期判断）。
//
// 直接驱动 handleGrokMediaErrorResponse（真实端到端路径）来证明：即便配了一条会命中
// 的 failover 规则，最终状态码/消息体/OpsSkipPassthroughKey 三者都是透传规则产出的，
// 而不是规则引擎产出的。同时保留一次对 grokMediaErrorHandlingRuleOverride 的直接调用，
// 验证执行层自身的让路判断（executeErrorHandlingRule 内部的 errorPassthroughRuleMatches
// 检查）在被单独问到时也是正确的——这一段在当前 grok media 的调用顺序下是多一层保险，
// 但与其它平台的实现保持同一契约，避免以后接线顺序变化时才发现这里从来没测过。
func TestGrokMediaErrorHandlingRuleYieldsToPassthroughRule(t *testing.T) {
	account := grokMediaRuleAccount()
	body := []byte(`{"error":{"code":"internal","message":"boom"}}`)

	// 阶段一：直接调用 grokMediaErrorHandlingRuleOverride，验证执行层自身的让路判断。
	c1, _ := newGrokMediaRuleTestContext()
	svc := newGrokMediaRuleService(t, nil, ErrorHandlingRule{
		ID: "broad-500", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformGrok},
	})
	BindErrorPassthroughService(c1, newGrokMediaMatchAllErrorPassthroughService(t))
	_, handled := svc.grokMediaErrorHandlingRuleOverride(context.Background(), c1, grokMediaErrorHandlingRuleInput{
		Account: account, StatusCode: http.StatusInternalServerError, Body: body, ReqModel: "grok-imagine",
		BuiltinWillFailover: false,
	})
	require.False(t, handled, "内置不换号 + 透传规则命中 ⇒ 错误处理规则让路")

	// 内置要换号的分支不受影响：那条分支上本来就问不到透传规则。
	c1b, _ := newGrokMediaRuleTestContext()
	BindErrorPassthroughService(c1b, newGrokMediaMatchAllErrorPassthroughService(t))
	_, handled2 := svc.grokMediaErrorHandlingRuleOverride(context.Background(), c1b, grokMediaErrorHandlingRuleInput{
		Account: account, StatusCode: http.StatusInternalServerError, Body: body, ReqModel: "grok-imagine",
		BuiltinWillFailover: true,
	})
	require.True(t, handled2)

	// 阶段二：驱动真实的 handleGrokMediaErrorResponse 端到端路径，断言最终状态码 /
	// 消息体 / OpsSkipPassthroughKey 三者都来自透传规则，规则引擎接线点从未被问到。
	c2, rec := newGrokMediaRuleTestContext()
	BindErrorPassthroughService(c2, newGrokMediaMatchAllErrorPassthroughService(t))
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
	_, err := svc.handleGrokMediaErrorResponse(context.Background(), resp, c2, account, GrokMediaEndpointImagesGenerations, "req-2", "grok-imagine")
	require.Error(t, err)

	require.Equal(t, http.StatusTeapot, rec.Code)
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "上游请求失败", payload.Error.Message)

	skip, ok := c2.Get(OpsSkipPassthroughKey)
	require.True(t, ok, "OpsSkipPassthroughKey 必须被置位，避免下游重复应用透传规则")
	require.Equal(t, true, skip)

	events := opsUpstreamErrorEvents(t, c2)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind,
			"透传规则先于规则引擎接线点物理 return，规则引擎必须从未被问到")
	}
}

// ==================== 平台过滤 ====================

func TestGrokMediaErrorHandlingRule_OtherPlatformRuleDoesNotApply(t *testing.T) {
	svc := newGrokMediaRuleService(t, nil, ErrorHandlingRule{
		ID: "anthropic-only", StatusCodes: []int{500}, Action: ErrorHandlingActionPassthrough,
		Platforms: []string{PlatformAnthropic},
	})
	c, _ := newGrokMediaRuleTestContext()

	failoverErr, handled := svc.grokMediaErrorHandlingRuleOverride(context.Background(), c, grokMediaErrorHandlingRuleInput{
		Account: grokMediaRuleAccount(), StatusCode: http.StatusInternalServerError,
		Body:     []byte(`{"error":{"code":"internal","message":"boom"}}`),
		ReqModel: "grok-imagine", BuiltinWillFailover: true,
	})

	require.False(t, handled, "规则没勾 grok，不能命中")
	require.Nil(t, failoverErr)
	require.Empty(t, opsUpstreamErrorEvents(t, c), "未命中不得留下规则事件")
}

// ==================== #228 §六：video_status / video_content 查询例外 ====================

// video_status / video_content 查询绑定到创建任务时选中的原账号（owner binding，由
// internal/handler/grok_media.go 的 ResolveGrokMediaVideoRequestAccount 强制校验），
// 不能切换账号。这里直接驱动真实的 ForwardGrokMedia（package service 能触达的最深
// 真实路径：endpoint=GrokMediaEndpointVideoStatus，一路走到 handleGrokMediaErrorResponse
// 的接线点），而不是单独调用 endpoint.IsGenerationRequest() 或
// grokMediaErrorHandlingRuleOverride，证明即便配了一条会命中的 failover 规则，
// video_status 查询返回的错误也不是规则引擎产出的。
//
// 范围说明（诚实披露，不悄悄降级）：owner binding 本身（
// GrokMediaVideoRequestSessionHash / BindGrokMediaVideoRequestAccount /
// ResolveGrokMediaVideoRequestAccount 的缓存读写、以及 internal/handler/grok_media.go
// 里对 endpoint.IsVideoLookupRequest() 的强制校验与"立即终止换号"分支）完全生活在
// internal/handler 包，而不是这里测试的 internal/service 包——package service 的单元
// 测试没有能力独立驱动 handler 层那个选号前置校验。本测试能验证、且已经验证的是：
// service 层这个共用函数不会对 video_status 产出一个"看起来已经生效"的规则版
// failover 错误（ErrorRuleID 非空、Kind=error_handling_rule_failover）——这正是
// §六要防的"假生效"的根源：如果这里放行，规则的效果会在 ops_error_logs 里显得像是
// 生效了，即便 handler 层的 owner binding 最终确实会阻止真正换号。account 参数在
// 调用前后保持同一个对象、从未被替换，是 service 层能证明的"没有尝试切换账号"的最
// 强代理断言；真正跨进程的 owner binding 不变性需要一个 internal/handler 包的集成
// 测试才能覆盖，这不在本任务（#228 task-9）的文件列表内。
func TestGrokVideoStatusQueryBypassesErrorHandlingRule(t *testing.T) {
	c, _ := newGrokMediaRuleTestContext()
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/request-999", nil)
	account := grokMediaRuleAccount()
	originalAccount := account

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"internal","message":"boom"}}`)),
	}}
	svc := newGrokMediaRuleService(t, upstream, ErrorHandlingRule{
		ID: "broad-500-video-status", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformGrok},
	})

	_, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointVideoStatus, "request-999", nil, "")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "builtin 的 failover 分支本身也会产出 *UpstreamFailoverError，这里断言的是它不是规则引擎产出的")
	require.Empty(t, failoverErr.ErrorRuleID,
		"video_status 查询即便配了匹配规则也必须 handled=false：ErrorRuleID 非空说明规则引擎接管了这次查询，"+
			"会让 ops_error_logs 显得像 failover 已生效，而 owner binding 实际上仍会阻止真正换号")

	events := opsUpstreamErrorEvents(t, c)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind,
			"不该出现规则接管的事件——video_status 查询必须绕过规则引擎")
	}
	require.Same(t, originalAccount, account, "调用前后账号对象未被替换——service 层没有尝试切换账号")
}

// video_content 查询（内部先查一次 video_status，见 forwardGrokMediaVideoContent）
// 与上面的 video_status 直查共用同一个 handleGrokMediaErrorResponse 调用点
// （GrokMediaEndpointVideoContent），这里驱动 ForwardGrokMedia(endpoint=VideoContent)
// 走到内部的 status 子请求失败分支，验证同一例外同样生效。
func TestGrokVideoContentQueryBypassesErrorHandlingRule(t *testing.T) {
	c, _ := newGrokMediaRuleTestContext()
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/request-998/content", nil)
	account := grokMediaRuleAccount()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"internal","message":"boom"}}`)),
	}}
	svc := newGrokMediaRuleService(t, upstream, ErrorHandlingRule{
		ID: "broad-500-video-content", StatusCodes: []int{500}, Action: ErrorHandlingActionFailover,
		Platforms: []string{PlatformGrok},
	})

	_, err := svc.ForwardGrokMedia(context.Background(), c, account, GrokMediaEndpointVideoContent, "request-998", nil, "")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, failoverErr.ErrorRuleID,
		"video_content 查询即便配了匹配规则也必须 handled=false，理由同 video_status")

	events := opsUpstreamErrorEvents(t, c)
	for _, ev := range events {
		require.NotEqual(t, "error_handling_rule_failover", ev.Kind)
	}
}
