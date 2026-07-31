package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ErrAccessTokenNotFound is returned when a lookup or delete on
// user_access_tokens does not match any row.
var ErrAccessTokenNotFound = infraerrors.NotFound("ACCESS_TOKEN_NOT_FOUND", "access token not found")

// UserAccessToken is the persistence representation of a user's single
// customer-facing "sat-…" access token. user_id is the primary key: each
// user has at most one token at a time, and issuing a new one (Upsert)
// rotates out the previous one.
type UserAccessToken struct {
	UserID     int64
	Token      string
	LastUsedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UserAccessTokenRepository persists user_access_tokens rows.
type UserAccessTokenRepository interface {
	GetByUserID(ctx context.Context, userID int64) (*UserAccessToken, error)
	GetByToken(ctx context.Context, token string) (*UserAccessToken, error)
	Upsert(ctx context.Context, userID int64, token string) (*UserAccessToken, error)
	Delete(ctx context.Context, userID int64) error
	TouchLastUsed(ctx context.Context, userID int64, token string) error
}
