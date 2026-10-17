//go:build unit

package repository

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/responseoutcome"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func usageLogSelectColumnIndex(t *testing.T, column string) int {
	t.Helper()
	for i, name := range strings.Split(usageLogSelectColumns, ", ") {
		if strings.TrimSpace(name) == column {
			return i
		}
	}
	t.Fatalf("column %s not in usageLogSelectColumns", column)
	return -1
}

// namedColumnScannerStub 按列名覆盖少数列，其余列保持目标零值，避免每加一列就重写整行。
type namedColumnScannerStub struct {
	overrides map[int]any
}

func (s namedColumnScannerStub) Scan(dest ...any) error {
	for idx, value := range s.overrides {
		if idx >= len(dest) {
			return fmt.Errorf("override index %d out of range %d", idx, len(dest))
		}
		reflect.ValueOf(dest[idx]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

func TestPrepareUsageLogInsert_FinishReasonArgWiring(t *testing.T) {
	reason := responseoutcome.ReasonRefusal
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:       1,
		APIKeyID:     2,
		RequestID:    "client:finish-reason",
		Model:        "claude-x",
		FinishReason: &reason,
		ResponseOutcome: &responseoutcome.Outcome{
			Protocol:   responseoutcome.ProtocolAnthropicMessages,
			StopReason: "refusal",
			Refusal:    true,
			Terminal:   true,
		},
		CreatedAt: time.Now().UTC(),
	})
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	// 尾序：..., image_response_format, finish_reason, response_outcome, native_compaction_v2, created_at。
	finishIdx := len(prepared.args) - 4
	outcomeIdx := len(prepared.args) - 3
	require.Equal(t, "text", usageLogInsertArgTypes[finishIdx])
	require.Equal(t, "jsonb", usageLogInsertArgTypes[outcomeIdx])
	require.Equal(t, sql.NullString{String: "refusal", Valid: true}, prepared.args[finishIdx])
	outcomeJSON, ok := prepared.args[outcomeIdx].(string)
	require.True(t, ok, "response_outcome arg should be JSON string, got %T", prepared.args[outcomeIdx])
	require.JSONEq(t, `{"protocol":"anthropic_messages","stop_reason":"refusal","refusal":true,"has_output":false,"terminal":true}`, outcomeJSON)

	absent := prepareUsageLogInsert(&service.UsageLog{UserID: 1, APIKeyID: 2, RequestID: "client:absent", Model: "m", CreatedAt: time.Now().UTC()})
	require.Equal(t, sql.NullString{}, absent.args[finishIdx], "uncollected finish_reason must be NULL, never stop")
	require.Nil(t, absent.args[outcomeIdx], "uncollected response_outcome must be NULL")
}

func TestScanUsageLog_FinishReasonAndOutcome(t *testing.T) {
	finishIdx := usageLogSelectColumnIndex(t, "finish_reason")
	outcomeIdx := usageLogSelectColumnIndex(t, "response_outcome")

	log, err := scanUsageLog(namedColumnScannerStub{overrides: map[int]any{
		finishIdx:  sql.NullString{Valid: true, String: "truncated"},
		outcomeIdx: sql.NullString{Valid: true, String: `{"protocol":"openai_responses","status":"incomplete","incomplete_reason":"max_output_tokens","has_output":true,"terminal":true}`},
	}})
	require.NoError(t, err)
	require.NotNil(t, log.FinishReason)
	require.Equal(t, "truncated", *log.FinishReason)
	require.NotNil(t, log.ResponseOutcome)
	require.Equal(t, "max_output_tokens", log.ResponseOutcome.IncompleteReason)
	require.Equal(t, responseoutcome.ReasonTruncated, log.ResponseOutcome.FinishReason())

	legacy, err := scanUsageLog(namedColumnScannerStub{})
	require.NoError(t, err)
	require.Nil(t, legacy.FinishReason, "historical rows keep NULL")
	require.Nil(t, legacy.ResponseOutcome)
}

func TestAppendFinishReasonWhereCondition(t *testing.T) {
	conditions, args := appendFinishReasonWhereCondition([]string{"user_id = $1"}, []any{int64(7)}, []string{"refusal", " ", "truncated"})
	require.Equal(t, []string{"user_id = $1", "finish_reason IN ($2, $3)"}, conditions)
	require.Equal(t, []any{int64(7), "refusal", "truncated"}, args)

	conditions, args = appendFinishReasonWhereCondition(nil, nil, nil)
	require.Empty(t, conditions)
	require.Empty(t, args)
}
