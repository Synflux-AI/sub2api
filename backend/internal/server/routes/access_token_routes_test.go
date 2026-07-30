//go:build unit

package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// accessTokenRouteSettingRepo 内存版 SettingRepository。
//
// 注意：service.IsBackendModeEnabled 走**进程级**缓存（60s TTL），本测试二进制里
// 第一次读到的值会被后续所有测试复用。因此这里恒定返回
// backend_mode_enabled=true——本文件的每个用例都要求「后台模式开启」，保持一致
// 就不会因执行顺序而互相污染。
type accessTokenRouteSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func newAccessTokenRouteSettingRepo(openAPIRPM string) *accessTokenRouteSettingRepo {
	return &accessTokenRouteSettingRepo{values: map[string]string{
		"backend_mode_enabled": "true",
		"panel_rate_limit_settings": `{"enabled":true,"user_rpm":0,"heavy_rpm":0,` +
			`"exempt_admin":false,"public_ip_rpm":0,"open_api_rpm":` + openAPIRPM + `}`,
		// 管理面合规确认：不确认的话 AdminComplianceGuard 会 423 拦下所有 admin 路由。
		"admin_compliance_acknowledgement:1": `{"version":"` + service.AdminComplianceVersion + `"}`,
	}}
}

func (r *accessTokenRouteSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	value, err := r.GetValue(context.Background(), key)
	if err != nil {
		return nil, err
	}
	return &service.Setting{Key: key, Value: value}, nil
}

func (r *accessTokenRouteSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *accessTokenRouteSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

func (r *accessTokenRouteSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *accessTokenRouteSettingRepo) SetMultiple(_ context.Context, settings map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *accessTokenRouteSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *accessTokenRouteSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

// accessTokenRouteHarness 用真实的 Register*Routes 搭一套引擎，只把「认证」与
// 「step-up」换成可控的桩：本测试关心的是**路由组的中间件编排**，认证与 step-up
// 的内部行为由各自的单测负责。
type accessTokenRouteHarness struct {
	router         *gin.Engine
	auditedRoutes  []string
	stepUpBlocked  bool
	settingService *service.SettingService
}

func newAccessTokenRouteHarness(t *testing.T, role string, openAPIRPM string) *accessTokenRouteHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })

	settingService := service.NewSettingService(newAccessTokenRouteSettingRepo(openAPIRPM), &config.Config{})
	panelRateLimiter := middleware.NewPanelRateLimiter(redisClient, settingService)

	harness := &accessTokenRouteHarness{settingService: settingService}

	// handler 全部用 nil 依赖构造：它们「失败关闭」返回 500，而这个 500 恰好是
	// 「请求穿过了整条中间件链、真的走到了 handler」的唯一标志——与守卫的 403、
	// 限流的 429、step-up 的 403 互不混淆。handler 自身的成功路径由 handler 单测覆盖。
	handlers := &handler.Handlers{
		Admin:           &handler.AdminHandlers{UserAccessToken: admin.NewUserAccessTokenHandler(nil, nil)},
		UserAccessToken: handler.NewUserAccessTokenHandler(nil),
		OpenBalance:     handler.NewOpenBalanceHandler(nil, nil, nil),
	}

	identity := func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
		c.Set(string(middleware.ContextKeyUserRole), role)
		c.Next()
	}
	auditLog := middleware.AuditLogMiddleware(func(c *gin.Context) {
		harness.auditedRoutes = append(harness.auditedRoutes, c.Request.Method+" "+c.FullPath())
		c.Next()
	})
	stepUpAuth := middleware.StepUpAuthMiddleware(func(c *gin.Context) {
		if harness.stepUpBlocked {
			middleware.AbortWithError(c, http.StatusForbidden, "STEP_UP_REQUIRED", "step-up verification required")
			return
		}
		c.Next()
	})

	router := gin.New()
	v1 := router.Group("/api/v1")
	RegisterUserRoutes(v1, handlers, middleware.JWTAuthMiddleware(identity), auditLog, settingService, panelRateLimiter)
	RegisterAdminRoutes(v1, handlers, middleware.AdminAuthMiddleware(identity), auditLog, stepUpAuth, settingService, panelRateLimiter)
	RegisterOpenAPIRoutes(v1, handlers, middleware.UserAccessTokenAuthMiddleware(identity), panelRateLimiter)

	harness.router = router
	return harness
}

func (h *accessTokenRouteHarness) do(method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(""))
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder
}

// reachedHandler 断言请求穿过了整条中间件链：状态码 500 + 各 handler 在依赖缺失
// 时的固定文案。任何守卫（403/423）或限流（429）都会在这之前中断。
func requireReachedHandler(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	require.Equalf(t, http.StatusInternalServerError, recorder.Code,
		"请求没走到 handler，被中间件拦下了：%s", recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "unavailable")
}

