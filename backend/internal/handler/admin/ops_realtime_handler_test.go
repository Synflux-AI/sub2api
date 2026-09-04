package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type realtimeOffSettingRepo struct {
	service.SettingRepository
}

func (realtimeOffSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyOpsMonitoringEnabled: "true"}, nil
}

func (realtimeOffSettingRepo) GetValue(context.Context, string) (string, error) {
	return "false", nil
}

func TestGetConcurrencyStats_DisabledResponseIncludesModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := service.NewOpsService(nil, realtimeOffSettingRepo{}, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/concurrency", NewOpsHandler(svc).GetConcurrencyStats)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/concurrency", nil))

	require.Equal(t, http.StatusOK, response.Code)
	var envelope struct {
		Data struct {
			Enabled bool                                     `json:"enabled"`
			Model   map[string]*service.ModelConcurrencyInfo `json:"model"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.False(t, envelope.Data.Enabled)
	require.NotNil(t, envelope.Data.Model)
	require.Empty(t, envelope.Data.Model)
}
