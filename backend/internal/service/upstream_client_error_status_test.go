package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 上游 400/413/422 属于客户端错误，必须原样返回状态码，而不是塌成通用 502。
// 否则客户端会把"请求太大/参数不合法"误判为网关故障并无意义重试。

func TestGatewayHandleErrorResponse_Preserves413And422(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		svc := &GatewayService{}
		respBody := []byte(`{"type":"error","error":{"type":"request_too_large","message":"Prompt is too long"}}`)
		resp := &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(bytes.NewReader(respBody)),
			Header:     http.Header{},
		}
		account := &Account{ID: 21, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

		_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
		require.Error(t, err)
		var failoverErr *UpstreamFailoverError
		require.False(t, errors.As(err, &failoverErr))

		assert.Equal(t, status, rec.Code, "upstream %d must be preserved", status)
		// 与 400 一致：客户端错误原样透传上游响应体。
		assert.Equal(t, respBody, rec.Body.Bytes())
	}
}

func TestOpenAIHandleErrorResponse_PreservesClientErrorStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		svc := &OpenAIGatewayService{}
		respBody := []byte(`{"error":{"message":"Invalid schema for field messages","type":"invalid_request_error"}}`)
		resp := &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(bytes.NewReader(respBody)),
			Header:     http.Header{},
		}
		account := &Account{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

		_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
		require.Error(t, err)
		var failoverErr *UpstreamFailoverError
		require.False(t, errors.As(err, &failoverErr))

		assert.Equal(t, status, rec.Code, "upstream %d must be preserved", status)

		var payload map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
		errField, ok := payload["error"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "invalid_request_error", errField["type"])
		assert.Equal(t, "Invalid schema for field messages", errField["message"])
	}
}

func TestGeminiWriteGeminiMappedError_Preserves413And422(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		upstreamStatus int
		wantType       string
		wantMsg        string
	}{
		{http.StatusRequestEntityTooLarge, "request_too_large", "Request payload too large"},
		{http.StatusUnprocessableEntity, "invalid_request_error", "Upstream could not process the request"},
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)

		svc := &GeminiMessagesCompatService{}
		// 非 JSON body（如代理/LB 直接拒绝时的纯文本），走纯 HTTP 状态码映射分支。
		respBody := []byte(`request entity too large`)
		account := &Account{ID: 23, Platform: PlatformGemini, Type: AccountTypeAPIKey}

		err := svc.writeGeminiMappedError(c, account, tc.upstreamStatus, "req-42", respBody)
		require.Error(t, err)

		assert.Equal(t, tc.upstreamStatus, rec.Code, "upstream %d must be preserved", tc.upstreamStatus)

		var payload map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
		errField, ok := payload["error"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, tc.wantType, errField["type"])
		assert.Equal(t, tc.wantMsg, errField["message"])
	}
}
