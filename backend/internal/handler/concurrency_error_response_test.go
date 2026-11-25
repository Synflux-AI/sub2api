package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConcurrencyErrorResponse(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		slotType    string
		wantStatus  int
		wantType    string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "true concurrency timeout remains rate limit",
			err:         &ConcurrencyError{SlotType: "account", IsTimeout: true},
			slotType:    "user",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantCode:    gatewayConcurrencyLimitCode,
			wantMessage: "Concurrency limit exceeded for account, please retry later",
		},
		{
			name:        "full local wait queue has gateway code",
			err:         &WaitQueueFullError{SlotType: "account"},
			slotType:    "account",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantCode:    gatewayQueueFullCode,
			wantMessage: "Too many pending requests, please retry later",
		},
		{
			name:        "client cancellation is not classified as concurrency limit",
			err:         context.Canceled,
			slotType:    "user",
			wantStatus:  statusClientClosedRequest,
			wantType:    "api_error",
			wantMessage: "context canceled",
		},
		{
			name:        "deadline exceeded is service unavailable",
			err:         context.DeadlineExceeded,
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantMessage: "Service temporarily unavailable, please retry later",
		},
		{
			name:        "redis acquire error is service unavailable",
			err:         errors.New("redis unavailable"),
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantMessage: "Service temporarily unavailable, please retry later",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errType, code, message := concurrencyErrorResponse(tt.err, tt.slotType)
			require.Equal(t, tt.wantStatus, status)
			require.Equal(t, tt.wantType, errType)
			require.Equal(t, tt.wantCode, code)
			require.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestGoogleConcurrencyError(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantGStatus string
		wantMessage string
	}{
		{
			name:        "client cancellation is 499",
			err:         context.Canceled,
			wantStatus:  statusClientClosedRequest,
			wantGStatus: "CANCELLED",
			wantMessage: "context canceled",
		},
		{
			name:        "full wait queue is 429",
			err:         &WaitQueueFullError{SlotType: "user"},
			wantStatus:  http.StatusTooManyRequests,
			wantGStatus: "RESOURCE_EXHAUSTED",
			wantMessage: "Too many pending requests, please retry later",
		},
		{
			name:        "slot wait timeout is 429",
			err:         &ConcurrencyError{SlotType: "user", IsTimeout: true},
			wantStatus:  http.StatusTooManyRequests,
			wantGStatus: "RESOURCE_EXHAUSTED",
			wantMessage: "Concurrency limit exceeded for user, please retry later",
		},
		{
			name:        "acquire backend error is 503",
			err:         errors.New("redis unavailable"),
			wantStatus:  http.StatusServiceUnavailable,
			wantGStatus: "INTERNAL",
			wantMessage: "Service temporarily unavailable, please retry later",
		},
	}

	gin.SetMode(gin.TestMode)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)

			googleConcurrencyError(c, tt.err, "user")

			require.Equal(t, tt.wantStatus, rec.Code)
			var body struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
					Status  string `json:"status"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, tt.wantStatus, body.Error.Code)
			require.Equal(t, tt.wantGStatus, body.Error.Status)
			require.Equal(t, tt.wantMessage, body.Error.Message)
		})
	}
}

func TestMarkLocalUserSlotError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		err      error
		slotType string
		want     string
	}{
		{name: "user wait queue full", err: &WaitQueueFullError{SlotType: "user"}, slotType: "account", want: httputil.ErrorSourceLocal},
		{name: "user concurrency timeout", err: &ConcurrencyError{SlotType: "user", IsTimeout: true}, slotType: "account", want: httputil.ErrorSourceLocal},
		{name: "empty slot falls back to caller slot", err: &ConcurrencyError{IsTimeout: true}, slotType: "user", want: httputil.ErrorSourceLocal},
		{name: "account concurrency is platform capacity", err: &ConcurrencyError{SlotType: "account", IsTimeout: true}, slotType: "user", want: ""},
		{name: "account wait queue is platform capacity", err: &WaitQueueFullError{SlotType: "account"}, slotType: "user", want: ""},
		{name: "backend error is not marked", err: errors.New("redis down"), slotType: "user", want: ""},
		{name: "client cancellation is not marked", err: context.Canceled, slotType: "user", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			markLocalUserSlotError(c, tt.err, tt.slotType)
			require.Equal(t, tt.want, rec.Header().Get(httputil.ErrorSourceHeader))
		})
	}
}

func TestWriteUpstreamResponseDropsUpstreamErrorSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	headers := http.Header{}
	headers.Set(httputil.ErrorSourceHeader, httputil.ErrorSourceLocal)
	headers.Set("Content-Type", "application/json")

	writeUpstreamResponse(c, &service.UpstreamHTTPResult{
		StatusCode: http.StatusTooManyRequests,
		Headers:    headers,
		Body:       []byte(`{"error":{"message":"upstream"}}`),
	})

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Empty(t, rec.Header().Get(httputil.ErrorSourceHeader))
}
