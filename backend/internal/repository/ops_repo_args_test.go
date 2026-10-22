//go:build unit

package repository

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpsInsertErrorLogArgsPreservesExplicitZeroUpstreamStatus(t *testing.T) {
	zero := 0
	args := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{UpstreamStatusCode: &zero})

	require.Len(t, args, strings.Count(insertOpsErrorLogSQL, "$"))
	encoded, ok := args[28].(sql.NullInt64)
	require.True(t, ok)
	require.True(t, encoded.Valid)
	require.Zero(t, encoded.Int64)
}

func TestOpsInsertErrorLogArgsRequestAttributes(t *testing.T) {
	zeroMs := 0
	args := opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{
		DurationMs: &zeroMs,
		OpsRequestAttributes: service.OpsRequestAttributes{
			ReasoningEffort: "high", ServiceTier: "priority",
			ImageSize: "2K", ImageInputSize: "1536x1024", ImageResponseFormat: "url", ImageCount: 2,
		},
	})
	require.Len(t, args, 46)
	require.Equal(t, []any{
		sql.NullInt64{Int64: 0, Valid: true}, // 测到的 0ms 不是 NULL
		sql.NullString{String: "high", Valid: true},
		sql.NullString{String: "priority", Valid: true},
		sql.NullString{String: "2K", Valid: true},
		sql.NullString{String: "1536x1024", Valid: true},
		sql.NullString{String: "url", Valid: true},
		sql.NullInt64{Int64: 2, Valid: true},
	}, args[39:])

	for i, arg := range opsInsertErrorLogArgs(&service.OpsInsertErrorLogInput{})[39:] {
		switch v := arg.(type) {
		case sql.NullString:
			require.False(t, v.Valid, "arg %d", 39+i)
		case sql.NullInt64:
			require.False(t, v.Valid, "arg %d", 39+i)
		default:
			t.Fatalf("unexpected arg type %T", arg)
		}
	}
}

func TestOpsNullableIntPointerDistinguishesNilZeroAndStatus(t *testing.T) {
	missing := opsNullableIntPointer(nil).(sql.NullInt64)
	require.False(t, missing.Valid)

	zeroValue := 0
	zero := opsNullableIntPointer(&zeroValue).(sql.NullInt64)
	require.True(t, zero.Valid)
	require.Zero(t, zero.Int64)

	statusValue := 503
	status := opsNullableIntPointer(&statusValue).(sql.NullInt64)
	require.True(t, status.Valid)
	require.EqualValues(t, 503, status.Int64)
}
