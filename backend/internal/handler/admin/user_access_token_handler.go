package admin

import (
	"context"
	"errors"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// userAccessTokenAdminService 是本 handler 对 *service.UserAccessTokenService 的
// 窄依赖（只用管理员路径）。管理员路径**不校验目标用户密码**——管理员不知道对方
// 密码；调用方的权限与 step-up 2FA 由路由中间件负责。
type userAccessTokenAdminService interface {
	Get(ctx context.Context, userID int64) (*service.UserAccessToken, error)
	RotateForAdmin(ctx context.Context, targetUserID int64) (*service.UserAccessToken, error)
	RevokeForAdmin(ctx context.Context, targetUserID int64) error
}

// userAccessTokenTargetReader 用于「目标用户是否存在」的前置检查。
type userAccessTokenTargetReader interface {
	GetByID(ctx context.Context, id int64) (*service.User, error)
}

// UserAccessTokenHandler 管理员侧的用户令牌查看 / 轮换 / 撤销（面板信封）。
type UserAccessTokenHandler struct {
	tokens userAccessTokenAdminService
	users  userAccessTokenTargetReader
}

// NewUserAccessTokenHandler 构造管理员令牌 handler。
func NewUserAccessTokenHandler(
	accessTokenService *service.UserAccessTokenService,
	userService *service.UserService,
) *UserAccessTokenHandler {
	h := &UserAccessTokenHandler{}
	// 显式判空：nil 具体指针装进接口后接口值非 nil，调用即 panic。
	if accessTokenService != nil {
		h.tokens = accessTokenService
	}
	if userService != nil {
		h.users = userService
	}
	return h
}

// Get 返回目标用户的令牌明文；未生成时 token 为 null。
// GET /api/v1/admin/users/:id/access-token
func (h *UserAccessTokenHandler) Get(c *gin.Context) {
	userID, ok := h.resolveTarget(c)
	if !ok {
		return
	}
	record, err := h.tokens.Get(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 响应体含明文令牌：禁止任何中间缓存留存。
	c.Header("Cache-Control", "no-store")
	response.Success(c, dto.UserAccessTokenFromService(record))
}

// Rotate 为目标用户生成新令牌（首次生成与轮换共用此接口）。
// POST /api/v1/admin/users/:id/access-token/rotate
func (h *UserAccessTokenHandler) Rotate(c *gin.Context) {
	userID, ok := h.resolveTarget(c)
	if !ok {
		return
	}
	record, err := h.tokens.RotateForAdmin(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, dto.UserAccessTokenFromService(record))
}

// Revoke 撤销目标用户的令牌，立即生效。
// DELETE /api/v1/admin/users/:id/access-token
func (h *UserAccessTokenHandler) Revoke(c *gin.Context) {
	userID, ok := h.resolveTarget(c)
	if !ok {
		return
	}
	if err := h.tokens.RevokeForAdmin(c.Request.Context(), userID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 无明文令牌出现在响应里，不需要 no-store。
	response.Success(c, gin.H{"success": true})
}

// resolveTarget 解析 :id 并**前置确认目标用户存在**。
//
// 这道前置检查是必须的：RotateForAdmin / RevokeForAdmin 都不读目标用户，
// 不存在的 user_id 会在 Upsert 时撞上 user_access_tokens.user_id 的外键约束，
// 客户端看到的是 500 而不是语义正确的 404。三个方法统一走同一条前置检查，
// GET 也不例外——否则「查不存在的用户」会得到 200 + token:null，与
// 「查存在但未生成的用户」无法区分。
func (h *UserAccessTokenHandler) resolveTarget(c *gin.Context) (int64, bool) {
	userID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid user ID")
		return 0, false
	}
	if h.tokens == nil || h.users == nil {
		response.InternalError(c, "Access token service is unavailable")
		return 0, false
	}
	user, err := h.users.GetByID(c.Request.Context(), userID)
	if err != nil {
		if errors.Is(err, service.ErrUserNotFound) {
			response.NotFound(c, "User not found")
			return 0, false
		}
		response.ErrorFrom(c, err)
		return 0, false
	}
	if user == nil {
		response.NotFound(c, "User not found")
		return 0, false
	}
	return userID, true
}
