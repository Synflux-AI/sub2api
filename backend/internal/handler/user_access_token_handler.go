package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// accessTokenPasswordRequest 承载给「查看 / 轮换 / 撤销明文令牌」加门的账户密码。
// 绑定错误刻意吞掉（同 passkeyPasswordRequest）：缺失或畸形 body 会得到空密码，
// 由 service 统一判成 PASSWORD_REQUIRED(400)，密码门的形状只有一处来源。
type accessTokenPasswordRequest struct {
	Password string `json:"password"`
}

// bindAccessTokenPassword 与 bindPasskeyPassword 形状相同但刻意不复用：两个功能的
// 密码门要能独立演进（本功能后续可能加 TOTP 字段），共用一个请求结构会让其中一边
// 的字段变更悄悄影响另一边。
func bindAccessTokenPassword(c *gin.Context) string {
	var req accessTokenPasswordRequest
	_ = c.ShouldBindJSON(&req)
	return req.Password
}

// userAccessTokenSelfService 是本 handler 对 *service.UserAccessTokenService 的
// 窄依赖（仅自助路径的三个方法）。保留成接口便于单测注入返回精确契约错误的假
// 实现，而不必搭起整条 repo/UserService 依赖链。
type userAccessTokenSelfService interface {
	Get(ctx context.Context, userID int64) (*service.UserAccessToken, error)
	Rotate(ctx context.Context, userID int64, password string) (*service.UserAccessToken, error)
	Revoke(ctx context.Context, userID int64, password string) error
}

// UserAccessTokenHandler 提供用户自助的令牌查看 / 轮换 / 撤销（面板信封）。
type UserAccessTokenHandler struct {
	tokens userAccessTokenSelfService
}

// NewUserAccessTokenHandler 构造用户自助令牌 handler。
func NewUserAccessTokenHandler(accessTokenService *service.UserAccessTokenService) *UserAccessTokenHandler {
	h := &UserAccessTokenHandler{}
	// 显式判空：nil 具体指针装进接口后接口值非 nil，调用即 panic。
	if accessTokenService != nil {
		h.tokens = accessTokenService
	}
	return h
}

// Get 返回当前用户的令牌明文；未生成时 token 为 null。
// GET /api/v1/user/access-token
func (h *UserAccessTokenHandler) Get(c *gin.Context) {
	subject, ok := h.requireSubject(c)
	if !ok {
		return
	}
	record, err := h.tokens.Get(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 响应体含明文令牌：禁止任何中间缓存留存。
	c.Header("Cache-Control", "no-store")
	response.Success(c, dto.UserAccessTokenFromService(record))
}

// Rotate 校验账户密码后生成新令牌（首次生成与轮换共用此接口）。
// POST /api/v1/user/access-token/rotate
func (h *UserAccessTokenHandler) Rotate(c *gin.Context) {
	subject, ok := h.requireSubject(c)
	if !ok {
		return
	}
	record, err := h.tokens.Rotate(c.Request.Context(), subject.UserID, bindAccessTokenPassword(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, dto.UserAccessTokenFromService(record))
}

// Revoke 校验账户密码后撤销令牌，立即生效（服务层不做认证缓存）。
// DELETE /api/v1/user/access-token
func (h *UserAccessTokenHandler) Revoke(c *gin.Context) {
	subject, ok := h.requireSubject(c)
	if !ok {
		return
	}
	if err := h.tokens.Revoke(c.Request.Context(), subject.UserID, bindAccessTokenPassword(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 无明文令牌出现在响应里，不需要 no-store。
	response.Success(c, gin.H{"success": true})
}

func (h *UserAccessTokenHandler) requireSubject(c *gin.Context) (middleware2.AuthSubject, bool) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return middleware2.AuthSubject{}, false
	}
	if h.tokens == nil {
		response.InternalError(c, "Access token service is unavailable")
		return middleware2.AuthSubject{}, false
	}
	return subject, true
}
