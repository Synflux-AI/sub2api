//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetErrorLogByID_APIKeyPrefixAndUpstreamStatus(t *testing.T) {
	ctx := context.Background()
	_, _ = integrationDB.ExecContext(ctx, "TRUNCATE ops_error_logs RESTART IDENTITY CASCADE")
	repo := NewOpsRepository(integrationDB).(*opsRepository)

	var plainID int64
	err := integrationDB.QueryRowContext(ctx, `
		INSERT INTO ops_error_logs (
			error_phase, error_type, severity, status_code, created_at
		) VALUES (
			'upstream', 'upstream_error', 'error', 500, NOW()
		) RETURNING id`,
	).Scan(&plainID)
	require.NoError(t, err)

	plain, err := repo.GetErrorLogByID(ctx, plainID)
	require.NoError(t, err)
	require.Empty(t, plain.APIKeyPrefix)

	validID, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		TraceID:      "trace-error-123",
		ErrorPhase:   "request",
		ErrorType:    "api_error",
		Severity:     "error",
		StatusCode:   402,
		CreatedAt:    time.Now(),
		APIKeyPrefix: "sk-valid",
	})
	require.NoError(t, err)

	valid, err := repo.GetErrorLogByID(ctx, validID)
	require.NoError(t, err)
	require.Equal(t, "sk-valid", valid.APIKeyPrefix)
	require.Equal(t, "trace-error-123", valid.TraceID)

	zero := 0
	credentialFailureID, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		ErrorPhase:         "account_auth",
		ErrorType:          "upstream_error",
		Severity:           "error",
		StatusCode:         503,
		UpstreamStatusCode: &zero,
		CreatedAt:          time.Now(),
	})
	require.NoError(t, err)

	credentialFailure, err := repo.GetErrorLogByID(ctx, credentialFailureID)
	require.NoError(t, err)
	require.NotNil(t, credentialFailure.UpstreamStatusCode)
	require.Zero(t, *credentialFailure.UpstreamStatusCode)

	var userID int64
	email := fmt.Sprintf("ops-user-notes-%d@example.test", time.Now().UnixNano())
	err = integrationDB.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, notes)
		VALUES ($1, 'test-password-hash', '华东客户')
		RETURNING id`, email,
	).Scan(&userID)
	require.NoError(t, err)

	userErrorID, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		UserID:     &userID,
		ErrorPhase: "upstream",
		ErrorType:  "upstream_error",
		Severity:   "error",
		StatusCode: 500,
		CreatedAt:  time.Now(),
	})
	require.NoError(t, err)

	list, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{
		UserID:   &userID,
		View:     "all",
		Page:     1,
		PageSize: 20,
	})
	require.NoError(t, err)
	require.Len(t, list.Errors, 1)
	require.Equal(t, email, list.Errors[0].UserEmail)
	require.Equal(t, "华东客户", list.Errors[0].UserNotes)

	userDetail, err := repo.GetErrorLogByID(ctx, userErrorID)
	require.NoError(t, err)
	require.Equal(t, email, userDetail.UserEmail)
	require.Equal(t, "华东客户", userDetail.UserNotes)
}
