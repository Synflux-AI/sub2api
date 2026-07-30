//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeAccessTokenAuthenticator 只实现 accessTokenAuthenticator 这一个方法并记
// 录调用次数。它没有 TouchLastUsed 方法可供中间件调用——这本身就是「中间件绝不
// 触发 last_used_at 写入」的编译期证明；calls 计数器再从行为上钉住它。
type fakeAccessTokenAuthenticator struct {
	calls        atomic.Int32
	authenticate func(ctx context.Context, token string) (*service.User, error)
}

func (f *fakeAccessTokenAuthenticator) Authenticate(ctx context.Context, token string) (*service.User, error) {
	f.calls.Add(1)
	if f.authenticate != nil {
		return f.authenticate(ctx, token)
	}
	return nil, service.ErrInvalidAccessToken
}

func newAccessTokenTestAPIKeyService(cfg *config.Config) *service.APIKeyService {
	if cfg == nil {
		cfg = &config.Config{RunMode: config.RunModeSimple}
	}
	return service.NewAPIKeyService(&stubApiKeyRepo{}, nil, nil, nil, nil, nil, cfg)
}

func newAccessTokenAuthRouter(t *testing.T, authenticator accessTokenAuthenticator, apiKeyService *service.APIKeyService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.HandlerFunc(UserAccessTokenAuthMiddleware(userAccessTokenAuth(authenticator, apiKeyService))))
	r.GET("/t", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func accessTokenRequest(headers map[string]string, rawQuery string) *http.Request {
	target := "/t"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "203.0.113.20:12345"
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req
}

// validAccessTokenFixture 满足 IsValidAccessTokenFormat：sat- + 64 位十六进制。
var validAccessTokenFixture = "sat-" + strings.Repeat("0123456789abcdef", 4)

func TestUserAccessTokenAuthNoCredentialUnauthorized(t *testing.T) {
	fake := &fakeAccessTokenAuthenticator{}
	r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, accessTokenRequest(nil, ""))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ACCESS_TOKEN")
	require.Zero(t, fake.calls.Load(), "missing credential must never reach Authenticate")
}

func TestUserAccessTokenAuthInvalidTokenUnauthorized(t *testing.T) {
	fake := &fakeAccessTokenAuthenticator{authenticate: func(context.Context, string) (*service.User, error) {
		return nil, service.ErrInvalidAccessToken
	}}
	r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, accessTokenRequest(map[string]string{"Authorization": "Bearer " + validAccessTokenFixture}, ""))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ACCESS_TOKEN")
	require.Equal(t, int32(1), fake.calls.Load())
}

func TestUserAccessTokenAuthDeactivatedUserUnauthorized(t *testing.T) {
	// Authenticate 对「令牌存在但用户被停用」也返回 ErrInvalidAccessToken——
	// 服务层已经把这条信息收敛掉了，中间件这里同样拿不到区分信号。
	fake := &fakeAccessTokenAuthenticator{authenticate: func(context.Context, string) (*service.User, error) {
		return nil, service.ErrInvalidAccessToken
	}}
	r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, accessTokenRequest(map[string]string{"Authorization": "Bearer " + validAccessTokenFixture}, ""))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ACCESS_TOKEN")
}

// TestUserAccessTokenAuthResponsesAreByteIdentical 钉住 §15.4 的核心属性：无
// 凭据、格式非法、令牌不存在、用户被停用，四种情况必须产生完全相同的响应体，
// 不能让探测者靠响应差异区分具体原因。
func TestUserAccessTokenAuthResponsesAreByteIdentical(t *testing.T) {
	authFails := func(context.Context, string) (*service.User, error) {
		return nil, service.ErrInvalidAccessToken
	}

	noCred := httptest.NewRecorder()
	newAccessTokenAuthRouter(t, &fakeAccessTokenAuthenticator{}, newAccessTokenTestAPIKeyService(nil)).
		ServeHTTP(noCred, accessTokenRequest(nil, ""))

	malformed := httptest.NewRecorder()
	newAccessTokenAuthRouter(t, &fakeAccessTokenAuthenticator{}, newAccessTokenTestAPIKeyService(nil)).
		ServeHTTP(malformed, accessTokenRequest(map[string]string{"Authorization": "Bearer sk-not-an-access-token"}, ""))

	unknownToken := httptest.NewRecorder()
	newAccessTokenAuthRouter(t, &fakeAccessTokenAuthenticator{authenticate: authFails}, newAccessTokenTestAPIKeyService(nil)).
		ServeHTTP(unknownToken, accessTokenRequest(map[string]string{"Authorization": "Bearer " + validAccessTokenFixture}, ""))

	deactivatedUser := httptest.NewRecorder()
	newAccessTokenAuthRouter(t, &fakeAccessTokenAuthenticator{authenticate: authFails}, newAccessTokenTestAPIKeyService(nil)).
		ServeHTTP(deactivatedUser, accessTokenRequest(map[string]string{"Authorization": "Bearer " + validAccessTokenFixture}, ""))

	for _, w := range []*httptest.ResponseRecorder{noCred, malformed, unknownToken, deactivatedUser} {
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}
	require.Equal(t, noCred.Body.String(), malformed.Body.String())
	require.Equal(t, noCred.Body.String(), unknownToken.Body.String())
	require.Equal(t, noCred.Body.String(), deactivatedUser.Body.String())
}

