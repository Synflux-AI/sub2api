package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// UserAccessTokenAuthMiddleware 用户级 access token（sat-…）鉴权中间件类型。
type UserAccessTokenAuthMiddleware gin.HandlerFunc

// maxUserAccessTokenAuthorizationHeaderBytes 给 Authorization header 多留
// "Bearer " 前缀与空白的余量，值同 api_key_auth.go 的 maxAPIKeyAuthorizationHeaderBytes。
const maxUserAccessTokenAuthorizationHeaderBytes = service.MaxAccessTokenCredentialBytes + 128

// userAccessTokenPlaintextContextKey 是本功能私有的 gin context 键，刻意不进入
// middleware.go 里公共的 ContextKey 枚举：它只服务于 Task 5 的 handler——认证
// 成功后取回认证时用的令牌明文，与 user_id 做 (user_id, token) 双匹配去调用
// service.UserAccessTokenService.TouchLastUsed，防止令牌轮换后的旧请求把新
// 令牌的 last_used_at 误标记为已使用。
const userAccessTokenPlaintextContextKey = "user_access_token_plaintext"

// GetUserAccessTokenPlaintextFromContext 读取认证时使用的令牌明文；仅在
// UserAccessTokenAuthMiddleware 认证成功后存在。
func GetUserAccessTokenPlaintextFromContext(c *gin.Context) (string, bool) {
	value, exists := c.Get(userAccessTokenPlaintextContextKey)
	if !exists {
		return "", false
	}
	token, ok := value.(string)
	return token, ok
}

// accessTokenAuthenticator 是中间件对 *service.UserAccessTokenService 的窄依赖
// （仅 token → user）。保留成接口而不是直接引用具体类型，是为了让单测能在不
// 搭建整条 repo/UserService 依赖链的前提下，用一个记调用次数的假实现钉住「格式
// 校验必须先于查库」这条属性——*service.UserAccessTokenService 满足此接口。
type accessTokenAuthenticator interface {
	Authenticate(ctx context.Context, token string) (*service.User, error)
}

// NewUserAccessTokenAuthMiddleware 创建用户级 access token 鉴权中间件。
//
// 职责刻意做窄：只做「令牌 → 用户」，不碰分组、订阅、计费执行——这些概念不适用
// 于面向客户的 Open API（issue #110 §15）。中间件本身：
//   - 不做任何认证缓存（同 UserAccessTokenService 的设计取舍）；
//   - 不在这里更新 last_used_at、也绝不为此启动 goroutine（§15.2 明令禁止）：
//     那次 touch 延后到 Task 5 的 handler，在限流通过且余额读取成功之后，用本
//     中间件写入 context 的令牌明文调用 TouchLastUsed。
func NewUserAccessTokenAuthMiddleware(tokenService *service.UserAccessTokenService, apiKeyService *service.APIKeyService) UserAccessTokenAuthMiddleware {
	return UserAccessTokenAuthMiddleware(userAccessTokenAuth(tokenService, apiKeyService))
}

func userAccessTokenAuth(tokenAuth accessTokenAuthenticator, apiKeyService *service.APIKeyService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// ── 1. 反爆破前置门：复用既有 rejectInvalidAuthAbuse，不改它 ──────
		if rejectInvalidAuthAbuse(c, apiKeyService) {
			AbortWithError(c, http.StatusTooManyRequests, "INVALID_AUTH_RATE_LIMITED", "Too many invalid authentication attempts; retry later")
			return
		}

		// ── 2. header 长度门：查库之前挡掉超长输入 ────────────────────
		if accessTokenHeadersTooLarge(c) {
			recordInvalidAuthFailure(c, apiKeyService)
			abortInvalidAccessToken(c)
			return
		}

		// ── 3. 拒绝 query 凭据：key / api_key / access_token 一律不接受 ──
		if hasAccessTokenQueryCredential(c) {
			recordInvalidAuthFailure(c, apiKeyService)
			abortInvalidAccessToken(c)
			return
		}

		// ── 4. 取凭据：Authorization: Bearer → 回退 x-api-key ─────────
		// 刻意不支持 x-goog-api-key：api_key_auth.go 的第三回退是 Gemini CLI
		// 兼容，与本功能的用户级令牌无关，不在此处引入。
		token := extractAccessTokenCredential(c)

		// ── 5. 格式校验先于查库（§15.2 硬性要求）：空凭据、畸形前缀、非 hex、
		// 超长输入、乃至网关的 sk- API Key，全部在这里被挡下，绝不进数据库。
		if !service.IsValidAccessTokenFormat(token) {
			recordInvalidAuthFailure(c, apiKeyService)
			abortInvalidAccessToken(c)
			return
		}

		// ── 6. 查库鉴权：令牌不存在 / 用户不存在 / 用户被停用统一收敛成同一个
		// 错误，中间件（乃至 service）都无法区分具体原因，避免探测者靠响应
		// 差异推断「令牌存在但用户被停用」之类的信息。
		user, err := tokenAuth.Authenticate(c.Request.Context(), token)
		if err != nil || user == nil {
			recordInvalidAuthFailure(c, apiKeyService)
			abortInvalidAccessToken(c)
			return
		}

		// ── 7. 写 context：PanelRateLimiter.userScoped（Task 4）读取这两者 ──
		c.Set(string(ContextKeyUser), AuthSubject{UserID: user.ID})
		c.Set(string(ContextKeyUserRole), user.Role)
		// 认证时用的令牌明文单独存一份，供 handler 末尾做 (user_id, token) 双
		// 匹配 touch；不放进 AuthSubject 是因为那是跨中间件共享的通用鉴权身份，
		// 令牌明文只有本功能的 handler 需要。
		c.Set(userAccessTokenPlaintextContextKey, token)

		// 注意：这里不启动任何 goroutine 去 touch last_used_at（§15.2 明令禁
		// 止）。那次落库延后到 Task 5 的 handler，在限流与余额读取都成功之后
		// 才做，本中间件到这里的职责就此结束。
		c.Next()
	}
}

// abortInvalidAccessToken 是本中间件对外暴露的唯一失败响应：无凭据、格式非法、
// 令牌不存在、用户被停用这四种情况全部走这一个函数，产生字节级相同的响应体，
// 不给探测者留下可区分的信号。
func abortInvalidAccessToken(c *gin.Context) {
	AbortWithError(c, http.StatusUnauthorized, "INVALID_ACCESS_TOKEN", "invalid access token")
}

func accessTokenHeadersTooLarge(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return len(c.GetHeader("Authorization")) > maxUserAccessTokenAuthorizationHeaderBytes ||
		len(c.GetHeader("x-api-key")) > service.MaxAccessTokenCredentialBytes
}

func hasAccessTokenQueryCredential(c *gin.Context) bool {
	if c == nil {
		return false
	}
	for _, name := range []string{"key", "api_key", "access_token"} {
		if strings.TrimSpace(c.Query(name)) != "" {
			return true
		}
	}
	return false
}

func extractAccessTokenCredential(c *gin.Context) string {
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if token := strings.TrimSpace(parts[1]); token != "" {
				return token
			}
		}
	}
	// 没有 x-goog-api-key 回退：见函数上方（调用处）注释，是本功能有意的收敛。
	return strings.TrimSpace(c.GetHeader("x-api-key"))
}
