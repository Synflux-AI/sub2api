//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeUserAccessTokenSelfService 记录调用并按脚本返回，用于把 handler 的契约
// （状态码 / reason / Cache-Control）与 service 实现解耦。
type fakeUserAccessTokenSelfService struct {
	record *service.UserAccessToken
	getErr error

	rotateRecord *service.UserAccessToken
	rotateErr    error
	revokeErr    error

	rotateCalls    int
	revokeCalls    int
	lastPassword   string
	lastUserIDSeen int64
}

func (f *fakeUserAccessTokenSelfService) Get(_ context.Context, userID int64) (*service.UserAccessToken, error) {
	f.lastUserIDSeen = userID
	return f.record, f.getErr
}

func (f *fakeUserAccessTokenSelfService) Rotate(_ context.Context, userID int64, password string) (*service.UserAccessToken, error) {
	f.rotateCalls++
	f.lastUserIDSeen = userID
	f.lastPassword = password
	return f.rotateRecord, f.rotateErr
}

func (f *fakeUserAccessTokenSelfService) Revoke(_ context.Context, userID int64, password string) error {
	f.revokeCalls++
	f.lastUserIDSeen = userID
	f.lastPassword = password
	return f.revokeErr
}

// newUserAccessTokenTestRouter 挂一个只写 AuthSubject 的假认证，专注 handler 契约。
func newUserAccessTokenTestRouter(h *UserAccessTokenHandler, userID int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID})
		c.Set(string(middleware2.ContextKeyUserRole), service.RoleUser)
		c.Next()
	})
	router.GET("/api/v1/user/access-token", h.Get)
	router.POST("/api/v1/user/access-token/rotate", h.Rotate)
	router.DELETE("/api/v1/user/access-token", h.Revoke)
	return router
}

func doAccessTokenRequest(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// decodePanelEnvelope 解出面板信封的 code / reason / data。
func decodePanelEnvelope(t *testing.T, body string) (envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Reason  string          `json:"reason"`
	Data    json.RawMessage `json:"data"`
}) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	return envelope
}

func TestUserAccessTokenGetReturnsNullTokenWhenAbsentAndNeverCaches(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{record: nil}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodGet, "/api/v1/user/access-token", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	// §15.5：任何可能携带明文令牌的响应都必须 no-store，包含「当前无令牌」这一支
	// ——否则同一个 URL 的后续 200（已生成令牌）可能命中中间缓存。
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	envelope := decodePanelEnvelope(t, recorder.Body.String())
	require.Equal(t, 0, envelope.Code)
	require.JSONEq(t, `{"token":null,"created_at":null,"last_used_at":null}`, string(envelope.Data))
	require.EqualValues(t, 42, fake.lastUserIDSeen)
}

func TestUserAccessTokenGetReturnsPlaintextWithNoStore(t *testing.T) {
	created := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	used := time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC)
	fake := &fakeUserAccessTokenSelfService{record: &service.UserAccessToken{
		UserID:     42,
		Token:      "sat-" + strings.Repeat("a", 64),
		CreatedAt:  created,
		LastUsedAt: &used,
	}}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodGet, "/api/v1/user/access-token", "")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))

	envelope := decodePanelEnvelope(t, recorder.Body.String())
	require.JSONEq(t, `{
		"token": "sat-`+strings.Repeat("a", 64)+`",
		"created_at": "2026-07-01T02:03:04Z",
		"last_used_at": "2026-07-02T03:04:05Z"
	}`, string(envelope.Data))
}

func TestUserAccessTokenRotateReturnsPlaintextWithNoStore(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{rotateRecord: &service.UserAccessToken{
		UserID:    42,
		Token:     "sat-" + strings.Repeat("b", 64),
		CreatedAt: time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
	}}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodPost, "/api/v1/user/access-token/rotate", `{"password":"correct-horse"}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Equal(t, "correct-horse", fake.lastPassword)

	envelope := decodePanelEnvelope(t, recorder.Body.String())
	require.JSONEq(t, `{
		"token": "sat-`+strings.Repeat("b", 64)+`",
		"created_at": "2026-07-03T00:00:00Z",
		"last_used_at": null
	}`, string(envelope.Data))
}

// 密码错误必须严格 403 + ACCESS_TOKEN_PASSWORD_INCORRECT：仓内既有的
// ErrPasswordIncorrect 映射为 400，会让客户端把「密码错」当成「请求格式错」。
func TestUserAccessTokenRotateWrongPasswordIsForbidden(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{rotateErr: service.ErrAccessTokenPasswordIncorrect}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodPost, "/api/v1/user/access-token/rotate", `{"password":"wrong"}`)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Empty(t, recorder.Header().Get("Cache-Control"))

	envelope := decodePanelEnvelope(t, recorder.Body.String())
	require.Equal(t, http.StatusForbidden, envelope.Code)
	require.Equal(t, "ACCESS_TOKEN_PASSWORD_INCORRECT", envelope.Reason)
}

func TestUserAccessTokenRotateMissingPasswordIsBadRequest(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{rotateErr: service.ErrPasswordRequired}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	// 空 body：绑定错误被吞掉，空密码交给 service 判定（照 bindPasskeyPassword）。
	recorder := doAccessTokenRequest(router, http.MethodPost, "/api/v1/user/access-token/rotate", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, 1, fake.rotateCalls, "缺密码也要走到 service，密码门只有一处形状来源")
	require.Empty(t, fake.lastPassword)

	envelope := decodePanelEnvelope(t, recorder.Body.String())
	require.Equal(t, http.StatusBadRequest, envelope.Code)
	require.Equal(t, "PASSWORD_REQUIRED", envelope.Reason)
}

func TestUserAccessTokenRevokeAcceptsPasswordInDeleteBody(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodDelete, "/api/v1/user/access-token", `{"password":"correct-horse"}`)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 1, fake.revokeCalls)
	require.Equal(t, "correct-horse", fake.lastPassword)
	// 撤销响应不含明文令牌，因此不设 no-store。
	require.Empty(t, recorder.Header().Get("Cache-Control"))
}

func TestUserAccessTokenRevokeWrongPasswordIsForbidden(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{revokeErr: service.ErrAccessTokenPasswordIncorrect}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodDelete, "/api/v1/user/access-token", `{"password":"wrong"}`)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Equal(t, "ACCESS_TOKEN_PASSWORD_INCORRECT", decodePanelEnvelope(t, recorder.Body.String()).Reason)
}

func TestUserAccessTokenRevokeMissingTokenIsNotFound(t *testing.T) {
	fake := &fakeUserAccessTokenSelfService{revokeErr: service.ErrAccessTokenNotFound}
	router := newUserAccessTokenTestRouter(&UserAccessTokenHandler{tokens: fake}, 42)

	recorder := doAccessTokenRequest(router, http.MethodDelete, "/api/v1/user/access-token", `{"password":"correct-horse"}`)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Equal(t, "ACCESS_TOKEN_NOT_FOUND", decodePanelEnvelope(t, recorder.Body.String()).Reason)
}

// nil service 不该 panic：NewUserAccessTokenHandler 显式判空后才装进接口。
func TestNewUserAccessTokenHandlerWithNilServiceFailsClosed(t *testing.T) {
	router := newUserAccessTokenTestRouter(NewUserAccessTokenHandler(nil), 42)
	recorder := doAccessTokenRequest(router, http.MethodGet, "/api/v1/user/access-token", "")
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
}