// TestUserAccessTokenAuthRejectsGatewayAPIKey 是反向隔离测试（issue §11）：一个
// 合法的 sk- 网关 API Key 绝不能通过用户级 access token 中间件认证——它连格式
// 校验都过不了（前缀不是 sat-），因此永远不会送进 Authenticate。
func TestUserAccessTokenAuthRejectsGatewayAPIKey(t *testing.T) {
	fake := &fakeAccessTokenAuthenticator{authenticate: func(context.Context, string) (*service.User, error) {
		t.Fatal("sk- gateway API key must not reach Authenticate")
		return nil, nil
	}}
	r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, accessTokenRequest(map[string]string{
		"Authorization": "Bearer sk-" + strings.Repeat("a", 48),
	}, ""))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_ACCESS_TOKEN")
	require.Zero(t, fake.calls.Load())
}

// TestUserAccessTokenAuthBadInputNeverReachesRepository 钉住 §15.2/§15.5：坏前
// 缀、非 hex、超长 header、query 凭据全部在格式/长度门被挡下，用调用计数器
// （而非走读代码）断言零次查库。
func TestUserAccessTokenAuthBadInputNeverReachesRepository(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		query   string
	}{
		{name: "bad_prefix", headers: map[string]string{"Authorization": "Bearer sat_" + strings.Repeat("0", 64)}},
		{name: "non_hex", headers: map[string]string{"Authorization": "Bearer sat-" + strings.Repeat("g", 64)}},
		{name: "oversized_header", headers: map[string]string{"x-api-key": strings.Repeat("a", service.MaxAccessTokenCredentialBytes+1)}},
		{name: "oversized_authorization_header", headers: map[string]string{"Authorization": "Bearer " + strings.Repeat("a", maxUserAccessTokenAuthorizationHeaderBytes+1)}},
		{name: "query_key", query: "key=" + validAccessTokenFixture},
		{name: "query_api_key", query: "api_key=" + validAccessTokenFixture},
		{name: "query_access_token", query: "access_token=" + validAccessTokenFixture},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAccessTokenAuthenticator{}
			r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, accessTokenRequest(tc.headers, tc.query))
			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.Zero(t, fake.calls.Load(), "must not reach Authenticate")
		})
	}
}

func TestUserAccessTokenAuthAbuseGateReturns429(t *testing.T) {
	cfg := invalidAuthAbuseTestConfig(2)
	apiKeyService := newAccessTokenTestAPIKeyService(cfg)
	fake := &fakeAccessTokenAuthenticator{}
	r := newAccessTokenAuthRouter(t, fake, apiKeyService)

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, accessTokenRequest(nil, ""))
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, accessTokenRequest(nil, ""))
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	require.Contains(t, w.Body.String(), "INVALID_AUTH_RATE_LIMITED")
}

func TestUserAccessTokenAuthSuccessSetsContextAndSkipsTouch(t *testing.T) {
	user := &service.User{ID: 42, Role: service.RoleUser, Status: service.StatusActive}
	fake := &fakeAccessTokenAuthenticator{authenticate: func(_ context.Context, token string) (*service.User, error) {
		require.Equal(t, validAccessTokenFixture, token)
		return user, nil
	}}

	var gotSubject AuthSubject
	var gotRole string
	var gotToken string
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.HandlerFunc(UserAccessTokenAuthMiddleware(userAccessTokenAuth(fake, newAccessTokenTestAPIKeyService(nil)))))
	r.GET("/t", func(c *gin.Context) {
		gotSubject, _ = GetAuthSubjectFromContext(c)
		gotRole, _ = GetUserRoleFromContext(c)
		gotToken, _ = GetUserAccessTokenPlaintextFromContext(c)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, accessTokenRequest(map[string]string{"Authorization": "Bearer " + validAccessTokenFixture}, ""))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(42), gotSubject.UserID)
	require.Equal(t, service.RoleUser, gotRole)
	require.Equal(t, validAccessTokenFixture, gotToken)
	require.Equal(t, int32(1), fake.calls.Load())
	// fakeAccessTokenAuthenticator 没有 TouchLastUsed 方法：中间件在编译期就
	// 不可能触发它；这里的调用计数为 1（仅 Authenticate）本身就是证据。
}

func TestUserAccessTokenAuthAcceptsBearerAndXAPIKeyNotGoogHeader(t *testing.T) {
	user := &service.User{ID: 7, Role: service.RoleUser, Status: service.StatusActive}
	authOK := func(_ context.Context, token string) (*service.User, error) {
		if token == validAccessTokenFixture {
			return user, nil
		}
		return nil, service.ErrInvalidAccessToken
	}

	t.Run("authorization_bearer", func(t *testing.T) {
		fake := &fakeAccessTokenAuthenticator{authenticate: authOK}
		r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, accessTokenRequest(map[string]string{"Authorization": "Bearer " + validAccessTokenFixture}, ""))
		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("x_api_key", func(t *testing.T) {
		fake := &fakeAccessTokenAuthenticator{authenticate: authOK}
		r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, accessTokenRequest(map[string]string{"x-api-key": validAccessTokenFixture}, ""))
		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("x_goog_api_key_not_supported", func(t *testing.T) {
		fake := &fakeAccessTokenAuthenticator{authenticate: authOK}
		r := newAccessTokenAuthRouter(t, fake, newAccessTokenTestAPIKeyService(nil))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, accessTokenRequest(map[string]string{"x-goog-api-key": validAccessTokenFixture}, ""))
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Zero(t, fake.calls.Load())
	})
}
