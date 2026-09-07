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
func TestHandleCCFailoverExhaustedErrorPassthroughRuleWinsOverRuleEngine(t *testing.T) {
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

	if rec.Code != responseCode {
		t.Fatalf("expected error-passthrough rule's response_code %d to win, got %d body=%s", responseCode, rec.Code, rec.Body.String())
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
		t.Fatalf("expected error-passthrough rule's custom_message to win over SafeErrorMessage, got %+v", payload.Error)
	}
	if _, exists := c.Get(service.OpsSkipPassthroughKey); !exists {
		t.Fatal("expected skip_monitoring=true rule to set OpsSkipPassthroughKey")
	}
}
