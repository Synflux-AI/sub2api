//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeOpenBalanceCache struct {
	balance float64
	err     error
	calls   int
}

func (f *fakeOpenBalanceCache) GetUserBalance(_ context.Context, _ int64) (float64, error) {
	f.calls++
	return f.balance, f.err
}

type fakeOpenBalanceUserReader struct {
	user  *service.User
	err   error
	calls int
}

func (f *fakeOpenBalanceUserReader) GetByID(_ context.Context, _ int64) (*service.User, error) {
	f.calls++
	return f.user, f.err
}

type fakeOpenBalanceToucher struct {
	calls   int
	userIDs []int64
	tokens  []string
}

func (f *fakeOpenBalanceToucher) TouchLastUsed(_ context.Context, userID int64, token string) {
	f.calls++
	f.userIDs = append(f.userIDs, userID)
	f.tokens = append(f.tokens, token)
}

const openBalanceTestToken = "sat-" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// setAccessTokenPlaintextForTest 写入认证中间件的私有 context 键。字面量随即用
// 导出的 getter 回读校验：Task 3 若改键名，这里会立刻炸掉，而不是让 touch 断言
// 静默变成「永远不 touch」的假绿。
func setAccessTokenPlaintextForTest(t *testing.T, c *gin.Context, token string) {
	t.Helper()
	c.Set("user_access_token_plaintext", token)
	got, ok := middleware2.GetUserAccessTokenPlaintextFromContext(c)
	require.True(t, ok, "context 键与 middleware.GetUserAccessTokenPlaintextFromContext 不一致")
	require.Equal(t, token, got)
}

func newOpenBalanceTestRouter(t *testing.T, h *OpenBalanceHandler, userID int64, token string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID})
		c.Set(string(middleware2.ContextKeyUserRole), service.RoleUser)
		if token != "" {
			setAccessTokenPlaintextForTest(t, c, token)
		}
		c.Next()
	})
	router.GET("/api/v1/open/balance", h.GetBalance)
	return router
}

func getOpenBalance(router *gin.Engine) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/open/balance", nil))
	return recorder
}

// 余额响应是**无信封扁平结构**：字段齐全、schema_version 为 1、no-store，
// 且两个金额都四舍五入到 4 位小数（float64 直出会带 …0001 噪声）。
func TestOpenBalanceResponseIsFlatRoundedAndNoStore(t *testing.T) {
	cache := &fakeOpenBalanceCache{balance: 12.34567891}
	users := &fakeOpenBalanceUserReader{user: &service.User{ID: 7, FrozenBalance: 0.987654321}}
	toucher := &fakeOpenBalanceToucher{}
	handler := &OpenBalanceHandler{billingCache: cache, users: users, accessTokens: toucher}

	recorder := getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	// 无信封：顶层没有 code / message / data。
	require.NotContains(t, payload, "code")
	require.NotContains(t, payload, "data")

	require.Equal(t, "sub2api.balance", payload["object"])
	require.EqualValues(t, 1, payload["schema_version"])
	require.Equal(t, "USD", payload["currency"])
	require.InDelta(t, 12.3457, payload["balance"], 0)
	require.InDelta(t, 0.9877, payload["frozen_balance"], 0)
	require.Contains(t, payload, "observed_at")
	require.NotEmpty(t, payload["observed_at"])

	// 舍入必须体现在序列化文本里，而不只是浮点近似。
	require.Contains(t, recorder.Body.String(), `"balance":12.3457`)
	require.Contains(t, recorder.Body.String(), `"frozen_balance":0.9877`)
}

func TestOpenBalanceTouchesLastUsedOnlyAfterSuccessfulRead(t *testing.T) {
	cache := &fakeOpenBalanceCache{balance: 5}
	users := &fakeOpenBalanceUserReader{user: &service.User{ID: 7}}
	toucher := &fakeOpenBalanceToucher{}
	handler := &OpenBalanceHandler{billingCache: cache, users: users, accessTokens: toucher}

	require.Equal(t, http.StatusOK, getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken)).Code)

	require.Equal(t, 1, toucher.calls)
	// (user_id, token) 双匹配：token 取自认证中间件写入的明文，不是重新查库拿到的。
	require.Equal(t, []int64{7}, toucher.userIDs)
	require.Equal(t, []string{openBalanceTestToken}, toucher.tokens)
}

