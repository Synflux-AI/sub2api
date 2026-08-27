//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// #189 的接口契约：platforms 是裸数组，「全平台」由前端把所有平台勾上表达。
// 前端不传（或传空数组）一律按「存量配置」处理，服务层收窄成 anthropic —— 这样
// 老版本前端 PUT 上来的规则不会在升级瞬间对 OpenAI 生效。

func getErrorHandlingRuleSettings(t *testing.T, h *SettingHandler) dto.ErrorHandlingRuleSettings {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/settings/error-handling-rules", nil)
	h.GetErrorHandlingRuleSettings(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data dto.ErrorHandlingRuleSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Data
}

func TestErrorHandlingRuleSettingsBackfillsMissingPlatformsAsAnthropic(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{
			{ID: "legacy", StatusCodes: []int{429}, Action: service.ErrorHandlingActionRetry},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	got := getErrorHandlingRuleSettings(t, h)
	require.Len(t, got.Rules, 1)
	require.Equal(t, []string{service.PlatformAnthropic}, got.Rules[0].Platforms)
	require.Zero(t, got.Rules[0].MaxUpstreamLatencyMs)
}

func TestErrorHandlingRuleSettingsRoundTripsPlatformsAndLatency(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{{
			ID: "images-lost-ping", Name: "images 连接丢失换号",
			StatusCodes: []int{502}, Keywords: []string{"connection lost"},
			Action:    service.ErrorHandlingActionFailover,
			Platforms: []string{service.PlatformOpenAI}, MaxUpstreamLatencyMs: 5000,
		}},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	got := getErrorHandlingRuleSettings(t, h)
	require.Len(t, got.Rules, 1)
	require.Equal(t, []string{service.PlatformOpenAI}, got.Rules[0].Platforms)
	require.Equal(t, 5000, got.Rules[0].MaxUpstreamLatencyMs)
}

func TestUpdateErrorHandlingRuleSettingsRejectsUnsupportedPlatform(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{{
			ID: "r1", StatusCodes: []int{500}, Action: service.ErrorHandlingActionRetry,
			Platforms: []string{service.PlatformGemini},
		}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"引擎没接线 gemini，勾了不生效，必须在写入时就拦下来")
}

func TestUpdateErrorHandlingRuleSettingsRejectsNegativeUpstreamLatency(t *testing.T) {
	h := newErrorHandlingRuleTestHandler(t)
	rec := doErrorHandlingRulePut(t, h, UpdateErrorHandlingRuleSettingsRequest{
		Enabled: true, DefaultRetryCount: 1,
		Rules: []dto.ErrorHandlingRule{{
			ID: "r1", StatusCodes: []int{500}, Action: service.ErrorHandlingActionRetry,
			MaxUpstreamLatencyMs: -1,
		}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
