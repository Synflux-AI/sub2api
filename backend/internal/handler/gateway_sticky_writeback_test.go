//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stickyWritebackContextKey struct{}

type stickyWritebackCall struct {
	err      error
	groupID  int64
	account  int64
	session  string
	ttl      time.Duration
	value    any
	deadline time.Time
}

type stickyWritebackCache struct {
	service.GatewayCache
	accountID int64
	finished  bool
	calls     []stickyWritebackCall
}

func (s *stickyWritebackCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return s.accountID, nil
}

func (s *stickyWritebackCache) SetSessionAccountID(ctx context.Context, groupID int64, session string, account int64, ttl time.Duration) error {
	if s.finished {
		deadline, _ := ctx.Deadline()
		s.calls = append(s.calls, stickyWritebackCall{
			err: ctx.Err(), groupID: groupID, account: account, session: session, ttl: ttl,
			value: ctx.Value(stickyWritebackContextKey{}), deadline: deadline,
		})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.accountID = account
	return nil
}

func (s *stickyWritebackCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (s *stickyWritebackCache) DeleteSessionAccountID(context.Context, int64, string) error {
	s.accountID = 0
	return nil
}

type stickyWritebackBody struct {
	io.Reader
	finish func()
}

func (b *stickyWritebackBody) Close() error {
	b.finish()
	return nil
}

func TestGatewayMessagesStickyWritebackAfterForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// metadata=true 走 metadata.user_id 会话（原粘性语义）；metadata=false 走
	// 定深锚点（#347，默认开启）：粘性账号被否决换号后，锚点改写为实际服务的账号。
	for _, metadata := range []bool{true, false} {
		for _, stream := range []bool{false, true} {
			name := "buffered"
			if stream {
				name = "streaming"
			}
			if metadata {
				name = "metadata/" + name
			} else {
				name = "anchor/" + name
			}
			for _, tt := range []struct {
				name             string
				bound            int64
				cancel           bool
				fail             bool
				wantWrites       int
				wantAnchorWrites int
			}{
				{name: "new binding", wantWrites: 1, wantAnchorWrites: 1},
				{name: "new binding after disconnect", cancel: true, wantWrites: 1, wantAnchorWrites: 1},
				{name: "refresh binding", bound: 9401, wantWrites: 1, wantAnchorWrites: 1},
				{name: "refresh after disconnect", bound: 9401, cancel: true, wantWrites: 1, wantAnchorWrites: 1},
				{name: "different binding", bound: 9499, cancel: true, wantAnchorWrites: 1},
				{name: "forward failed", cancel: true, fail: true},
			} {
				t.Run(name+"/"+tt.name, func(t *testing.T) {
					groupID := int64(9400)
					group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive, RateMultiplier: 1}
					account := &service.Account{
						ID: 9401, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
						Status: service.StatusActive, Schedulable: true, Concurrency: 1,
						Credentials:   map[string]any{"api_key": "test-key"},
						Extra:         map[string]any{"anthropic_passthrough": true},
						AccountGroups: []service.AccountGroup{{AccountID: 9401, GroupID: groupID}},
					}
					cache := &stickyWritebackCache{accountID: tt.bound}
					ctx := context.WithValue(context.Background(), stickyWritebackContextKey{}, "request-scoped value")
					ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
					t.Cleanup(cancel)
					responseBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
					contentType := "application/json"
					if stream {
						responseBody = compatStreamUsagePartialStream + compatStreamUsageCompletion
						contentType = "text/event-stream"
					}
					status := http.StatusOK
					if tt.fail {
						status = http.StatusBadRequest
						responseBody = `{"type":"error","error":{"type":"invalid_request_error","message":"invalid input"}}`
					}
					upstream := &compatStreamUsageUpstream{response: &http.Response{
						StatusCode: status,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body: &stickyWritebackBody{Reader: strings.NewReader(responseBody), finish: func() {
							// Forward closes its upstream body on return. Cancel precisely
							// after that work, before the handler's terminal sticky write.
							cache.finished = true
							if tt.cancel {
								cancel()
							}
						}},
					}}
					cfg := &config.Config{RunMode: config.RunModeSimple}
					other := *account
					other.ID = 9499
					other.Schedulable = false
					snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account, &other}}, nil, nil, nil, nil)
					gw := service.NewGatewayService(
						nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, cache, cfg,
						snapshot, nil, service.NewBillingService(cfg, nil), nil, nil, nil, upstream,
						&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
					)
					billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
					t.Cleanup(billingCache.Stop)
					h := &GatewayHandler{
						gatewayService: gw, billingCacheService: billingCache, cfg: cfg, maxAccountSwitches: 1,
						concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
					}
					requestBody := `{"model":"claude-sonnet-4.5","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"sticky writeback test"}]}`
					if stream {
						requestBody = strings.Replace(requestBody, `"stream":false`, `"stream":true`, 1)
					}
					if metadata {
						requestBody = strings.Replace(requestBody, `"max_tokens":16,`, `"max_tokens":16,"metadata":{"user_id":"user_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2_account__session_123e4567-e89b-12d3-a456-426614174000"},`, 1)
					}
					wantWrites := tt.wantWrites
					if !metadata {
						wantWrites = tt.wantAnchorWrites
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody)).WithContext(ctx)
					c.Request.Header.Set("Content-Type", "application/json")
					key := &service.APIKey{ID: 9402, UserID: 9403, GroupID: &groupID, Group: group, User: &service.User{ID: 9403, Balance: 100}}
					c.Set(string(middleware.ContextKeyAPIKey), key)
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9403})

					h.Messages(c)

					require.Equal(t, 1, upstream.calls)
					require.True(t, cache.finished)
					require.Equal(t, status, rec.Code)
					if tt.cancel {
						require.ErrorIs(t, ctx.Err(), context.Canceled)
					}
					require.Len(t, cache.calls, wantWrites)
					for _, call := range cache.calls {
						if metadata {
							require.Equal(t, "123e4567-e89b-12d3-a456-426614174000", call.session)
						} else {
							require.True(t, strings.HasPrefix(call.session, "anchor:"), "anchor session key must use its own prefix: %q", call.session)
						}
						require.NoError(t, call.err, "a completed upstream request must retain its terminal sticky write")
						require.Equal(t, groupID, call.groupID)
						require.Equal(t, account.ID, call.account)
						require.NotEmpty(t, call.session)
						require.Positive(t, call.ttl)
						require.Equal(t, "request-scoped value", call.value)
						require.False(t, call.deadline.IsZero(), "the detached cache write must have a deadline")
						require.WithinDuration(t, time.Now(), call.deadline, 5*time.Second)
					}
				})
			}
		}
	}
}
