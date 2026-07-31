//go:build unit

package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// accessTokenAuditCanary 是明文令牌的哨兵值：审计记录里出现它即为泄露。
const accessTokenAuditCanary = "sat-" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// accessTokenAuditPasswordCanary 是账户密码哨兵值。
const accessTokenAuditPasswordCanary = "audit-canary-account-password"

// newAccessTokenAuditRouter 用真实审计中间件包起四条令牌路由，handler 刻意在
// **响应体**里回写明文令牌，用来验证响应从不入库。
func newAccessTokenAuditRouter(t *testing.T) (*gin.Engine, *auditCaptureRepository, *service.AuditLogService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repository := &auditCaptureRepository{}
	auditService := service.NewAuditLogService(repository, nil)
	auditService.Start()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 77})
		c.Set(string(ContextKeyUserRole), service.RoleAdmin)
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))

	respondWithToken := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{"token": accessTokenAuditCanary}})
	}
	router.GET("/api/v1/user/access-token", respondWithToken)
	router.POST("/api/v1/user/access-token/rotate", respondWithToken)
	router.DELETE("/api/v1/user/access-token", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"code": 0})
	})
	router.GET("/api/v1/admin/users/:id/access-token", respondWithToken)
	router.POST("/api/v1/admin/users/:id/access-token/rotate", respondWithToken)
	router.DELETE("/api/v1/admin/users/:id/access-token", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"code": 0})
	})
	return router, repository, auditService
}

func accessTokenAuditLogs(t *testing.T, repository *auditCaptureRepository) map[string]*service.AuditLog {
	t.Helper()
	repository.mu.Lock()
	defer repository.mu.Unlock()
	byAction := make(map[string]*service.AuditLog, len(repository.logs))
	for _, entry := range repository.logs {
		byAction[entry.Action] = entry
	}
	return byAction
}

// requireNoCanary 把整条记录序列化后做整体扫描：任何字段（RequestBody / Extra /
// CredentialMasked / …）泄露哨兵都会被抓住，不依赖逐字段枚举。
func requireNoCanary(t *testing.T, entry *service.AuditLog, canaries ...string) {
	t.Helper()
	raw, err := json.Marshal(entry)
	require.NoError(t, err)
	for _, canary := range canaries {
		require.NotContainsf(t, string(raw), canary, "审计记录泄露了 %q", canary)
	}
}

// 两条敏感 GET 必须在白名单里，键是精确的 "<METHOD> <gin FullPath()>"
// （含 /api/v1 前缀与 :id 占位符）——写错一个字符这条读取就静默不入审计。
func TestAccessTokenSensitiveReadRoutesAreRegistered(t *testing.T) {
	require.Equal(t, "user.access_token.read", auditSensitiveReads["GET /api/v1/user/access-token"])
	require.Equal(t, "admin.users.access_token.read", auditSensitiveReads["GET /api/v1/admin/users/:id/access-token"])
	// 明文令牌只出现在响应体，而审计只捕获请求体，所以这两条读取不需要进
	// auditBodyOmittedRoutes（那张表是给「请求体整体是凭证」的路由用的）。
	require.NotContains(t, auditBodyOmittedRoutes, "GET /api/v1/user/access-token")
	require.NotContains(t, auditBodyOmittedRoutes, "GET /api/v1/admin/users/:id/access-token")
}

// 轮换 / 撤销是非 GET，已被无条件审计；这里钉住它们的 action 名稳定可读。
func TestAccessTokenMutationAuditActionsAreStable(t *testing.T) {
	expected := map[string]string{
		"POST /api/v1/user/access-token/rotate":            "user.access_token.rotate",
		"DELETE /api/v1/user/access-token":                 "user.access_token.revoke",
		"POST /api/v1/admin/users/:id/access-token/rotate": "admin.users.access_token.rotate",
		"DELETE /api/v1/admin/users/:id/access-token":      "admin.users.access_token.revoke",
	}
	for route, action := range expected {
		require.Equalf(t, action, auditActionOverrides[route], "%s 的审计动作名漂移了", route)
	}
}

