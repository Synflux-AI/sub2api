//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// accessTokenRepoStub 只实现本功能用到的方法并记录调用次数；未桩方法调用即 panic
// （嵌入 nil 接口），这样「不该查库的路径查了库」会直接炸而不是静默通过。
type accessTokenRepoStub struct {
	UserAccessTokenRepository

	mu sync.Mutex

	record   *UserAccessToken
	getErr   error
	tokenErr error

	getByUserIDCalls int
	getByTokenCalls  int
	upsertCalls      int
	deleteCalls      int

	upsertedTokens []string
	deleteErr      error

	// touch 相关
	touchCalls   atomic.Int64
	touched      chan accessTokenTouchRecord
	touchBlockCh chan struct{}
}

type accessTokenTouchRecord struct {
	userID   int64
	token    string
	ctxErr   error
	deadline bool
}

func newAccessTokenRepoStub() *accessTokenRepoStub {
	return &accessTokenRepoStub{touched: make(chan accessTokenTouchRecord, 64)}
}

func (r *accessTokenRepoStub) GetByUserID(_ context.Context, _ int64) (*UserAccessToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getByUserIDCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	return r.record, nil
}

func (r *accessTokenRepoStub) GetByToken(_ context.Context, token string) (*UserAccessToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getByTokenCalls++
	if r.tokenErr != nil {
		return nil, r.tokenErr
	}
	if r.record == nil || r.record.Token != token {
		return nil, ErrAccessTokenNotFound
	}
	return r.record, nil
}

func (r *accessTokenRepoStub) Upsert(_ context.Context, userID int64, token string) (*UserAccessToken, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsertCalls++
	r.upsertedTokens = append(r.upsertedTokens, token)
	r.record = &UserAccessToken{UserID: userID, Token: token, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	return r.record, nil
}

func (r *accessTokenRepoStub) Delete(_ context.Context, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteCalls++
	return r.deleteErr
}

func (r *accessTokenRepoStub) TouchLastUsed(ctx context.Context, userID int64, token string) error {
	if r.touchBlockCh != nil {
		<-r.touchBlockCh
	}
	r.touchCalls.Add(1)
	_, hasDeadline := ctx.Deadline()
	r.touched <- accessTokenTouchRecord{
		userID:   userID,
		token:    token,
		ctxErr:   ctx.Err(),
		deadline: hasDeadline,
	}
	return nil
}

func (r *accessTokenRepoStub) counts() (getByUserID, getByToken, upsert, del int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getByUserIDCalls, r.getByTokenCalls, r.upsertCalls, r.deleteCalls
}

type accessTokenUserStub struct {
	user *User
	err  error

	calls atomic.Int64
}

func (s *accessTokenUserStub) GetByID(context.Context, int64) (*User, error) {
	s.calls.Add(1)
	return s.user, s.err
}

func newAccessTokenTestUser(t *testing.T, password string) *User {
	t.Helper()
	user := &User{ID: 7, Status: StatusActive, Role: RoleUser}
	require.NoError(t, user.SetPassword(password))
	return user
}

func newAccessTokenServiceForTest(
	t *testing.T,
	repo UserAccessTokenRepository,
	users accessTokenUserLookup,
	opts UserAccessTokenServiceOptions,
) *UserAccessTokenService {
	t.Helper()
	svc := newUserAccessTokenService(repo, users, opts)
	t.Cleanup(svc.Stop)
	return svc
}

func TestGenerateAccessTokenShapeAndUniqueness(t *testing.T) {
	first, err := generateAccessToken()
	require.NoError(t, err)
	second, err := generateAccessToken()
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	for _, token := range []string{first, second} {
		require.True(t, strings.HasPrefix(token, "sat-"))
		require.Len(t, token, len("sat-")+64)
		require.True(t, IsValidAccessTokenFormat(token), "generated token must pass its own validator: %s", token)
	}
}

func TestIsValidAccessTokenFormat(t *testing.T) {
	valid := "sat-" + strings.Repeat("0123456789abcdef", 4)
	require.Len(t, valid, 68)
	require.True(t, IsValidAccessTokenFormat(valid))

	hex64 := strings.Repeat("a", 64)
	invalid := map[string]string{
		"empty":            "",
		"prefix only":      "sat-",
		"gateway key":      "sk-" + hex64,
		"no prefix":        hex64,
		"uppercase hex":    "sat-" + strings.Repeat("A", 64),
		"non hex char":     "sat-" + strings.Repeat("a", 63) + "g",
		"too short":        "sat-" + strings.Repeat("a", 63),
		"too long":         "sat-" + strings.Repeat("a", 65),
		"whitespace":       "sat-" + strings.Repeat("a", 63) + " ",
		"prefix uppercase": "SAT-" + hex64,
	}
	for name, token := range invalid {
		t.Run(name, func(t *testing.T) {
			require.False(t, IsValidAccessTokenFormat(token))
		})
	}
}

func TestRotatePasswordGate(t *testing.T) {
	user := newAccessTokenTestUser(t, "correct-horse")

	t.Run("missing password", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		record, err := svc.Rotate(context.Background(), user.ID, "")
		require.Nil(t, record)
		require.ErrorIs(t, err, ErrPasswordRequired)
		_, _, upsert, _ := repo.counts()
		require.Zero(t, upsert)
	})

	t.Run("wrong password", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		record, err := svc.Rotate(context.Background(), user.ID, "nope")
		require.Nil(t, record)
		require.ErrorIs(t, err, ErrAccessTokenPasswordIncorrect)
		require.NotErrorIs(t, err, ErrPasswordIncorrect, "must not reuse the 400-mapped sentinel")
		_, _, upsert, _ := repo.counts()
		require.Zero(t, upsert)
	})

	t.Run("correct password", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		record, err := svc.Rotate(context.Background(), user.ID, "correct-horse")
		require.NoError(t, err)
		require.NotNil(t, record)
		require.True(t, IsValidAccessTokenFormat(record.Token))
		_, _, upsert, _ := repo.counts()
		require.Equal(t, 1, upsert)

		// 再次轮换必须换出一个不同的令牌
		next, err := svc.Rotate(context.Background(), user.ID, "correct-horse")
		require.NoError(t, err)
		require.NotEqual(t, record.Token, next.Token)
	})
}

