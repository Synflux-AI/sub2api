//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeAdminAccessTokenService struct {
	record       *service.UserAccessToken
	getErr       error
	rotateRecord *service.UserAccessToken
	rotateErr    error
	revokeErr    error

	getCalls    int
	rotateCalls int
	revokeCalls int
}

func (f *fakeAdminAccessTokenService) Get(_ context.Context, _ int64) (*service.UserAccessToken, error) {
	f.getCalls++
	return f.record, f.getErr
}

func (f *fakeAdminAccessTokenService) RotateForAdmin(_ context.Context, _ int64) (*service.UserAccessToken, error) {
	f.rotateCalls++
	return f.rotateRecord, f.rotateErr
}

func (f *fakeAdminAccessTokenService) RevokeForAdmin(_ context.Context, _ int64) error {
	f.revokeCalls++
	return f.revokeErr
}

type fakeAdminAccessTokenUserReader struct {
	users map[int64]*service.User
	// wrap 复现 UserService.GetByID 的包装形状：fmt.Errorf("get user: %w", err)。
	wrap bool
}

func (f *fakeAdminAccessTokenUserReader) GetByID(_ context.Context, id int64) (*service.User, error) {
	if user, ok := f.users[id]; ok {
		return user, nil
	}
	if f.wrap {
		return nil, fmt.Errorf("get user: %w", service.ErrUserNotFound)
	}
	return nil, service.ErrUserNotFound
}

func newAdminAccessTokenRouter(h *UserAccessTokenHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/admin/users/:id/access-token", h.Get)
	router.POST("/api/v1/admin/users/:id/access-token/rotate", h.Rotate)
	router.DELETE("/api/v1/admin/users/:id/access-token", h.Revoke)
	return router
}

func doAdminAccessTokenRequest(router *gin.Engine, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func adminEnvelopeReason(t *testing.T, body string) (int, string) {
	t.Helper()
	var envelope struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	return envelope.Code, envelope.Reason
}

func newAdminAccessTokenHandler(tokens *fakeAdminAccessTokenService, users *fakeAdminAccessTokenUserReader) *UserAccessTokenHandler {
	return &UserAccessTokenHandler{tokens: tokens, users: users}
}

func TestAdminAccessTokenGetAndRotateSetNoStore(t *testing.T) {
	created := time.Date(2026, 7, 4, 5, 6, 7, 0, time.UTC)
	tokens := &fakeAdminAccessTokenService{
		record:       &service.UserAccessToken{UserID: 9, Token: "sat-" + strings.Repeat("c", 64), CreatedAt: created},
		rotateRecord: &service.UserAccessToken{UserID: 9, Token: "sat-" + strings.Repeat("d", 64), CreatedAt: created},
	}
	users := &fakeAdminAccessTokenUserReader{users: map[int64]*service.User{9: {ID: 9}}}
	router := newAdminAccessTokenRouter(newAdminAccessTokenHandler(tokens, users))

	get := doAdminAccessTokenRequest(router, http.MethodGet, "/api/v1/admin/users/9/access-token")
	require.Equal(t, http.StatusOK, get.Code)
	require.Equal(t, "no-store", get.Header().Get("Cache-Control"))
	require.Contains(t, get.Body.String(), "sat-"+strings.Repeat("c", 64))

	rotate := doAdminAccessTokenRequest(router, http.MethodPost, "/api/v1/admin/users/9/access-token/rotate")
	require.Equal(t, http.StatusOK, rotate.Code)
	require.Equal(t, "no-store", rotate.Header().Get("Cache-Control"))
	require.Contains(t, rotate.Body.String(), "sat-"+strings.Repeat("d", 64))
}

// 管理员路径**不校验目标用户密码**：不带任何 body 也能轮换/撤销成功。
func TestAdminAccessTokenRotateAndRevokeNeedNoTargetPassword(t *testing.T) {
	tokens := &fakeAdminAccessTokenService{rotateRecord: &service.UserAccessToken{UserID: 9, Token: "sat-" + strings.Repeat("e", 64)}}
	users := &fakeAdminAccessTokenUserReader{users: map[int64]*service.User{9: {ID: 9}}}
	router := newAdminAccessTokenRouter(newAdminAccessTokenHandler(tokens, users))

	require.Equal(t, http.StatusOK, doAdminAccessTokenRequest(router, http.MethodPost, "/api/v1/admin/users/9/access-token/rotate").Code)
	require.Equal(t, http.StatusOK, doAdminAccessTokenRequest(router, http.MethodDelete, "/api/v1/admin/users/9/access-token").Code)
	require.Equal(t, 1, tokens.rotateCalls)
	require.Equal(t, 1, tokens.revokeCalls)
}

// RotateForAdmin / RevokeForAdmin 都不读目标用户：不存在的 user_id 会撞
// user_access_tokens.user_id 外键约束变成 500。前置检查把它收敛成 404，
// 且**根本不调用** service，避免留下半成品写入。
func TestAdminAccessTokenMissingTargetUserIs404AndSkipsService(t *testing.T) {
	for _, wrap := range []bool{false, true} {
		tokens := &fakeAdminAccessTokenService{}
		users := &fakeAdminAccessTokenUserReader{users: map[int64]*service.User{9: {ID: 9}}, wrap: wrap}
		router := newAdminAccessTokenRouter(newAdminAccessTokenHandler(tokens, users))

		for _, request := range []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/v1/admin/users/404/access-token"},
			{http.MethodPost, "/api/v1/admin/users/404/access-token/rotate"},
			{http.MethodDelete, "/api/v1/admin/users/404/access-token"},
		} {
			recorder := doAdminAccessTokenRequest(router, request.method, request.path)
			require.Equalf(t, http.StatusNotFound, recorder.Code, "%s %s (wrapped=%v)", request.method, request.path, wrap)
		}
		require.Zero(t, tokens.getCalls)
		require.Zero(t, tokens.rotateCalls)
		require.Zero(t, tokens.revokeCalls)
	}
}