// 两条敏感 GET 确实产生审计记录，且记录里没有明文令牌——因为审计只捕获请求体，
// 响应体（唯一含明文的地方）从不入库。这条性质无需新增脱敏代码，只需钉住。
func TestAccessTokenSensitiveReadsAreAuditedWithoutPlaintext(t *testing.T) {
	router, repository, auditService := newAccessTokenAuditRouter(t)

	for _, target := range []string{"/api/v1/user/access-token", "/api/v1/admin/users/9/access-token"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+accessTokenAuditCanary)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code)
		// handler 确实把明文写进了响应体——否则这个测试是空转。
		require.Contains(t, recorder.Body.String(), accessTokenAuditCanary)
	}
	auditService.Stop()

	byAction := accessTokenAuditLogs(t, repository)
	for _, action := range []string{"user.access_token.read", "admin.users.access_token.read"} {
		entry := byAction[action]
		require.NotNilf(t, entry, "%s 没有产生审计记录", action)
		require.Equal(t, http.StatusOK, entry.StatusCode)
		require.Empty(t, entry.RequestBody, "GET 不该捕获请求体")
		requireNoCanary(t, entry, accessTokenAuditCanary)
		// 请求头里的令牌只以首尾掩码形式留存。
		require.NotEmpty(t, entry.CredentialMasked)
		require.Contains(t, entry.CredentialMasked, "Bearer ")
	}
	require.Equal(t, "9", byAction["admin.users.access_token.read"].Extra["params"].(map[string]string)["id"])
}

// 轮换的请求体含账户密码：既有的子串脱敏清单（auditBodySensitiveSubstrings 含
// "password"）已覆盖，本测试只负责钉住，不新增脱敏代码。
func TestAccessTokenRotateAuditRedactsPassword(t *testing.T) {
	router, repository, auditService := newAccessTokenAuditRouter(t)

	body := `{"password":"` + accessTokenAuditPasswordCanary + `"}`
	for _, target := range []string{
		"/api/v1/user/access-token/rotate",
		"/api/v1/admin/users/9/access-token/rotate",
	} {
		request := httptest.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code)
	}
	auditService.Stop()

	byAction := accessTokenAuditLogs(t, repository)
	for _, action := range []string{"user.access_token.rotate", "admin.users.access_token.rotate"} {
		entry := byAction[action]
		require.NotNilf(t, entry, "%s 没有产生审计记录", action)
		// 键级脱敏保留结构、擦掉值，便于追责又不留凭证。
		require.Contains(t, entry.RequestBody, `"password"`)
		require.Contains(t, entry.RequestBody, `"***"`)
		requireNoCanary(t, entry, accessTokenAuditPasswordCanary, accessTokenAuditCanary)
	}
}

// 撤销同样带 {password} body（本仓既有姿态：DELETE 带 body），脱敏一致。
func TestAccessTokenRevokeAuditRedactsPassword(t *testing.T) {
	router, repository, auditService := newAccessTokenAuditRouter(t)

	body := `{"password":"` + accessTokenAuditPasswordCanary + `"}`
	for _, target := range []string{"/api/v1/user/access-token", "/api/v1/admin/users/9/access-token"} {
		request := httptest.NewRequest(http.MethodDelete, target, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code)
	}
	auditService.Stop()

	byAction := accessTokenAuditLogs(t, repository)
	for _, action := range []string{"user.access_token.revoke", "admin.users.access_token.revoke"} {
		entry := byAction[action]
		require.NotNilf(t, entry, "%s 没有产生审计记录", action)
		require.Contains(t, entry.RequestBody, `"***"`)
		requireNoCanary(t, entry, accessTokenAuditPasswordCanary)
	}
}

// 脱敏清单本身的形状：password 走子串匹配，token 亦然——两者都不该被移出清单。
func TestAccessTokenAuditRedactionListsCoverPasswordAndToken(t *testing.T) {
	redacted := service.RedactAuditBody([]byte(`{"password":"p","access_token":"t","note":"keep-me"}`), "application/json")
	require.NotContains(t, redacted, `"p"`)
	require.NotContains(t, redacted, `"t"`)
	require.Contains(t, redacted, "keep-me", "非敏感字段应保留，便于追责")
	require.Equal(t, 2, strings.Count(redacted, `"***"`))
}