func TestRevokePasswordGate(t *testing.T) {
	user := newAccessTokenTestUser(t, "correct-horse")

	t.Run("missing password", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		require.ErrorIs(t, svc.Revoke(context.Background(), user.ID, ""), ErrPasswordRequired)
		_, _, _, del := repo.counts()
		require.Zero(t, del)
	})

	t.Run("wrong password", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		require.ErrorIs(t, svc.Revoke(context.Background(), user.ID, "nope"), ErrAccessTokenPasswordIncorrect)
		_, _, _, del := repo.counts()
		require.Zero(t, del)
	})

	t.Run("correct password", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		require.NoError(t, svc.Revoke(context.Background(), user.ID, "correct-horse"))
		_, _, _, del := repo.counts()
		require.Equal(t, 1, del)
	})

	t.Run("no token", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		repo.deleteErr = ErrAccessTokenNotFound
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: user}, UserAccessTokenServiceOptions{})

		require.ErrorIs(t, svc.Revoke(context.Background(), user.ID, "correct-horse"), ErrAccessTokenNotFound)
	})
}

func TestAdminPathsSkipPasswordVerification(t *testing.T) {
	repo := newAccessTokenRepoStub()
	users := &accessTokenUserStub{err: errors.New("admin path must not read the target user")}
	svc := newAccessTokenServiceForTest(t, repo, users, UserAccessTokenServiceOptions{})

	record, err := svc.RotateForAdmin(context.Background(), 42)
	require.NoError(t, err)
	require.True(t, IsValidAccessTokenFormat(record.Token))
	require.EqualValues(t, 42, record.UserID)

	require.NoError(t, svc.RevokeForAdmin(context.Background(), 42))

	_, _, upsert, del := repo.counts()
	require.Equal(t, 1, upsert)
	require.Equal(t, 1, del)
	require.Zero(t, users.calls.Load(), "admin paths must not verify the target user's password")
}

func TestGetReturnsNilWithoutTokenAndPassesThroughRecord(t *testing.T) {
	repo := newAccessTokenRepoStub()
	repo.getErr = ErrAccessTokenNotFound
	svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{}, UserAccessTokenServiceOptions{})

	record, err := svc.Get(context.Background(), 7)
	require.NoError(t, err)
	require.Nil(t, record)

	repo.getErr = nil
	repo.record = &UserAccessToken{UserID: 7, Token: "sat-" + strings.Repeat("a", 64)}
	record, err = svc.Get(context.Background(), 7)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, repo.record.Token, record.Token)
}

