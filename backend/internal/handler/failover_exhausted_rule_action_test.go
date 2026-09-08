//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func newExhaustedTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, rec
}

func rulePassthroughExhaustedError() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:       http.StatusTooManyRequests,
		ExhaustedAction:  service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:    "rate_limit_error",
		SafeErrorMessage: "upstream rate limited",
	}
}

// exhausted_action=passthrough 必须原样交付上游状态码与脱敏消息，
// 而不是退回「All available accounts exhausted」。
func TestHandleCCFailoverExhaustedHonorsRulePassthrough(t *testing.T) {
	c, rec := newExhaustedTestContext()
	h := &GatewayHandler{}

	h.handleCCFailoverExhausted(c, rulePassthroughExhaustedError(), service.PlatformOpenAI, false)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected upstream status 429 to be passed through, got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not CC-shaped JSON: %v body=%s", err, rec.Body.String())
	}
	if payload.Error.Type != "rate_limit_error" || payload.Error.Message != "upstream rate limited" {
		t.Fatalf("expected safe upstream error to be delivered, got %+v", payload.Error)
	}
}

// Gemini 的错误写出函数 googleError(c, code, msg) 没有错误类型参数，
// 所以只能断言消息，不能断言 SafeErrorType；SafeErrorType 仍是
// 「规则引擎已经填充完整」的前置判据之一（连同 SafeErrorMessage 一起判断）。
func TestHandleGeminiFailoverExhaustedHonorsRulePassthrough(t *testing.T) {
	c, rec := newExhaustedTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	h := &GatewayHandler{}

	h.handleGeminiFailoverExhausted(c, rulePassthroughExhaustedError())

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected upstream status 429 to be passed through, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body == "" {
		t.Fatal("expected a non-empty error body")
	} else if !strings.Contains(body, "upstream rate limited") {
		t.Fatalf("expected safe upstream message in body, got %s", body)
	}
}

// 未配规则时行为必须一行不变：仍是通用耗尽错误。
func TestHandleCCFailoverExhaustedWithoutRuleKeepsGenericError(t *testing.T) {
	c, rec := newExhaustedTestContext()
	h := &GatewayHandler{}

	h.handleCCFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: http.StatusInternalServerError}, service.PlatformOpenAI, false)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected upstream status to be kept, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "All available accounts exhausted") {
		t.Fatalf("expected the generic exhausted message, got %s", rec.Body.String())
	}
}

// #228 task-12 修复轮 1（Finding 2）：exhausted_passthrough 维度 6 行里有 4 行
// （Anthropic Messages / Gemini Messages / Gemini Native / Responses）此前只引用了
// 命中分支的测试，没有「完全未配规则」的未命中分支——TestXxxWithoutSafeErrorFallsBack
// 系列断的是「规则命中但缺 SafeErrorType/Message 时的安全阀回退」，跟「压根没配规则」
// 不是同一件事。下面三个补齐真正的未命中分支，模式与
// TestHandleCCFailoverExhaustedWithoutRuleKeepsGenericError 一致：喂一个不带
// ExhaustedAction 的 UpstreamFailoverError，断言存量通用文案原样不变。
// Gemini Messages 与 Gemini Native 共用同一个 handleGeminiFailoverExhausted，
// 与命中分支的既有共享假设保持一致，这里只写一份。

func TestHandleAnthropicFailoverExhaustedWithoutRuleKeepsGenericError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, &service.UpstreamFailoverError{
		StatusCode: http.StatusInternalServerError,
	}, false)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected the built-in mapUpstreamError mapping to be kept, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Upstream service temporarily unavailable") {
		t.Fatalf("expected the generic exhausted message, got %s", rec.Body.String())
	}
}

func TestHandleGeminiFailoverExhaustedWithoutRuleKeepsGenericError(t *testing.T) {
	c, rec := newExhaustedTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	h := &GatewayHandler{}

	h.handleGeminiFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: http.StatusInternalServerError})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected the built-in mapGeminiUpstreamError mapping to be kept, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Upstream service temporarily unavailable") {
		t.Fatalf("expected the generic exhausted message, got %s", rec.Body.String())
	}
}

func TestHandleResponsesFailoverExhaustedWithoutRuleKeepsGenericError(t *testing.T) {
	c, rec := newExhaustedTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h := &GatewayHandler{}

	h.handleResponsesFailoverExhausted(c, &service.UpstreamFailoverError{StatusCode: http.StatusInternalServerError}, false)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected upstream status to be kept, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "All available accounts exhausted") {
		t.Fatalf("expected the generic exhausted message, got %s", rec.Body.String())
	}
}