// 余额读取失败：500 且**不** touch。last_used_at 的语义是「这枚令牌成功取到过
// 数据」，失败请求不该把它推新。
func TestOpenBalanceReadFailureReturns500AndSkipsTouch(t *testing.T) {
	cache := &fakeOpenBalanceCache{err: errors.New("redis and db both down")}
	users := &fakeOpenBalanceUserReader{user: &service.User{ID: 7}}
	toucher := &fakeOpenBalanceToucher{}
	handler := &OpenBalanceHandler{billingCache: cache, users: users, accessTokens: toucher}

	recorder := getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, 0, toucher.calls)
	require.Equal(t, 0, users.calls, "余额读失败后不该再去读用户")
	require.Empty(t, recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), "BALANCE_UNAVAILABLE")
}

// 冻结余额来自用户读取，它失败同样是 500 且不 touch。
func TestOpenBalanceUserReadFailureReturns500AndSkipsTouch(t *testing.T) {
	cache := &fakeOpenBalanceCache{balance: 5}
	users := &fakeOpenBalanceUserReader{err: service.ErrUserNotFound}
	toucher := &fakeOpenBalanceToucher{}
	handler := &OpenBalanceHandler{billingCache: cache, users: users, accessTokens: toucher}

	recorder := getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, 0, toucher.calls)
}

// 没有令牌明文（认证中间件未写入）时不 touch，也绝不 panic。
func TestOpenBalanceWithoutTokenPlaintextSkipsTouch(t *testing.T) {
	cache := &fakeOpenBalanceCache{balance: 5}
	users := &fakeOpenBalanceUserReader{user: &service.User{ID: 7}}
	toucher := &fakeOpenBalanceToucher{}
	handler := &OpenBalanceHandler{billingCache: cache, users: users, accessTokens: toucher}

	require.Equal(t, http.StatusOK, getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, "")).Code)
	require.Equal(t, 0, toucher.calls)
}

// 无认证主体（中间件缺位）时的失败响应与认证中间件字节级一致，不泄露差异。
func TestOpenBalanceWithoutAuthSubjectIsUnauthorized(t *testing.T) {
	handler := &OpenBalanceHandler{
		billingCache: &fakeOpenBalanceCache{balance: 5},
		users:        &fakeOpenBalanceUserReader{user: &service.User{ID: 7}},
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/open/balance", handler.GetBalance)

	recorder := getOpenBalance(router)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.JSONEq(t, `{"code":"INVALID_ACCESS_TOKEN","message":"invalid access token"}`, recorder.Body.String())
}

// nil 依赖不该 panic：NewOpenBalanceHandler 显式判空后才装进接口。
func TestNewOpenBalanceHandlerWithNilDepsFailsClosed(t *testing.T) {
	recorder := getOpenBalance(newOpenBalanceTestRouter(t, NewOpenBalanceHandler(nil, nil, nil), 7, openBalanceTestToken))
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
}

func TestRoundBalanceAmountKeepsFourDecimals(t *testing.T) {
	require.Equal(t, 12.3457, roundBalanceAmount(12.34567891))
	require.Equal(t, 0.0001, roundBalanceAmount(0.00005))
	require.Equal(t, 0.0, roundBalanceAmount(0.00004))
	require.Equal(t, -1.2346, roundBalanceAmount(-1.23456))
	require.Equal(t, 100.0, roundBalanceAmount(100))
}

// 令牌明文常量必须是服务层认可的格式，否则 TouchLastUsed 会在真实链路上被
// IsValidAccessTokenFormat 静默挡掉，而测试仍然通过。
func TestOpenBalanceTestTokenMatchesServiceFormat(t *testing.T) {
	require.True(t, service.IsValidAccessTokenFormat(openBalanceTestToken))
	require.True(t, strings.HasPrefix(openBalanceTestToken, "sat-"))
}

type openBalanceLogSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *openBalanceLogSink) WriteLogEvent(event *logger.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *openBalanceLogSink) find(t *testing.T, message string) *logger.LogEvent {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event != nil && event.Message == message {
			return event
		}
	}
	t.Fatalf("没有 message 为 %q 的日志事件（收到 %d 条）", message, len(s.events))
	return nil
}

