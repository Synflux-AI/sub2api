package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type opsErrorCorrelationRepo struct {
	service.OpsRepository
	detail *service.OpsErrorLogDetail
	filter *service.OpsErrorLogFilter
}

func (r *opsErrorCorrelationRepo) GetErrorLogByID(context.Context, int64) (*service.OpsErrorLogDetail, error) {
	return r.detail, nil
}

func (r *opsErrorCorrelationRepo) ListErrorLogs(_ context.Context, filter *service.OpsErrorLogFilter) (*service.OpsErrorLogList, error) {
	r.filter = filter
	return &service.OpsErrorLogList{}, nil
}

func TestOpsHandler_ListRequestErrorUpstreamErrors_CorrelationPriority(t *testing.T) {
	tests := []struct {
		name                string
		detail              *service.OpsErrorLogDetail
		wantTraceID         string
		wantRequestID       string
		wantClientRequestID string
	}{
		{
			name: "trace id",
			detail: &service.OpsErrorLogDetail{OpsErrorLog: service.OpsErrorLog{
				TraceID: " trace-1 ", RequestID: "request-1", ClientRequestID: "client-1",
			}},
			wantTraceID: "trace-1",
		},
		{
			name: "request id fallback",
			detail: &service.OpsErrorLogDetail{OpsErrorLog: service.OpsErrorLog{
				RequestID: " request-1 ", ClientRequestID: "client-1",
			}},
			wantRequestID: "request-1",
		},
		{
			name: "client request id fallback",
			detail: &service.OpsErrorLogDetail{OpsErrorLog: service.OpsErrorLog{
				ClientRequestID: " client-1 ",
			}},
			wantClientRequestID: "client-1",
		},
	}

	gin.SetMode(gin.TestMode)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &opsErrorCorrelationRepo{detail: tt.detail}
			svc := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.GET("/request-errors/:id/upstream-errors", NewOpsHandler(svc).ListRequestErrorUpstreamErrors)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/request-errors/1/upstream-errors", nil))

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200", w.Code)
			}
			if repo.filter == nil {
				t.Fatal("ListErrorLogs was not called")
			}
			if repo.filter.TraceID != tt.wantTraceID || repo.filter.RequestID != tt.wantRequestID || repo.filter.ClientRequestID != tt.wantClientRequestID {
				t.Fatalf("correlation filter = (%q, %q, %q), want (%q, %q, %q)",
					repo.filter.TraceID, repo.filter.RequestID, repo.filter.ClientRequestID,
					tt.wantTraceID, tt.wantRequestID, tt.wantClientRequestID)
			}
		})
	}
}