// FIX 3（#228 最终评审）：耗尽后走 passthrough 时，如果 StatusCode 是传输层失败
// 合成的虚拟 502（SyntheticStatus=true），不能把它写进 ops_error_logs 顶层的
// upstream_status_code——那一列为 NULL 正是"这是传输层失败、根本没有 HTTP 响应"
// 的判定依据。客户端仍然要拿到这个合成状态码（这是既有行为，FIX 3 不改）。
// CC / Gemini / Responses 三个 handler 各自一份拷贝，必须分别钉住。
func syntheticExhaustedError() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:       http.StatusBadGateway,
		ResponseBody:     []byte(`{"error":{"type":"upstream_error","message":"upstream request failed: connection reset"}}`),
		ExhaustedAction:  service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:    "upstream_error",
		SafeErrorMessage: "upstream request failed: connection reset",
		SyntheticStatus:  true,
	}
}

func TestHandleCCFailoverExhausted_SyntheticStatusNotRecordedAsUpstreamStatus(t *testing.T) {
	c, rec := newExhaustedTestContext()
	h := &GatewayHandler{}

	h.handleCCFailoverExhausted(c, syntheticExhaustedError(), service.PlatformOpenAI, false)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected the synthetic status to still be delivered to the client, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := c.Get(service.OpsUpstreamStatusCodeKey); ok {
		t.Fatal("合成状态码只能用于客户端响应，不得落进 ops_error_logs 顶层列")
	}
}

func TestHandleGeminiFailoverExhausted_SyntheticStatusNotRecordedAsUpstreamStatus(t *testing.T) {
	c, rec := newExhaustedTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", nil)
	h := &GatewayHandler{}

	h.handleGeminiFailoverExhausted(c, syntheticExhaustedError())

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected the synthetic status to still be delivered to the client, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := c.Get(service.OpsUpstreamStatusCodeKey); ok {
		t.Fatal("合成状态码只能用于客户端响应，不得落进 ops_error_logs 顶层列")
	}
}

func TestHandleResponsesFailoverExhausted_SyntheticStatusNotRecordedAsUpstreamStatus(t *testing.T) {
	c, rec := newExhaustedTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h := &GatewayHandler{}

	h.handleResponsesFailoverExhausted(c, syntheticExhaustedError(), false)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected the synthetic status to still be delivered to the client, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := c.Get(service.OpsUpstreamStatusCodeKey); ok {
		t.Fatal("合成状态码只能用于客户端响应，不得落进 ops_error_logs 顶层列")
	}
}

// fakeErrorPassthroughRepo 是 service.ErrorPassthroughRepository 的最小实现，
// 只用来把固定规则集喂给 service.NewErrorPassthroughService 的启动加载。
// handler 包测试里没有现成的 mock（那个 mock 在 internal/service 包内且未导出），
// 按生产代码同样的构造方式（repo + cache，cache 传 nil 即可）自己搭一个。
type fakeErrorPassthroughRepo struct {
	rules []*model.ErrorPassthroughRule
}

func (f *fakeErrorPassthroughRepo) List(ctx context.Context) ([]*model.ErrorPassthroughRule, error) {
	return f.rules, nil
}

