//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newErrorHandlingRuleTestHandler(t *testing.T) *SettingHandler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &panelRateLimitHandlerRepoStub{values: map[string]string{}}
	svc := service.NewSettingService(repo, &config.Config{})
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
}

// errorHandlingRuleFailingSetRepoStub 复用内存 stub 的读路径，只让写入失败，
// 用来模拟「配置合法但仓储写不进去」这类服务端故障。
type errorHandlingRuleFailingSetRepoStub struct {
	*panelRateLimitHandlerRepoStub
	setErr error
}

func (r *errorHandlingRuleFailingSetRepoStub) Set(_ context.Context, _, _ string) error {
	return r.setErr
}

func newErrorHandlingRuleFailingWriteHandler(t *testing.T, setErr error) *SettingHandler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &errorHandlingRuleFailingSetRepoStub{
		panelRateLimitHandlerRepoStub: &panelRateLimitHandlerRepoStub{values: map[string]string{}},
		setErr:                        setErr,
	}
	svc := service.NewSettingService(repo, &config.Config{})
	return NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
}

func doErrorHandlingRulePut(t *testing.T, h *SettingHandler, req UpdateErrorHandlingRuleSettingsRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/admin/settings/error-handling-rules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateErrorHandlingRuleSettings(c)
	return rec
}

func TestUpdateErrorHandlingRuleSettingsRejectsEmptyRule(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{{ID: "r1"}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdateErrorHandlingRuleSettingsRejectsRetryCountAboveCap(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	retry := 99
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{{ID: "r1", StatusCodes: []int{500}, Action: service.ErrorHandlingActionRetry, RetryCount: &retry}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdateErrorHandlingRuleSettingsRepoWriteFailureReturns500(t *testing.T) {
	setErr := errors.New("settings table is read-only: dsn=postgres://admin:secret@db")
	h := newErrorHandlingRuleFailingWriteHandler(t, setErr)
	retryCount := 2
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{
			{ID: "r1", Name: "限流重试", StatusCodes: []int{429}, Action: service.ErrorHandlingActionRetry, RetryCount: &retryCount, ExhaustedAction: service.ErrorHandlingExhaustedActionPassthrough},
		},
	})
	require.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, "repo write failure must not be reported as a client error")
	// 内部错误文本（含连接串）不得回显给管理端
	require.NotContains(t, rec.Body.String(), "read-only")
	require.NotContains(t, rec.Body.String(), "postgres://")
}

func TestUpdateErrorHandlingRuleSettingsValidationFailureKeepsMessage(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{{ID: "r1", StatusCodes: []int{429}, Action: "nope"}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.True(t, strings.Contains(rec.Body.String(), "unknown action"),
		"validation message must stay human-readable, got: %s", rec.Body.String())
}

func TestGetAndUpdateErrorHandlingRuleSettingsRoundTrip(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	retryCount := 2
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{
			{ID: "r1", Name: "限流重试", StatusCodes: []int{429}, Action: service.ErrorHandlingActionRetry, RetryCount: &retryCount, ExhaustedAction: service.ErrorHandlingExhaustedActionPassthrough},
			{ID: "r2", Name: "超长直接透传", Keywords: []string{"prompt is too long"}, Action: service.ErrorHandlingActionPassthrough},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var updateResp struct {
		Data dto.ErrorHandlingRuleSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updateResp))
	require.True(t, updateResp.Data.Enabled)
	require.Len(t, updateResp.Data.Rules, 2)
	require.Equal(t, "限流重试", updateResp.Data.Rules[0].Name)
	require.Equal(t, []int{429}, updateResp.Data.Rules[0].StatusCodes)
	require.NotNil(t, updateResp.Data.Rules[0].RetryCount)
	require.Equal(t, 2, *updateResp.Data.Rules[0].RetryCount)
	require.Equal(t, service.ErrorHandlingExhaustedActionPassthrough, updateResp.Data.Rules[0].ExhaustedAction)
	require.Equal(t, service.ErrorHandlingActionPassthrough, updateResp.Data.Rules[1].Action)
	require.Nil(t, updateResp.Data.Rules[1].RetryCount)
	require.Equal(t, service.ErrorHandlingExhaustedActionDefault, updateResp.Data.Rules[1].ExhaustedAction)

	getRec := httptest.NewRecorder()
	getC, _ := gin.CreateTestContext(getRec)
	getC.Request = httptest.NewRequest(http.MethodGet, "/admin/settings/error-handling-rules", nil)
	h.GetErrorHandlingRuleSettings(getC)
	require.Equal(t, http.StatusOK, getRec.Code)

	var getResp struct {
		Data dto.ErrorHandlingRuleSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	require.Equal(t, updateResp.Data, getResp.Data)
}

// enabled 是指针字段，往返里最容易被 DTO 层吃掉：显式 false 必须原样回来，
// 而存量规则（不带该字段）必须保持 nil，服务层才会按启用处理。
func TestErrorHandlingRuleSettingsRoundTripsEnabledFlag(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	disabled := false
	enabled := true
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{
			{ID: "r1", Enabled: &disabled, StatusCodes: []int{429}, Action: service.ErrorHandlingActionRetry},
			{ID: "r2", Enabled: &enabled, StatusCodes: []int{500}, Action: service.ErrorHandlingActionFailover},
			{ID: "r3", StatusCodes: []int{502}, Action: service.ErrorHandlingActionFailover},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	getRec := httptest.NewRecorder()
	getC, _ := gin.CreateTestContext(getRec)
	getC.Request = httptest.NewRequest(http.MethodGet, "/admin/settings/error-handling-rules", nil)
	h.GetErrorHandlingRuleSettings(getC)
	require.Equal(t, http.StatusOK, getRec.Code)

	var getResp struct {
		Data dto.ErrorHandlingRuleSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	require.Len(t, getResp.Data.Rules, 3)
	require.NotNil(t, getResp.Data.Rules[0].Enabled)
	require.False(t, *getResp.Data.Rules[0].Enabled)
	require.NotNil(t, getResp.Data.Rules[1].Enabled)
	require.True(t, *getResp.Data.Rules[1].Enabled)
	require.Nil(t, getResp.Data.Rules[2].Enabled, "未带 enabled 的规则不能被补成显式值")
}
