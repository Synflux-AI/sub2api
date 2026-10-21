package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newOpsAttrTestOpsService() *service.OpsService {
	return service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func TestOpsErrorLoggerMiddleware_RecordsRequestAttributesAndDuration(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 4)
	gin.SetMode(gin.TestMode)
	ops := newOpsAttrTestOpsService()

	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/responses", func(c *gin.Context) {
		effort, tier := "xhigh", "priority"
		service.SetOpsReasoningEffort(c, &effort)
		service.SetOpsServiceTier(c, &tier)
		time.Sleep(5 * time.Millisecond)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error", "message": "upstream failed"}})
	})
	router.POST("/v1/recovered", func(c *gin.Context) {
		effort := "low"
		service.SetOpsReasoningEffort(c, &effort)
		c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{AccountID: 7, UpstreamStatusCode: 500, Kind: "failover"}})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	job := <-opsErrorLogQueue
	require.Equal(t, "xhigh", job.entry.ReasoningEffort)
	require.Equal(t, "priority", job.entry.ServiceTier)
	require.Zero(t, job.entry.ImageCount, "非出图请求 image_count 为 NULL")
	require.NotNil(t, job.entry.DurationMs)
	require.GreaterOrEqual(t, *job.entry.DurationMs, 5)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/recovered", nil))
	job = <-opsErrorLogQueue
	require.Equal(t, "Recovered upstream error 500", job.entry.ErrorMessage)
	require.Equal(t, "low", job.entry.ReasoningEffort)
	require.NotNil(t, job.entry.DurationMs)
}

// WebSocket 在连接结束才落库：每条 turn 的错误必须带该 turn 标记时的属性与耗时，
// 而不是连接结束时最后一个 turn 的值。
func TestLogOpsStreamError_WSTurnUsesSnapshotAttributes(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 4)
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)

	service.BeginOpsStreamTurn(c, 1)
	first := "high"
	service.SetOpsReasoningEffort(c, &first)
	service.MarkOpsStreamFailure(c, "upstream_error", "", "turn one failed", http.StatusBadGateway)

	service.BeginOpsStreamTurn(c, 2)
	second := "low"
	service.SetOpsReasoningEffort(c, &second)
	service.MarkOpsStreamFailure(c, "upstream_error", "", "turn two failed", http.StatusBadGateway)

	logOpsStreamError(c, newOpsAttrTestOpsService(), http.StatusOK)
	require.Equal(t, int64(2), OpsErrorLogQueueLength())
	got := map[string]*service.OpsInsertErrorLogInput{}
	for range 2 {
		job := <-opsErrorLogQueue
		got[job.entry.ErrorMessage] = job.entry
	}
	require.Equal(t, "high", got["turn one failed"].ReasoningEffort)
	require.Equal(t, "low", got["turn two failed"].ReasoningEffort)
	require.NotNil(t, got["turn one failed"].DurationMs)
	require.NotNil(t, got["turn two failed"].DurationMs)
}

// Anthropic /v1/messages：转发失败时由 handler 补算的推理强度必须等于同一请求成功时
// stampForwardReasoningEffort 写进转发结果（即 usage_logs.reasoning_effort）的值。
func TestOpsErrorLoggerMiddleware_AnthropicMessagesFailureMatchesSuccessEffort(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		account *service.Account
		want    string
	}{
		{
			name:    "explicit output effort",
			body:    `{"model":"claude-sonnet-4-5","output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`,
			account: &service.Account{ID: 1, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey},
			want:    "high",
		},
		{
			name: "thinking default on account-mapped passback model",
			body: `{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":2048},"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`,
			account: &service.Account{ID: 2, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5": "glm-5"}}},
			want: "high",
		},
		{
			name:    "bedrock drops client effort",
			body:    `{"model":"claude-sonnet-4-5","output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`,
			account: &service.Account{ID: 3, Platform: service.PlatformAnthropic, Type: service.AccountTypeBedrock},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 2)
			gin.SetMode(gin.TestMode)
			parse := func() *service.ParsedRequest {
				parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(tc.body)), service.PlatformAnthropic)
				require.NoError(t, err)
				return parsed
			}

			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(newOpsAttrTestOpsService()))
			router.POST("/v1/messages", func(c *gin.Context) {
				// 与 gateway_handler 在 Forward 失败（result 为 nil）后的调用一致。
				setOpsForwardReasoningEffort(c, nil, parse(), tc.account)
				c.JSON(http.StatusBadGateway, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": "upstream failed"}})
			})
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
			job := <-opsErrorLogQueue

			parsed := parse()
			success := &service.ForwardResult{Model: parsed.Model, UpstreamModel: tc.account.GetMappedModel(parsed.Model)}
			stampForwardReasoningEffort(success, parsed, tc.account)
			successEffort := ""
			if success.ReasoningEffort != nil {
				successEffort = *success.ReasoningEffort
			}
			require.Equal(t, tc.want, job.entry.ReasoningEffort)
			require.Equal(t, successEffort, job.entry.ReasoningEffort)
		})
	}
}

