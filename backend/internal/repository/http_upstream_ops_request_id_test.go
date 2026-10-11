package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamRecordsAccountUpstreamRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Relay-Request-Id", "relay-42")
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	account := &service.Account{ID: 11, Extra: map[string]any{service.AccountExtraUpstreamRequestIDHeader: "X-Relay-Request-Id"}}
	ctx := service.WithOpsUpstreamRequestIDCapture(context.Background())
	service.RegisterOpsAccountUpstreamRequestIDHeader(ctx, account)

	upstream := NewHTTPUpstream(nil)
	for name, do := range map[string]func(*http.Request) (*http.Response, error){
		"Do": func(req *http.Request) (*http.Response, error) { return upstream.Do(req, "", account.ID, 1) },
		"DoWithTLS": func(req *http.Request) (*http.Response, error) {
			return upstream.DoWithTLS(req, "", account.ID, 1, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
			require.NoError(t, err)
			resp, err := do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, "relay-42", service.OpsUpstreamRequestIDFromContext(ctx, account.ID))

			// 后续尝试连不上上游时清掉旧值，不把上一次的标识挂到本次错误上。
			bad, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:1", nil)
			require.NoError(t, err)
			_, err = do(bad)
			require.Error(t, err)
			require.Empty(t, service.OpsUpstreamRequestIDFromContext(ctx, account.ID))
		})
	}
}