// snapshot 把所有事件序列化成一段文本，用于哨兵扫描。
func (s *openBalanceLogSink) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var builder strings.Builder
	for _, event := range s.events {
		if event == nil {
			continue
		}
		builder.WriteString(event.Message)
		fmt.Fprintf(&builder, " %v\n", event.Fields)
	}
	return builder.String()
}

func newOpenBalanceLogSink(t *testing.T) *openBalanceLogSink {
	t.Helper()
	require.NoError(t, logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output:      logger.OutputOptions{ToStdout: false, ToFile: false},
		Sampling:    logger.SamplingOptions{Enabled: false},
	}))
	sink := &openBalanceLogSink{}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	return sink
}

// 面向客户的 500 必须留下运维可辨识的痕迹：AbortWithError 不打日志，
// RequestLogger 也不产出 per-request 访问行，不记的话「Redis 与 DB 双挂」和
// 「用户行消失」在线上完全无法区分。同时：日志绝不能带令牌明文，响应体必须
// 保持恒定文案、不泄露内部细节。
func TestOpenBalanceFailurePathsAreLoggedWithoutTokenPlaintext(t *testing.T) {
	const wantBody = `{"code":"BALANCE_UNAVAILABLE","message":"balance is temporarily unavailable"}`

	t.Run("balance read", func(t *testing.T) {
		sink := newOpenBalanceLogSink(t)
		handler := &OpenBalanceHandler{
			billingCache: &fakeOpenBalanceCache{err: errors.New("redis and db both down")},
			users:        &fakeOpenBalanceUserReader{user: &service.User{ID: 7}},
			accessTokens: &fakeOpenBalanceToucher{},
		}

		recorder := getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken))
		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		require.JSONEq(t, wantBody, recorder.Body.String())

		event := sink.find(t, "open_balance_read_failed")
		require.EqualValues(t, 7, event.Fields["user_id"])
		require.Contains(t, fmt.Sprint(event.Fields["error"]), "redis and db both down")
		require.NotContains(t, sink.snapshot(), openBalanceTestToken, "日志里绝不能出现令牌明文")
	})

	t.Run("user read", func(t *testing.T) {
		sink := newOpenBalanceLogSink(t)
		handler := &OpenBalanceHandler{
			billingCache: &fakeOpenBalanceCache{balance: 5},
			users:        &fakeOpenBalanceUserReader{err: service.ErrUserNotFound},
			accessTokens: &fakeOpenBalanceToucher{},
		}

		recorder := getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken))
		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		require.JSONEq(t, wantBody, recorder.Body.String())

		// 两条失败路径分开记：排查方向完全不同，合成一条就白记了。
		event := sink.find(t, "open_balance_user_read_failed")
		require.EqualValues(t, 7, event.Fields["user_id"])
		require.NotContains(t, sink.snapshot(), openBalanceTestToken)
	})

	t.Run("missing dependencies", func(t *testing.T) {
		sink := newOpenBalanceLogSink(t)
		recorder := getOpenBalance(newOpenBalanceTestRouter(t, NewOpenBalanceHandler(nil, nil, nil), 7, openBalanceTestToken))
		require.Equal(t, http.StatusInternalServerError, recorder.Code)
		require.JSONEq(t, wantBody, recorder.Body.String())

		event := sink.find(t, "open_balance_dependencies_missing")
		require.Equal(t, true, event.Fields["billing_cache_missing"])
		require.Equal(t, true, event.Fields["user_reader_missing"])
	})

	t.Run("success path stays silent", func(t *testing.T) {
		sink := newOpenBalanceLogSink(t)
		handler := &OpenBalanceHandler{
			billingCache: &fakeOpenBalanceCache{balance: 5},
			users:        &fakeOpenBalanceUserReader{user: &service.User{ID: 7}},
			accessTokens: &fakeOpenBalanceToucher{},
		}
		require.Equal(t, http.StatusOK, getOpenBalance(newOpenBalanceTestRouter(t, handler, 7, openBalanceTestToken)).Code)
		require.NotContains(t, sink.snapshot(), "open_balance_", "成功路径不该产生任何 open_balance_* 日志")
	})
}
