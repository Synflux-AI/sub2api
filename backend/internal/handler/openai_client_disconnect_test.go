package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type mockDisconnectAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r mockDisconnectAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r mockDisconnectAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r mockDisconnectAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r mockDisconnectAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r mockDisconnectAccountRepo) accountsForPlatform(platform string) []service.Account {
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out
}

type mockDisconnectUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	onFirstDo  func()
	returnErr  error
	statusCode int
	body       string
}

func (u *mockDisconnectUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.onFirstDo != nil {
		u.onFirstDo()
	}
	if u.returnErr != nil {
		return nil, u.returnErr
	}
	status := u.statusCode
	if status == 0 {
		status = http.StatusOK
	}
	body := u.body
	if body == "" {
		body = `{"id":"test","choices":[{"message":{"content":"hi"}}]}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}, nil
}

func newDisconnectTestHandler(t *testing.T, upstream service.HTTPUpstream) (*OpenAIGatewayHandler, *service.OpenAIGatewayService) {
	t.Helper()
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "test-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1"},
		},
	}
	accountRepo := mockDisconnectAccountRepo{accounts: accounts}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 1
	return handler, gatewayService
}

func newDisconnectMessagesTestContext(t *testing.T, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := int64(3131)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                    groupID,
			Platform:              service.PlatformOpenAI,
			AllowMessagesDispatch: true,
		},
		User: &service.User{ID: 100, Balance: 100},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

func newDisconnectChatCompletionsTestContext(t *testing.T, ctx context.Context) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	groupID := int64(3131)
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
		},
		User: &service.User{ID: 100, Balance: 100},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 100, Concurrency: 0})
	return c, rec
}

func TestOpenAIEnsureForwardErrorResponse_SkipsCanceledClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil).WithContext(ctx)
	cancel()

	h := &OpenAIGatewayHandler{}
	require.False(t, h.ensureForwardErrorResponse(c, true))
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Empty(t, recorder.Body.String())
}

func TestOpenAIEnsureAnthropicErrorResponse_SkipsCanceledClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	cancel()

	h := &OpenAIGatewayHandler{}
	require.False(t, h.ensureAnthropicErrorResponse(c, false))
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Empty(t, recorder.Body.String())
}

func TestOpenAIGatewayHandlerMessages_ClientDisconnectReturns499WithoutSchedulePenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream := &mockDisconnectUpstream{
		onFirstDo: cancel,
		returnErr: context.Canceled,
	}
	handler, gatewaySvc := newDisconnectTestHandler(t, upstream)

	var scheduleCalls atomic.Int64
	gatewaySvc.SetOnReportScheduleResultForTest(func(account *service.Account, model string, success bool, firstTokenMs *int, err error) {
		scheduleCalls.Add(1)
	})

	c, rec := newDisconnectMessagesTestContext(t, ctx)
	handler.Messages(c)

	require.Equal(t, statusClientClosedRequest, c.Writer.Status(), "response status must be 499")
	require.Zero(t, rec.Body.Len(), "no error response body must be written on client disconnect")
	require.Zero(t, scheduleCalls.Load(), "schedule failure must not be reported on client disconnect")
}

func TestOpenAIGatewayHandlerMessages_UpstreamErrorReturns502AndReportsScheduleFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &mockDisconnectUpstream{
		statusCode: http.StatusBadGateway,
		body:       `<html>502 Bad Gateway</html>`,
	}
	handler, gatewaySvc := newDisconnectTestHandler(t, upstream)

	var scheduleFailures atomic.Int64
	gatewaySvc.SetOnReportScheduleResultForTest(func(account *service.Account, model string, success bool, firstTokenMs *int, err error) {
		if !success {
			scheduleFailures.Add(1)
		}
	})

	c, rec := newDisconnectMessagesTestContext(t, nil)
	handler.Messages(c)

	require.Equal(t, http.StatusBadGateway, rec.Code, "authentic upstream error must return 502")
	require.NotEmpty(t, rec.Body.String(), "error response body must be written")
	require.Positive(t, scheduleFailures.Load(), "schedule failure must be reported on authentic upstream error")
}

func TestOpenAIGatewayHandlerChatCompletions_ClientDisconnectReturns499WithoutSchedulePenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	upstream := &mockDisconnectUpstream{
		onFirstDo: cancel,
		returnErr: context.Canceled,
	}
	handler, gatewaySvc := newDisconnectTestHandler(t, upstream)

	var scheduleCalls atomic.Int64
	gatewaySvc.SetOnReportScheduleResultForTest(func(account *service.Account, model string, success bool, firstTokenMs *int, err error) {
		scheduleCalls.Add(1)
	})

	c, rec := newDisconnectChatCompletionsTestContext(t, ctx)
	handler.ChatCompletions(c)

	require.Equal(t, statusClientClosedRequest, c.Writer.Status(), "response status must be 499")
	require.Zero(t, rec.Body.Len(), "no error response body must be written on client disconnect")
	require.Zero(t, scheduleCalls.Load(), "schedule failure must not be reported on client disconnect")
}

func TestOpenAIGatewayHandlerChatCompletions_UpstreamErrorReturns502AndReportsScheduleFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := &mockDisconnectUpstream{
		statusCode: http.StatusBadGateway,
		body:       `<html>502 Bad Gateway</html>`,
	}
	handler, gatewaySvc := newDisconnectTestHandler(t, upstream)

	var scheduleFailures atomic.Int64
	gatewaySvc.SetOnReportScheduleResultForTest(func(account *service.Account, model string, success bool, firstTokenMs *int, err error) {
		if !success {
			scheduleFailures.Add(1)
		}
	})

	c, rec := newDisconnectChatCompletionsTestContext(t, nil)
	handler.ChatCompletions(c)

	require.Equal(t, http.StatusBadGateway, rec.Code, "authentic upstream error must return 502")
	require.NotEmpty(t, rec.Body.String(), "error response body must be written")
	require.Positive(t, scheduleFailures.Load(), "schedule failure must be reported on authentic upstream error")
}
