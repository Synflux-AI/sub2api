package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"golang.org/x/sync/semaphore"
)

// getByTokenConcurrencyLimit bounds how many GetByToken lookups can be in
// flight against the database at once. A flood of invalid/valid tokens still
// lands one primary-key/unique-index lookup each; capping concurrency turns
// saturation into added latency instead of surfacing a new failure mode
// outside the feature's fixed 401/429 error contract.
const getByTokenConcurrencyLimit = 64

type userAccessTokenRepository struct {
	db  *sql.DB
	sem *semaphore.Weighted
}

func NewUserAccessTokenRepository(db *sql.DB) service.UserAccessTokenRepository {
	return &userAccessTokenRepository{
		db:  db,
		sem: semaphore.NewWeighted(getByTokenConcurrencyLimit),
	}
}

func (r *userAccessTokenRepository) GetByUserID(ctx context.Context, userID int64) (*service.UserAccessToken, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT user_id, token, last_used_at, created_at, updated_at
		FROM user_access_tokens
		WHERE user_id = $1
	`, userID)
	token, err := scanUserAccessToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAccessTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user access token by user id: %w", err)
	}
	return token, nil
}

func (r *userAccessTokenRepository) GetByToken(ctx context.Context, token string) (*service.UserAccessToken, error) {
	// Acquire blocks rather than fast-failing: at saturation this adds
	// latency only, it never introduces a status code beyond 401/429.
	if err := r.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer r.sem.Release(1)

	row := r.db.QueryRowContext(ctx, `
		SELECT user_id, token, last_used_at, created_at, updated_at
		FROM user_access_tokens
		WHERE token = $1
	`, token)
	record, err := scanUserAccessToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAccessTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user access token by token: %w", err)
	}
	return record, nil
}

func (r *userAccessTokenRepository) Upsert(ctx context.Context, userID int64, token string) (*service.UserAccessToken, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO user_access_tokens (user_id, token, last_used_at, created_at, updated_at)
		VALUES ($1, $2, NULL, NOW(), NOW())
		ON CONFLICT (user_id) DO UPDATE SET
		    token        = EXCLUDED.token,
		    last_used_at = NULL,
		    created_at   = NOW(),
		    updated_at   = NOW()
		RETURNING user_id, token, last_used_at, created_at, updated_at
	`, userID, token)
	record, err := scanUserAccessToken(row)
	if err != nil {
		return nil, fmt.Errorf("upsert user access token: %w", err)
	}
	return record, nil
}

func (r *userAccessTokenRepository) Delete(ctx context.Context, userID int64) error {
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM user_access_tokens
		WHERE user_id = $1
	`, userID)
	if err != nil {
		return fmt.Errorf("delete user access token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user access token: %w", err)
	}
	if affected == 0 {
		return service.ErrAccessTokenNotFound
	}
	return nil
}

func (r *userAccessTokenRepository) TouchLastUsed(ctx context.Context, userID int64, token string) error {
	// Matching on (user_id, token) together is deliberate: a delayed touch
	// from a token that has since been rotated or revoked must not stamp
	// last_used_at on whatever token is now current for that user. Affecting
	// 0 rows is that normal race, not an error.
	_, err := r.db.ExecContext(ctx, `
		UPDATE user_access_tokens
		SET last_used_at = NOW(), updated_at = NOW()
		WHERE user_id = $1 AND token = $2
	`, userID, token)
	if err != nil {
		return fmt.Errorf("touch user access token last used: %w", err)
	}
	return nil
}

type userAccessTokenScanner interface {
	Scan(dest ...any) error
}

func scanUserAccessToken(scanner userAccessTokenScanner) (*service.UserAccessToken, error) {
	var token service.UserAccessToken
	if err := scanner.Scan(
		&token.UserID,
		&token.Token,
		&token.LastUsedAt,
		&token.CreatedAt,
		&token.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &token, nil
}
