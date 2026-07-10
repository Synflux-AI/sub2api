package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClientRequestIDGeneratesAndExposesID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, w.Body.String())
	require.Equal(t, w.Body.String(), w.Header().Get(clientRequestIDHeader))
}

func TestClientRequestIDPreservesExistingContextID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.ClientRequestID, "existing-client-request-id"))
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "existing-client-request-id", w.Body.String())
	require.Equal(t, "existing-client-request-id", w.Header().Get(clientRequestIDHeader))
}

// serveClientRequestID 用给定入站头跑一次中间件，返回 handler 观察到的 ctx 值。
func serveClientRequestID(t *testing.T, inbound string, setHeader bool) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ClientRequestID())
	router.GET("/", func(c *gin.Context) {
		value, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		c.String(http.StatusOK, value)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if setHeader {
		req.Header.Set(clientRequestIDHeader, inbound)
	}
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, w.Body.String(), w.Header().Get(clientRequestIDHeader))
	return w.Body.String()
}

func TestClientRequestIDAdoptsValidInboundHeader(t *testing.T) {
	// 合法 UUID：跨实例沿用，输出标准小写形式。
	got := serveClientRequestID(t, "550E8400-E29B-41D4-A716-446655440000", true)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", got)

	// 受限字符集（≤64，字母数字与 - _ .）：原样沿用。
	restricted := "req_cascade-01.abc"
	require.Equal(t, restricted, serveClientRequestID(t, restricted, true))
}

func TestClientRequestIDRejectsInvalidInboundHeader(t *testing.T) {
	cases := map[string]string{
		"too_long":          "a123456789012345678901234567890123456789012345678901234567890123456789", // >64
		"illegal_chars":     "req id with spaces",
		"log_injection":     "abc\ndef",
		"control_and_slash": "req/../../etc",
	}
	for name, inbound := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveClientRequestID(t, inbound, true)
			// 非法入站值一律丢弃并回落到新生成，绝不沿用原值。
			require.NotEqual(t, inbound, got)
			require.NotEmpty(t, got)
		})
	}
}