func TestAuthenticate(t *testing.T) {
	validToken := "sat-" + strings.Repeat("b", 64)
	activeUser := &User{ID: 7, Status: StatusActive, Role: RoleUser}

	t.Run("success", func(t *testing.T) {
		repo := newAccessTokenRepoStub()
		repo.record = &UserAccessToken{UserID: 7, Token: validToken}
		svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{user: activeUser}, UserAccessTokenServiceOptions{})

		user, err := svc.Authenticate(context.Background(), validToken)
		require.NoError(t, err)
		require.Equal(t, activeUser.ID, user.ID)
	})

	// 三条失败路径必须返回完全相同、不可区分的错误
	badFormatRepo := newAccessTokenRepoStub()
	badFormatSvc := newAccessTokenServiceForTest(t, badFormatRepo, &accessTokenUserStub{user: activeUser}, UserAccessTokenServiceOptions{})
	_, badFormatErr := badFormatSvc.Authenticate(context.Background(), "sk-"+strings.Repeat("b", 64))
	require.ErrorIs(t, badFormatErr, ErrInvalidAccessToken)
	_, getByToken, _, _ := badFormatRepo.counts()
	require.Zero(t, getByToken, "malformed credentials must never reach the database")

	unknownRepo := newAccessTokenRepoStub()
	unknownSvc := newAccessTokenServiceForTest(t, unknownRepo, &accessTokenUserStub{user: activeUser}, UserAccessTokenServiceOptions{})
	_, unknownErr := unknownSvc.Authenticate(context.Background(), validToken)
	require.ErrorIs(t, unknownErr, ErrInvalidAccessToken)
	_, getByToken, _, _ = unknownRepo.counts()
	require.Equal(t, 1, getByToken)

	inactiveRepo := newAccessTokenRepoStub()
	inactiveRepo.record = &UserAccessToken{UserID: 7, Token: validToken}
	inactiveSvc := newAccessTokenServiceForTest(t, inactiveRepo,
		&accessTokenUserStub{user: &User{ID: 7, Status: StatusDisabled}}, UserAccessTokenServiceOptions{})
	_, inactiveErr := inactiveSvc.Authenticate(context.Background(), validToken)
	require.ErrorIs(t, inactiveErr, ErrInvalidAccessToken)

	lookupFailRepo := newAccessTokenRepoStub()
	lookupFailRepo.tokenErr = errors.New("database is down")
	lookupFailSvc := newAccessTokenServiceForTest(t, lookupFailRepo, &accessTokenUserStub{user: activeUser}, UserAccessTokenServiceOptions{})
	_, lookupFailErr := lookupFailSvc.Authenticate(context.Background(), validToken)
	require.ErrorIs(t, lookupFailErr, ErrInvalidAccessToken)

	for _, err := range []error{unknownErr, inactiveErr, lookupFailErr} {
		require.Equal(t, badFormatErr.Error(), err.Error(),
			"authentication failures must be indistinguishable")
		require.Same(t, badFormatErr, err)
	}
}

func TestTouchLastUsedDebouncesPerToken(t *testing.T) {
	repo := newAccessTokenRepoStub()
	svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{}, UserAccessTokenServiceOptions{
		TouchInterval: time.Hour,
		TouchWorkers:  1,
	})

	tokenA := "sat-" + strings.Repeat("a", 64)
	tokenB := "sat-" + strings.Repeat("b", 64)
	for i := 0; i < 5; i++ {
		svc.TouchLastUsed(context.Background(), 7, tokenA)
	}
	svc.TouchLastUsed(context.Background(), 8, tokenB)

	// 只有两次落库（每个令牌一次），且入队是同步完成的，所以收到两条之后队列必空。
	seen := map[string]int64{}
	for i := 0; i < 2; i++ {
		record := receiveTouch(t, repo)
		seen[record.token] = record.userID
	}
	require.Equal(t, map[string]int64{tokenA: 7, tokenB: 8}, seen)
	requireNoFurtherTouch(t, repo)
	require.EqualValues(t, 2, repo.touchCalls.Load())
}

func TestTouchLastUsedWritesAgainAfterWindow(t *testing.T) {
	clock := &accessTokenTestClock{}
	clock.set(time.Unix(1_700_000_000, 0))

	repo := newAccessTokenRepoStub()
	svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{}, UserAccessTokenServiceOptions{
		TouchInterval: time.Minute,
		TouchWorkers:  1,
		Now:           clock.now,
	})

	token := "sat-" + strings.Repeat("c", 64)
	svc.TouchLastUsed(context.Background(), 7, token)
	require.Equal(t, token, receiveTouch(t, repo).token)

	// 窗口内（59s）不再落库
	clock.advance(59 * time.Second)
	svc.TouchLastUsed(context.Background(), 7, token)
	requireNoFurtherTouch(t, repo)

	// 跨过窗口后再落一次
	clock.advance(2 * time.Second)
	svc.TouchLastUsed(context.Background(), 7, token)
	require.Equal(t, token, receiveTouch(t, repo).token)
	require.EqualValues(t, 2, repo.touchCalls.Load())
}