func TestCyberOpsEntriesCarryRequestAttributes(t *testing.T) {
	duration := 42
	meta := cyberPolicyOpsErrorMeta{
		RequestAttributes: service.OpsRequestAttributes{ReasoningEffort: "medium", ServiceTier: "flex"},
		DurationMs:        &duration,
	}
	for _, entry := range []*service.OpsInsertErrorLogInput{
		buildCyberSessionBlockedOpsEntry(meta),
		buildCyberPolicyOpsErrorEntry(meta, &service.CyberPolicyMark{UpstreamStatus: http.StatusBadRequest}),
	} {
		require.Equal(t, "medium", entry.ReasoningEffort)
		require.Equal(t, "flex", entry.ServiceTier)
		require.Equal(t, &duration, entry.DurationMs)
	}
}

type opsAttrImagesAccountRepo struct {
	service.AccountRepository
	account service.Account
}

func (r opsAttrImagesAccountRepo) GetByID(context.Context, int64) (*service.Account, error) {
	account := r.account
	return &account, nil
}

func (r opsAttrImagesAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]service.Account, error) {
	return []service.Account{r.account}, nil
}

func (r opsAttrImagesAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return []service.Account{r.account}, nil
}

func (r opsAttrImagesAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]service.Account, error) {
	return []service.Account{r.account}, nil
}

type opsAttrImagesFailingUpstream struct{ service.HTTPUpstream }

func (opsAttrImagesFailingUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(bytes.NewBufferString(
			"data: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"server_error\",\"message\":\"image backend unavailable\"}}\n\n")),
	}, nil
}

// /v1/images/generations 失败：错误日志带请求侧尺寸档位、原始尺寸、返回格式与请求张数。
func TestOpsErrorLoggerMiddleware_ImagesFailureRecordsImageAttributes(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 4)
	gin.SetMode(gin.TestMode)
	groupID := int64(4130)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		opsAttrImagesAccountRepo{account: service.Account{
			ID: 1, Name: "image-account", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "token-1"},
		}},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, opsAttrImagesFailingUpstream{},
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	h := NewOpenAIGatewayHandler(gatewayService, service.NewConcurrencyService(nil), billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)

	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(newOpsAttrTestOpsService()))
	router.POST("/v1/images/generations", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
			ID: 99, GroupID: &groupID,
			Group: &service.Group{ID: groupID, AllowImageGeneration: true},
			User:  &service.User{ID: 100},
		})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})
		h.Images(c)
	})

	body := []byte(`{"model":"gpt-image-1","prompt":"draw a cat","size":"1536x1024","n":2}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	job := <-opsErrorLogQueue
	require.Equal(t, "2K", job.entry.ImageSize)
	require.Equal(t, "1536x1024", job.entry.ImageInputSize)
	require.Equal(t, "b64_json", job.entry.ImageResponseFormat, "OAuth 出图默认返回 b64_json，与成功时同源")
	require.Equal(t, 2, job.entry.ImageCount)
	require.NotNil(t, job.entry.DurationMs)
}
