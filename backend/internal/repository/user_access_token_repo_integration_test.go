//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func mustCreateUserAccessTokenTestUser(t *testing.T) int64 {
	t.Helper()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{})
	return user.ID
}

func newUserAccessTokenTestToken(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

func countUserAccessTokenRows(t *testing.T, ctx context.Context, userID int64) int {
	t.Helper()
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_access_tokens WHERE user_id = $1`, userID).Scan(&count))
	return count
}

// TestUserAccessTokenRepo_Upsert_RotatesInPlace pins §15.1/§15.5: a second
// Upsert for the same user_id is an UPDATE, not a second row, and the
// previous token becomes unresolvable via GetByToken immediately.
func TestUserAccessTokenRepo_Upsert_RotatesInPlace(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	tokenA := newUserAccessTokenTestToken("sat-a")
	first, err := repo.Upsert(ctx, userID, tokenA)
	require.NoError(t, err)
	require.Equal(t, userID, first.UserID)
	require.Equal(t, tokenA, first.Token)
	require.Nil(t, first.LastUsedAt)
	require.Equal(t, 1, countUserAccessTokenRows(t, ctx, userID))

	tokenB := newUserAccessTokenTestToken("sat-b")
	second, err := repo.Upsert(ctx, userID, tokenB)
	require.NoError(t, err)
	require.Equal(t, userID, second.UserID)
	require.Equal(t, tokenB, second.Token)

	// Still exactly one row for this user: rotation updated in place.
	require.Equal(t, 1, countUserAccessTokenRows(t, ctx, userID))

	// The old token no longer resolves.
	_, err = repo.GetByToken(ctx, tokenA)
	require.ErrorIs(t, err, service.ErrAccessTokenNotFound)

	// The new token resolves to this user.
	got, err := repo.GetByToken(ctx, tokenB)
	require.NoError(t, err)
	require.Equal(t, userID, got.UserID)
}

// TestUserAccessTokenRepo_Upsert_RotateResetsTimestamps pins §15.5: rotating
// a token resets last_used_at to NULL and refreshes created_at to the new
// issuance time (created_at is "when this token was issued", not "when the
// user's very first token was issued").
func TestUserAccessTokenRepo_Upsert_RotateResetsTimestamps(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	tokenA := newUserAccessTokenTestToken("sat-a")
	first, err := repo.Upsert(ctx, userID, tokenA)
	require.NoError(t, err)

	// Mark the first token as used so we can observe it getting reset.
	require.NoError(t, repo.TouchLastUsed(ctx, userID, tokenA))
	touched, err := repo.GetByUserID(ctx, userID)
	require.NoError(t, err)
	require.NotNil(t, touched.LastUsedAt)

	tokenB := newUserAccessTokenTestToken("sat-b")
	second, err := repo.Upsert(ctx, userID, tokenB)
	require.NoError(t, err)

	require.Nil(t, second.LastUsedAt)
	require.True(t, second.CreatedAt.After(first.CreatedAt) || second.CreatedAt.Equal(first.CreatedAt),
		"rotated token's created_at should be refreshed to the new issuance time")

	reloaded, err := repo.GetByUserID(ctx, userID)
	require.NoError(t, err)
	require.Nil(t, reloaded.LastUsedAt)
}

// TestUserAccessTokenRepo_Delete_InvalidatesImmediately pins the Delete
// contract: both lookup paths fail right after deletion, and deleting a
// user with no token returns the not-found sentinel too.
func TestUserAccessTokenRepo_Delete_InvalidatesImmediately(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	token := newUserAccessTokenTestToken("sat")
	_, err := repo.Upsert(ctx, userID, token)
	require.NoError(t, err)

	require.NoError(t, repo.Delete(ctx, userID))

	_, err = repo.GetByUserID(ctx, userID)
	require.ErrorIs(t, err, service.ErrAccessTokenNotFound)

	_, err = repo.GetByToken(ctx, token)
	require.ErrorIs(t, err, service.ErrAccessTokenNotFound)

	// Deleting again (no row left) also reports not-found.
	err = repo.Delete(ctx, userID)
	require.ErrorIs(t, err, service.ErrAccessTokenNotFound)
}

// TestUserAccessTokenRepo_Delete_NonExistentUser pins Delete's behaviour for
// a user_id that never had a token at all.
func TestUserAccessTokenRepo_Delete_NonExistentUser(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	err := repo.Delete(ctx, userID)
	require.ErrorIs(t, err, service.ErrAccessTokenNotFound)
}

// TestUserAccessTokenRepo_TouchLastUsed_StaleTokenAfterRotationIsNoOp pins
// §15.5, the most important invariant in this task: a delayed touch that
// arrives for a token which has since been rotated out must not stamp
// last_used_at on whatever token is now current for that user, and must not
// return an error (this is a normal, expected race).
func TestUserAccessTokenRepo_TouchLastUsed_StaleTokenAfterRotationIsNoOp(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	tokenA := newUserAccessTokenTestToken("sat-a")
	_, err := repo.Upsert(ctx, userID, tokenA)
	require.NoError(t, err)

	tokenB := newUserAccessTokenTestToken("sat-b")
	_, err = repo.Upsert(ctx, userID, tokenB)
	require.NoError(t, err)

	// A delayed touch using the old, now-rotated-out token.
	err = repo.TouchLastUsed(ctx, userID, tokenA)
	require.NoError(t, err, "touching a rotated-out token must not error")

	current, err := repo.GetByUserID(ctx, userID)
	require.NoError(t, err)
	require.Equal(t, tokenB, current.Token)
	require.Nil(t, current.LastUsedAt, "stale touch must not stamp the current token's last_used_at")
}

// TestUserAccessTokenRepo_TouchLastUsed_MatchingTokenWrites pins the
// positive case: touching with the (user_id, token) pair that is actually
// current does write last_used_at.
func TestUserAccessTokenRepo_TouchLastUsed_MatchingTokenWrites(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	token := newUserAccessTokenTestToken("sat")
	_, err := repo.Upsert(ctx, userID, token)
	require.NoError(t, err)

	before, err := repo.GetByUserID(ctx, userID)
	require.NoError(t, err)
	require.Nil(t, before.LastUsedAt)

	require.NoError(t, repo.TouchLastUsed(ctx, userID, token))

	after, err := repo.GetByUserID(ctx, userID)
	require.NoError(t, err)
	require.NotNil(t, after.LastUsedAt)
}

// TestUserAccessTokenRepo_TouchLastUsed_UnknownUserTokenIsNoOp covers the
// other 0-rows-affected race: neither the user nor the token exists at all.
func TestUserAccessTokenRepo_TouchLastUsed_UnknownUserTokenIsNoOp(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	err := repo.TouchLastUsed(ctx, userID, newUserAccessTokenTestToken("sat-never-issued"))
	require.NoError(t, err)
}

// TestUserAccessTokenRepo_GetByToken_ConcurrentLookups exercises the
// semaphore-bounded GetByToken path under concurrency to make sure the cap
// only adds latency and never surfaces an error for valid lookups.
func TestUserAccessTokenRepo_GetByToken_ConcurrentLookups(t *testing.T) {
	ctx := context.Background()
	repo := NewUserAccessTokenRepository(integrationDB)
	userID := mustCreateUserAccessTokenTestUser(t)

	token := newUserAccessTokenTestToken("sat")
	_, err := repo.Upsert(ctx, userID, token)
	require.NoError(t, err)

	const concurrency = 200
	errCh := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			_, getErr := repo.GetByToken(ctx, token)
			errCh <- getErr
		}()
	}
	for i := 0; i < concurrency; i++ {
		require.NoError(t, <-errCh)
	}
}