func (f *fakeErrorPassthroughRepo) GetByID(ctx context.Context, id int64) (*model.ErrorPassthroughRule, error) {
	for _, r := range f.rules {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, nil
}

func (f *fakeErrorPassthroughRepo) Create(ctx context.Context, rule *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	return rule, nil
}

func (f *fakeErrorPassthroughRepo) Update(ctx context.Context, rule *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	return rule, nil
}

func (f *fakeErrorPassthroughRepo) Delete(ctx context.Context, id int64) error {
	return nil
}

// 错误处理规则的 exhausted_action=passthrough 不能越过管理员显式配置的错误透传规则。
// 两者都命中时，错误透传规则（老机制）必须赢，且 skip_monitoring 要落地。
// TestHandleCCFailoverExhaustedRuleEngineWinsOverErrorPassthroughRule 验证
// 2026-09-08 项目所有者的反转决定：两个机制同时命中同一个错误时，错误处理规则
// 引擎的 exhausted_action=passthrough 优先于错误透传规则（旧名
// TestHandleCCFailoverExhaustedErrorPassthroughRuleWinsOverRuleEngine，旧语义
// 正相反）。
func TestHandleCCFailoverExhaustedRuleEngineWinsOverErrorPassthroughRule(t *testing.T) {
	customMessage := "passthrough rule wins"
	responseCode := http.StatusConflict
	rule := &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "test-priority-rule",
		Enabled:         true,
		Priority:        1,
		ErrorCodes:      []int{http.StatusTooManyRequests},
		Platforms:       []string{model.PlatformOpenAI},
		MatchMode:       model.MatchModeAny,
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		PassthroughBody: false,
		CustomMessage:   &customMessage,
		SkipMonitoring:  true,
	}
	svc := service.NewErrorPassthroughService(&fakeErrorPassthroughRepo{rules: []*model.ErrorPassthroughRule{rule}}, nil)

	c, rec := newExhaustedTestContext()
	h := &GatewayHandler{errorPassthroughService: svc}

	failoverErr := &service.UpstreamFailoverError{
		StatusCode:       http.StatusTooManyRequests,
		ResponseBody:     []byte(`{"error":"rate limited upstream, do not leak this"}`),
		ExhaustedAction:  service.ErrorHandlingExhaustedActionPassthrough,
		SafeErrorType:    "rate_limit_error",
		SafeErrorMessage: "upstream rate limited",
	}

	h.handleCCFailoverExhausted(c, failoverErr, service.PlatformOpenAI, false)

	// 规则引擎胜出：状态码与消息来自 failoverErr 自身（规则引擎算出来的
	// StatusCode/SafeErrorType/SafeErrorMessage），不是错误透传规则的
	// response_code/custom_message。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rule engine's upstream status %d to win, got %d body=%s", http.StatusTooManyRequests, rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not CC-shaped JSON: %v body=%s", err, rec.Body.String())
	}
	if payload.Error.Type != "rate_limit_error" || payload.Error.Message != "upstream rate limited" {
		t.Fatalf("expected SafeErrorType/SafeErrorMessage to win over the error-passthrough rule's custom_message, got %+v", payload.Error)
	}
	if payload.Error.Message == customMessage {
		t.Fatal("error-passthrough rule's custom_message leaked through — rule engine should have won")
	}
	if _, exists := c.Get(service.OpsSkipPassthroughKey); exists {
		t.Fatal("OpsSkipPassthroughKey must not be set — the error-passthrough rule should never be consulted when the rule engine wins")
	}
}

// TestHandleCCFailoverExhaustedErrorPassthroughRuleAloneStillApplies 是"透传规则
// 单独命中仍然生效"的守护测试：failoverErr 没有 ExhaustedAction=passthrough
// （规则引擎结构性不可能在这条分支命中），只有 errorPassthroughService 命中。
// 上面的反转只改变"两者同时命中"这一种碰撞，这条覆盖率此前从未独立存在过（旧版
// WinsOverRuleEngine 测试里两个机制永远同时配置），必须补一个只有透传规则单独
// 生效的用例，证明顺序反转没有连带破坏它。
func TestHandleCCFailoverExhaustedErrorPassthroughRuleAloneStillApplies(t *testing.T) {
	customMessage := "passthrough rule wins"
	responseCode := http.StatusConflict
	rule := &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "test-priority-rule",
		Enabled:         true,
		Priority:        1,
		ErrorCodes:      []int{http.StatusTooManyRequests},
		Platforms:       []string{model.PlatformOpenAI},
		MatchMode:       model.MatchModeAny,
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		PassthroughBody: false,
		CustomMessage:   &customMessage,
		SkipMonitoring:  true,
	}
	svc := service.NewErrorPassthroughService(&fakeErrorPassthroughRepo{rules: []*model.ErrorPassthroughRule{rule}}, nil)

	c, rec := newExhaustedTestContext()
	h := &GatewayHandler{errorPassthroughService: svc}

	// 没有任何错误处理规则命中（ExhaustedAction 为空），只有错误透传规则命中。
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusTooManyRequests,
		ResponseBody: []byte(`{"error":"rate limited upstream, do not leak this"}`),
	}

	h.handleCCFailoverExhausted(c, failoverErr, service.PlatformOpenAI, false)

	if rec.Code != responseCode {
		t.Fatalf("expected error-passthrough rule's response_code %d to apply, got %d body=%s", responseCode, rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not CC-shaped JSON: %v body=%s", err, rec.Body.String())
	}
	if payload.Error.Message != customMessage {
		t.Fatalf("expected error-passthrough rule's custom_message to apply, got %+v", payload.Error)
	}
	if _, exists := c.Get(service.OpsSkipPassthroughKey); !exists {
		t.Fatal("expected skip_monitoring=true rule to set OpsSkipPassthroughKey")
	}
}
