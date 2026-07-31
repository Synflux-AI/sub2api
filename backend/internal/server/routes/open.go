package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

// RegisterOpenAPIRoutes 注册面向客户的 Open API（用户级 access token 鉴权）。
//
// 三条刻意的取舍，改动前请先读完：
//
//   - 中间件顺序必须是「认证 → 限流」。PanelRateLimiter.OpenAPI() 读取认证中间件
//     写入 context 的 AuthSubject 来定位限流桶；顺序倒过来限流器拿不到主体，会走
//     「无认证主体则放行」的防御分支，按用户限流静默失效。
//   - **不挂** middleware.BackendModeUserGuard：后台模式下恰恰是管理员手工发令牌
//     给客户，客户的余额查询必须仍然可用。
//   - **不挂** jwtAuth：本组是第三方客户凭 sat- 令牌直接调用，与面板会话无关。
func RegisterOpenAPIRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	userAccessTokenAuth middleware.UserAccessTokenAuthMiddleware,
	panelRateLimiter *middleware.PanelRateLimiter,
) {
	open := v1.Group("/open")
	open.Use(gin.HandlerFunc(userAccessTokenAuth))
	open.Use(panelRateLimiter.OpenAPI())
	{
		open.GET("/balance", h.OpenBalance.GetBalance)
	}
}