func TestTouchLastUsedDetachesRequestContext(t *testing.T) {
	repo := newAccessTokenRepoStub()
	svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{}, UserAccessTokenServiceOptions{
		TouchWorkers: 1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟响应写完、请求 ctx 立刻被取消

	svc.TouchLastUsed(ctx, 7, "sat-"+strings.Repeat("d", 64))
	record := receiveTouch(t, repo)
	require.NoError(t, record.ctxErr, "background touch must not inherit request cancellation")
	require.True(t, record.deadline, "background touch must carry its own timeout")
}

func TestTouchLastUsedDropsWhenQueueIsFullWithoutBlocking(t *testing.T) {
	repo := newAccessTokenRepoStub()
	repo.touchBlockCh = make(chan struct{})
	svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{}, UserAccessTokenServiceOptions{
		TouchInterval:  time.Hour,
		TouchQueueSize: 1,
		TouchWorkers:   1,
	})
	t.Cleanup(func() { close(repo.touchBlockCh) })

	// 队列容量 1 + 唯一 worker 阻塞在仓储里：之后的 touch 必须被丢弃而不是阻塞。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			svc.TouchLastUsed(context.Background(), int64(i+1), uniqueTestAccessToken(i))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TouchLastUsed blocked while the queue was full")
	}
	require.EqualValues(t, 0, repo.touchCalls.Load(), "the blocked worker cannot have completed any write")
}

func TestTouchLastUsedIgnoresInvalidInput(t *testing.T) {
	repo := newAccessTokenRepoStub()
	svc := newAccessTokenServiceForTest(t, repo, &accessTokenUserStub{}, UserAccessTokenServiceOptions{TouchWorkers: 1})

	valid := "sat-" + strings.Repeat("e", 64)
	svc.TouchLastUsed(context.Background(), 0, valid)
	svc.TouchLastUsed(context.Background(), 7, "sk-"+strings.Repeat("e", 64))
	svc.TouchLastUsed(context.Background(), 7, "")
	requireNoFurtherTouch(t, repo)

	svc.TouchLastUsed(context.Background(), 7, valid)
	require.Equal(t, valid, receiveTouch(t, repo).token)
}

func TestTouchGateClearsWhenOverCapacity(t *testing.T) {
	gate := newAccessTokenTouchGate(time.Hour, 4, time.Now)

	for i := 0; i < 4; i++ {
		require.True(t, gate.claim(uniqueTestAccessToken(i)))
	}
	require.False(t, gate.claim(uniqueTestAccessToken(0)), "still inside the debounce window")
	require.EqualValues(t, 4, gate.size.Load())

	// 第 5 个键越界，整表清空重建；此后旧键的防抖状态被丢弃（可接受：最多多写一次库）。
	require.True(t, gate.claim(uniqueTestAccessToken(4)))
	require.EqualValues(t, 0, gate.size.Load())
	require.True(t, gate.claim(uniqueTestAccessToken(0)))
}

func TestStopIsIdempotent(t *testing.T) {
	svc := newUserAccessTokenService(newAccessTokenRepoStub(), &accessTokenUserStub{}, UserAccessTokenServiceOptions{})
	svc.Stop()
	svc.Stop()
}

func TestNewUserAccessTokenServiceToleratesNilUserService(t *testing.T) {
	svc := NewUserAccessTokenService(newAccessTokenRepoStub(), nil)
	t.Cleanup(svc.Stop)

	_, err := svc.Rotate(context.Background(), 7, "whatever")
	require.Error(t, err, "a nil *UserService must surface an error instead of panicking")
}

type accessTokenTestClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *accessTokenTestClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = t
}

func (c *accessTokenTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

func (c *accessTokenTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func uniqueTestAccessToken(i int) string {
	suffix := strings.Repeat("0", 64)
	hexDigits := "0123456789abcdef"
	return "sat-" + suffix[:60] +
		string(hexDigits[(i>>4)&0xf]) + string(hexDigits[i&0xf]) + "ab"
}

func receiveTouch(t *testing.T, repo *accessTokenRepoStub) accessTokenTouchRecord {
	t.Helper()
	select {
	case record := <-repo.touched:
		return record
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a debounced last_used_at write")
		return accessTokenTouchRecord{}
	}
}

// requireNoFurtherTouch 依赖入队的同步性：TouchLastUsed 返回时任务已经在队列里
// （或已被丢弃），因此「队列里没有更多任务」等价于「不会再有落库」，无需 sleep。
func requireNoFurtherTouch(t *testing.T, repo *accessTokenRepoStub) {
	t.Helper()
	select {
	case record := <-repo.touched:
		t.Fatalf("unexpected extra last_used_at write for user %d", record.userID)
	default:
	}
}
