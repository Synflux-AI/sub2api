package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 中间件挂上记录器，setOpsSelectedAccount 登记账户配置的上游ID头名，
// HTTP 上游的响应随后能按该头名解析出标识。
func TestOpsErrorLoggerMiddlewareCapturesSelectedAccountUpstreamRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &service.Account{
		ID:       21,
		Platform: service.PlatformAnthropic,
		Extra:    map[string]any{service.AccountExtraUpstreamRequestIDHeader: "X-Relay-Request-Id"},
	}

	var got string
	r := gin.New()
	r.Use(OpsErrorLoggerMiddleware(nil))
	r.POST("/v1/messages", func(c *gin.Context) {
		setOpsSelectedAccount(c, account)
		resp := &http.Response{Header: http.Header{"X-Relay-Request-Id": []string{"relay-7"}}}
		service.RecordOpsUpstreamResponse(c.Request, account.ID, resp, nil)
		got = service.OpsUpstreamRequestIDFromContext(c.Request.Context(), account.ID)
		c.Status(http.StatusBadGateway)
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	require.Equal(t, "relay-7", got)
}
