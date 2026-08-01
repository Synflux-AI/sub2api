//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// openBalanceRateLimitSettingRepo 只实现 GetValue：面板限流配置的读取路径只用它，
// 其余方法由嵌入的接口占位（被调用会 panic，正好暴露意外的依赖）。
type openBalanceRateLimitSettingRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *openBalanceRateLimitSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

// 被 Open API RPM 拒绝的请求**既不读余额、也不 touch last_used_at**。
//
// 这里用的是真实的 middleware.PanelRateLimiter（miniredis 后端）并按
// routes/open.go 的顺序挂载「认证 → 限流 → handler」，因此本用例同时钉住两件事：
//   - 429 由限流中间件在 handler 之前中断，假 service 的调用计数一次都不增加；
//   - 限流器确实看到了认证写入的 AuthSubject（否则会走「无认证主体则放行」的
//     防御分支，第二个请求不会是 429）。
func TestOpenBalanceRateLimitedRequestSkipsReadAndTouch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	settingRepo := &openBalanceRateLimitSettingRepo{values: map[string]string{
		"panel_rate_limit_settings": `{"enabled":true,"user_rpm":0,"heavy_rpm":0,` +
			`"exempt_admin":false,"public_ip_rpm":0,"open_api_rpm":1}`,
	}}
	settingService := service.NewSettingService(settingRepo, &config.Config{})
	panelRateLimiter := middleware2.NewPanelRateLimiter(redisClient, settingService)

	cache := &fakeOpenBalanceCache{balance: 3.5}
	users := &fakeOpenBalanceUserReader{user: &service.User{ID: 7, FrozenBalance: 1.25}}
	toucher := &fakeOpenBalanceToucher{}
	balanceHandler := &OpenBalanceHandler{billingCache: cache, users: users, accessTokens: toucher}

	router := gin.New()
	open := router.Group("/api/v1/open")
	open.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		c.Set(string(middleware2.ContextKeyUserRole), service.RoleUser)
		setAccessTokenPlaintextForTest(t, c, openBalanceTestToken)
		c.Next()
	})
	open.Use(panelRateLimiter.OpenAPI())
	open.GET("/balance", balanceHandler.GetBalance)

	perform := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/open/balance", nil))
		return recorder
	}

	first := perform()
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, 1, cache.calls)
	require.Equal(t, 1, users.calls)
	require.Equal(t, 1, toucher.calls)

	second := perform()
	require.Equal(t, http.StatusTooManyRequests, second.Code)
	require.Contains(t, second.Body.String(), "RATE_LIMITED")
	require.NotEmpty(t, second.Header().Get("Retry-After"))
	// 计数一个都没涨：被限流的请求根本没进 handler。
	require.Equal(t, 1, cache.calls, "被限流的请求不该读余额")
	require.Equal(t, 1, users.calls, "被限流的请求不该读用户")
	require.Equal(t, 1, toucher.calls, "被限流的请求不该 touch last_used_at")
	// 429 响应不含余额，也不该带 no-store（那是成功响应的标记）。
	require.Empty(t, second.Header().Get("Cache-Control"))
}
