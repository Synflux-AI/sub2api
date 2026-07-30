package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// UsageRecordWorkerPool.execute builds task contexts from context.Background(),
// so any correlation ID not copied here is gone by the time the usage row (and
// its business-event projection) is written.
func TestUsageRecordContextCarriesAllCorrelationIDs(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.TraceID, "trace-abc")
	parent = context.WithValue(parent, ctxkey.RequestID, "req-def")
	parent = context.WithValue(parent, ctxkey.ClientRequestID, "client-ghi")

	got := usageRecordContext(parent, context.Background())

	require.Equal(t, "trace-abc", got.Value(ctxkey.TraceID))
	require.Equal(t, "req-def", got.Value(ctxkey.RequestID))
	require.Equal(t, "client-ghi", got.Value(ctxkey.ClientRequestID))
}

func TestUsageRecordContextTrimsAndSkipsBlankIDs(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.TraceID, "  trace-abc  ")
	parent = context.WithValue(parent, ctxkey.RequestID, "   ")

	got := usageRecordContext(parent, context.Background())

	require.Equal(t, "trace-abc", got.Value(ctxkey.TraceID))
	require.Nil(t, got.Value(ctxkey.RequestID))
}

// A background settlement has no inbound request; it must not inherit a
// neighbouring request's trace.
func TestUsageRecordContextWithNilParentStaysBare(t *testing.T) {
	got := usageRecordContext(nil, context.Background()) //nolint:staticcheck // 显式验证 nil parent 的防御分支

	require.Nil(t, got.Value(ctxkey.TraceID))
	require.Nil(t, got.Value(ctxkey.RequestID))
	require.Nil(t, got.Value(ctxkey.ClientRequestID))
}

func TestWrapUsageRecordTaskContextPropagatesToTask(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.TraceID, "trace-abc")

	var seen string
	task := service.UsageRecordTask(func(ctx context.Context) {
		seen, _ = ctx.Value(ctxkey.TraceID).(string)
	})

	wrapUsageRecordTaskContext(parent, task)(context.Background())

	require.Equal(t, "trace-abc", seen)
}
