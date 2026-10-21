//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpsRepositoryInsertsRequestAttributes_Integration(t *testing.T) {
	ctx := context.Background()
	repo := NewOpsRepository(integrationDB).(*opsRepository)
	const grp = int64(987701)
	cleanup := func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM ops_error_logs WHERE group_id=$1`, grp)
	}
	cleanup()
	t.Cleanup(cleanup)

	g := grp
	durationMs := 1234
	now := time.Now().UTC()
	withAttrs := func(requestID string) *service.OpsInsertErrorLogInput {
		return &service.OpsInsertErrorLogInput{
			RequestID: requestID, GroupID: &g, ErrorPhase: "upstream", ErrorType: "upstream_error",
			Severity: "P2", StatusCode: 502, CreatedAt: now, DurationMs: &durationMs,
			OpsRequestAttributes: service.OpsRequestAttributes{
				ReasoningEffort: "xhigh", ServiceTier: "priority",
				ImageSize: "2K", ImageInputSize: "1536x1024", ImageResponseFormat: "b64_json", ImageCount: 2,
			},
		}
	}
	_, err := repo.InsertErrorLog(ctx, withAttrs("attrs-single"))
	require.NoError(t, err)
	_, err = repo.BatchInsertErrorLogs(ctx, []*service.OpsInsertErrorLogInput{
		withAttrs("attrs-batch"),
		{RequestID: "attrs-empty", GroupID: &g, ErrorPhase: "request", ErrorType: "api_error", Severity: "P3", StatusCode: 400, CreatedAt: now},
	})
	require.NoError(t, err)

	type row struct {
		duration                                                     sql.NullInt64
		effort, tier, imageSize, imageInputSize, imageResponseFormat sql.NullString
		imageCount                                                   sql.NullInt64
	}
	read := func(requestID string) row {
		var r row
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			SELECT duration_ms, reasoning_effort, service_tier, image_size, image_input_size,
			       image_response_format, image_count
			FROM ops_error_logs WHERE request_id = $1`, requestID,
		).Scan(&r.duration, &r.effort, &r.tier, &r.imageSize, &r.imageInputSize, &r.imageResponseFormat, &r.imageCount))
		return r
	}
	for _, requestID := range []string{"attrs-single", "attrs-batch"} {
		r := read(requestID)
		require.EqualValues(t, 1234, r.duration.Int64, requestID)
		require.Equal(t, "xhigh", r.effort.String)
		require.Equal(t, "priority", r.tier.String)
		require.Equal(t, "2K", r.imageSize.String)
		require.Equal(t, "1536x1024", r.imageInputSize.String)
		require.Equal(t, "b64_json", r.imageResponseFormat.String)
		require.EqualValues(t, 2, r.imageCount.Int64)
	}
	empty := read("attrs-empty")
	require.False(t, empty.duration.Valid)
	require.False(t, empty.effort.Valid)
	require.False(t, empty.tier.Valid)
	require.False(t, empty.imageSize.Valid)
	require.False(t, empty.imageInputSize.Valid)
	require.False(t, empty.imageResponseFormat.Valid)
	require.False(t, empty.imageCount.Valid)
}
