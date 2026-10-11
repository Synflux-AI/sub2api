package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newOpsUpstreamRequestIDTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request = req.WithContext(WithOpsUpstreamRequestIDCapture(req.Context()))
	return c
}

func opsUpstreamResponseWithHeader(name, value string) *http.Response {
	h := http.Header{}
	h.Set(name, value)
	return &http.Response{StatusCode: http.StatusBadGateway, Header: h}
}

func lastOpsUpstreamEvent(t *testing.T, c *gin.Context) *OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.NotEmpty(t, events)
	return events[len(events)-1]
}

func TestAppendOpsUpstreamErrorUsesAccountUpstreamRequestIDHeader(t *testing.T) {
	c := newOpsUpstreamRequestIDTestContext()
	account := &Account{ID: 7, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Relay-Request-Id"}}
	RegisterOpsAccountUpstreamRequestIDHeader(c.Request.Context(), account)

	RecordOpsUpstreamResponse(c.Request, account.ID, opsUpstreamResponseWithHeader("X-Relay-Request-Id", " relay-1 "), nil)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, UpstreamStatusCode: 502, UpstreamRequestID: "hardcoded-x-request-id"})

	require.Equal(t, "relay-1", lastOpsUpstreamEvent(t, c).UpstreamRequestID)
}

func TestAppendOpsUpstreamErrorKeepsPathIDWithoutAccountHeader(t *testing.T) {
	c := newOpsUpstreamRequestIDTestContext()
	account := &Account{ID: 7}
	RegisterOpsAccountUpstreamRequestIDHeader(c.Request.Context(), account)

	RecordOpsUpstreamResponse(c.Request, account.ID, opsUpstreamResponseWithHeader("X-Relay-Request-Id", "relay-1"), nil)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, UpstreamStatusCode: 502, UpstreamRequestID: "req-from-path"})

	require.Equal(t, "req-from-path", lastOpsUpstreamEvent(t, c).UpstreamRequestID)
}

func TestRecordOpsUpstreamResponseClearsStaleIDOnFailedAttempt(t *testing.T) {
	c := newOpsUpstreamRequestIDTestContext()
	account := &Account{ID: 7, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Relay-Request-Id"}}
	RegisterOpsAccountUpstreamRequestIDHeader(c.Request.Context(), account)

	RecordOpsUpstreamResponse(c.Request, account.ID, opsUpstreamResponseWithHeader("X-Relay-Request-Id", "relay-1"), nil)
	RecordOpsUpstreamResponse(c.Request, account.ID, nil, errors.New("connection reset"))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, Kind: "request_error", Message: "connection reset"})

	require.Empty(t, lastOpsUpstreamEvent(t, c).UpstreamRequestID)
}

func TestOpsUpstreamRequestIDIsTrackedPerAccount(t *testing.T) {
	c := newOpsUpstreamRequestIDTestContext()
	first := &Account{ID: 7, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Relay-Request-Id"}}
	second := &Account{ID: 8, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Other-Id"}}
	RegisterOpsAccountUpstreamRequestIDHeader(c.Request.Context(), first)
	RegisterOpsAccountUpstreamRequestIDHeader(c.Request.Context(), second)

	RecordOpsUpstreamResponse(c.Request, first.ID, opsUpstreamResponseWithHeader("X-Relay-Request-Id", "relay-1"), nil)
	RecordOpsUpstreamResponse(c.Request, second.ID, opsUpstreamResponseWithHeader("X-Other-Id", "other-1"), nil)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: first.ID, UpstreamStatusCode: 500})

	require.Equal(t, "relay-1", lastOpsUpstreamEvent(t, c).UpstreamRequestID)
}

func TestOpsUpstreamRequestIDCaptureIsNoopWithoutRecorder(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	account := &Account{ID: 7, Extra: map[string]any{AccountExtraUpstreamRequestIDHeader: "X-Relay-Request-Id"}}

	RegisterOpsAccountUpstreamRequestIDHeader(c.Request.Context(), account)
	RecordOpsUpstreamResponse(c.Request, account.ID, opsUpstreamResponseWithHeader("X-Relay-Request-Id", "relay-1"), nil)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{AccountID: account.ID, UpstreamStatusCode: 502, UpstreamRequestID: "req-from-path"})

	require.Equal(t, "req-from-path", lastOpsUpstreamEvent(t, c).UpstreamRequestID)
}

func TestWithOpsUpstreamRequestIDCaptureKeepsExistingRecorder(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx := WithOpsUpstreamRequestIDCapture(req.Context())
	require.Same(t, opsUpstreamRequestIDsFromContext(ctx), opsUpstreamRequestIDsFromContext(WithOpsUpstreamRequestIDCapture(ctx)))
}