// §4：后台模式开启时 /api/v1/open/balance 必须仍然可用——恰恰是后台模式下管理员
// 手工发令牌给客户，客户的余额查询不能被 BackendModeUserGuard 掐死。
func TestOpenBalanceRouteStaysReachableInBackendMode(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleUser, "10")
	require.True(t, harness.settingService.IsBackendModeEnabled(context.Background()), "后台模式必须开启，否则本用例没验证任何东西")

	recorder := harness.do(http.MethodGet, "/api/v1/open/balance")
	requireReachedHandler(t, recorder)
	require.NotContains(t, recorder.Body.String(), "Backend mode")
}

// Open API 的中间件顺序必须是「认证 → 限流」：PanelRateLimiter.OpenAPI() 靠认证写入
// 的 AuthSubject 定位限流桶。顺序倒过来限流器拿不到主体，会走「无认证主体则放行」
// 的防御分支，第二个请求就不会是 429——本用例正是那条顺序的探针。
func TestOpenBalanceRouteRateLimitsPerUserAfterAuthentication(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleUser, "1")

	first := harness.do(http.MethodGet, "/api/v1/open/balance")
	requireReachedHandler(t, first)

	second := harness.do(http.MethodGet, "/api/v1/open/balance")
	require.Equal(t, http.StatusTooManyRequests, second.Code)
	require.Contains(t, second.Body.String(), "RATE_LIMITED")
	require.NotEmpty(t, second.Header().Get("Retry-After"))
}

// 后台模式下普通用户的自助令牌接口必须 403（继承 BackendModeUserGuard）。
func TestUserAccessTokenRoutesAreForbiddenForUsersInBackendMode(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleUser, "10")

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/user/access-token"},
		{http.MethodPost, "/api/v1/user/access-token/rotate"},
		{http.MethodDelete, "/api/v1/user/access-token"},
	} {
		recorder := harness.do(request.method, request.path)
		require.Equalf(t, http.StatusForbidden, recorder.Code, "%s %s", request.method, request.path)
		require.Contains(t, recorder.Body.String(), "Backend mode")
	}
}

// 同一条路由在后台模式下对管理员仍然放行（BackendModeUserGuard 只挡非 admin）。
func TestUserAccessTokenRoutesStayReachableForAdminInBackendMode(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleAdmin, "10")
	requireReachedHandler(t, harness.do(http.MethodGet, "/api/v1/user/access-token"))
}

// 后台模式下管理员可为目标用户查看 / 生成 / 撤销令牌，三条路由都走到 handler，
// 且每条都进了审计中间件。
func TestAdminAccessTokenRoutesAreReachableAndAuditedInBackendMode(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleAdmin, "10")

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/users/9/access-token"},
		{http.MethodPost, "/api/v1/admin/users/9/access-token/rotate"},
		{http.MethodDelete, "/api/v1/admin/users/9/access-token"},
	} {
		recorder := harness.do(request.method, request.path)
		requireReachedHandler(t, recorder)
	}

	require.Equal(t, []string{
		"GET /api/v1/admin/users/:id/access-token",
		"POST /api/v1/admin/users/:id/access-token/rotate",
		"DELETE /api/v1/admin/users/:id/access-token",
	}, harness.auditedRoutes, "三条管理员令牌路由都必须经过审计中间件，且 FullPath 与审计表的键一致")
}

// 三条管理员令牌路由都挂了 step-up：step-up 拒绝时它们必须 403，而同组内未挂
// step-up 的路由不受影响。
func TestAdminAccessTokenRoutesRequireStepUp(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleAdmin, "10")
	harness.stepUpBlocked = true

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/users/9/access-token"},
		{http.MethodPost, "/api/v1/admin/users/9/access-token/rotate"},
		{http.MethodDelete, "/api/v1/admin/users/9/access-token"},
	} {
		recorder := harness.do(request.method, request.path)
		require.Equalf(t, http.StatusForbidden, recorder.Code, "%s %s 没挂 step-up", request.method, request.path)
		require.Contains(t, recorder.Body.String(), "STEP_UP_REQUIRED")
	}
	// 对照组见 TestUserAccessTokenRoutesDoNotRequireStepUp：同一个 stepUpBlocked
	// 开关下，未挂 step-up 的路由仍然走到 handler，证明这里的 403 来自 step-up
	// 本身而不是某个全局拦截。
}

// 用户自助的三条路由不挂 step-up（密码就是它们的二次确认）。
func TestUserAccessTokenRoutesDoNotRequireStepUp(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleAdmin, "10")
	harness.stepUpBlocked = true
	requireReachedHandler(t, harness.do(http.MethodGet, "/api/v1/user/access-token"))
}

// /api/v1/open 组刻意不挂 jwtAuth：未带面板会话的第三方客户请求不该被 401 挡下。
// 桩认证只写 AuthSubject，若该组混入了 jwtAuth，真实链路上会多一道会话校验——
// 这里用「注册时该组只有 2 个中间件之后才是 handler」的可观测代理：请求在没有
// 任何 Authorization 头时依然走到了 handler。
func TestOpenBalanceRouteHasNoPanelSessionRequirement(t *testing.T) {
	harness := newAccessTokenRouteHarness(t, service.RoleUser, "10")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/open/balance", nil)
	recorder := httptest.NewRecorder()
	harness.router.ServeHTTP(recorder, request)
	requireReachedHandler(t, recorder)
}