func TestAdminAccessTokenInvalidIDIsBadRequest(t *testing.T) {
	tokens := &fakeAdminAccessTokenService{}
	users := &fakeAdminAccessTokenUserReader{users: map[int64]*service.User{9: {ID: 9}}}
	router := newAdminAccessTokenRouter(newAdminAccessTokenHandler(tokens, users))

	for _, path := range []string{
		"/api/v1/admin/users/abc/access-token",
		"/api/v1/admin/users/0/access-token",
		"/api/v1/admin/users/-3/access-token",
	} {
		recorder := doAdminAccessTokenRequest(router, http.MethodGet, path)
		require.Equalf(t, http.StatusBadRequest, recorder.Code, "GET %s", path)
	}
	require.Zero(t, tokens.getCalls)
}

func TestAdminAccessTokenGetReturnsNullTokenWhenAbsent(t *testing.T) {
	tokens := &fakeAdminAccessTokenService{record: nil}
	users := &fakeAdminAccessTokenUserReader{users: map[int64]*service.User{9: {ID: 9}}}
	router := newAdminAccessTokenRouter(newAdminAccessTokenHandler(tokens, users))

	recorder := doAdminAccessTokenRequest(router, http.MethodGet, "/api/v1/admin/users/9/access-token")
	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	// 与用户自助侧字节级同形，前端可复用同一份解析。
	require.JSONEq(t, `{"token":null,"created_at":null,"last_used_at":null}`, string(envelope.Data))
}

func TestAdminAccessTokenRevokeMissingTokenIsNotFound(t *testing.T) {
	tokens := &fakeAdminAccessTokenService{revokeErr: service.ErrAccessTokenNotFound}
	users := &fakeAdminAccessTokenUserReader{users: map[int64]*service.User{9: {ID: 9}}}
	router := newAdminAccessTokenRouter(newAdminAccessTokenHandler(tokens, users))

	recorder := doAdminAccessTokenRequest(router, http.MethodDelete, "/api/v1/admin/users/9/access-token")
	code, reason := adminEnvelopeReason(t, recorder.Body.String())
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, "ACCESS_TOKEN_NOT_FOUND", reason)
}

func TestNewAdminUserAccessTokenHandlerWithNilServicesFailsClosed(t *testing.T) {
	router := newAdminAccessTokenRouter(NewUserAccessTokenHandler(nil, nil))
	require.Equal(t, http.StatusInternalServerError,
		doAdminAccessTokenRequest(router, http.MethodGet, "/api/v1/admin/users/9/access-token").Code)
}
