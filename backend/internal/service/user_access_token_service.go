package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// ErrAccessTokenPasswordIncorrect 为本功能定义独立的 403：既有
// ErrPasswordIncorrect 映射为 400，不满足 issue #110 §15.1 的契约要求。
var ErrAccessTokenPasswordIncorrect = infraerrors.Forbidden(
	"ACCESS_TOKEN_PASSWORD_INCORRECT", "account password is incorrect")

// ErrInvalidAccessToken 是 Authenticate 唯一对外暴露的失败原因。格式不合法、
// 令牌不存在、用户不存在、用户被停用全部返回它，调用方（中间件）无法区分是哪
// 一步失败，避免把「令牌存在但用户被停用」这类信息泄露给探测者。
var ErrInvalidAccessToken = infraerrors.Unauthorized("INVALID_ACCESS_TOKEN", "invalid access token")

const (
	// accessTokenPrefix 是硬编码的：本功能的令牌与网关 API Key 是两套凭据，
	// 前缀不跟随 cfg.Default.APIKeyPrefix 变化，否则 sk- 与 sat- 的隔离会被配置破坏。
	accessTokenPrefix = "sat-"
	// accessTokenRandomBytes 与 APIKeyService.GenerateKey 同为 32 字节。
	accessTokenRandomBytes = 32
	accessTokenHexLength   = accessTokenRandomBytes * 2

	// MaxAccessTokenCredentialBytes 与 MaxAPIKeyCredentialBytes 同值，供中间件
	// 在查库之前做 header 长度门。
	MaxAccessTokenCredentialBytes = 128
)

const (
	// defaultAccessTokenTouchInterval：同一令牌最多每分钟落库一次 last_used_at。
	defaultAccessTokenTouchInterval = time.Minute
	// defaultAccessTokenTouchQueueSize：有界队列，满则丢弃本次 touch。
	defaultAccessTokenTouchQueueSize = 256
	// defaultAccessTokenTouchWorkers：常驻 worker 数，落库是单行 UPDATE，2 个够用。
	defaultAccessTokenTouchWorkers = 2
	// defaultAccessTokenTouchWriteTimeout：后台落库自带超时，不沿用请求 ctx 的取消。
	defaultAccessTokenTouchWriteTimeout = 5 * time.Second
	// defaultAccessTokenTouchGateMaxEntries：防抖表容量上限，见 accessTokenTouchGate。
	defaultAccessTokenTouchGateMaxEntries = 10000
)

// accessTokenUserLookup 是本服务对用户读取的窄依赖，由 *UserService 满足；
// 单测注入桩，避免为了取一个用户去实现整个 UserRepository。
type accessTokenUserLookup interface {
	GetByID(ctx context.Context, id int64) (*User, error)
}

// UserAccessTokenServiceOptions 暴露防抖窗口与队列参数，供单测注入确定性取值
// （否则测试要等一分钟才能观察到第二次落库）。零值一律回落到 default 常量。
type UserAccessTokenServiceOptions struct {
	// TouchInterval 是同一令牌两次落库之间的最小间隔。
	TouchInterval time.Duration
	// TouchQueueSize 是有界队列容量。
	TouchQueueSize int
	// TouchWorkers 是常驻 worker 数量。
	TouchWorkers int
	// TouchWriteTimeout 是单次后台落库的超时。
	TouchWriteTimeout time.Duration
	// TouchGateMaxEntries 是防抖表条目上限，超过即清空重建。
	TouchGateMaxEntries int
	// Now 允许测试控制防抖时钟；nil 时用 time.Now。
	Now func() time.Time
}

func (o UserAccessTokenServiceOptions) normalized() UserAccessTokenServiceOptions {
	if o.TouchInterval <= 0 {
		o.TouchInterval = defaultAccessTokenTouchInterval
	}
	if o.TouchQueueSize <= 0 {
		o.TouchQueueSize = defaultAccessTokenTouchQueueSize
	}
	if o.TouchWorkers <= 0 {
		o.TouchWorkers = defaultAccessTokenTouchWorkers
	}
	if o.TouchWriteTimeout <= 0 {
		o.TouchWriteTimeout = defaultAccessTokenTouchWriteTimeout
	}
	if o.TouchGateMaxEntries <= 0 {
		o.TouchGateMaxEntries = defaultAccessTokenTouchGateMaxEntries
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// accessTokenTouchJob 是排入有界队列的一次 last_used_at 落库。ctx 在入队时就用
// context.WithoutCancel 摘掉了请求的取消与 deadline，只留下 trace / logger 等值。
type accessTokenTouchJob struct {
	ctx    context.Context
	userID int64
	token  string
}

// accessTokenTouchGate 是进程内防抖门：token -> 上次「认领落库」的时间。
//
// 表的增长天然被「一用户一令牌」封顶（条目数 = 调用过 Open API 的用户数），但
// 令牌轮换会留下旧键，所以仍要兜底：条目数超过上限时整表清空重建（用
// atomic.Pointer 换一张新的 sync.Map）。选清空重建而不是定期扫描的理由是它
// O(1) 且无需额外 goroutine；代价只是每个活跃令牌最多多写一次库——远小于
// 无界增长的风险。并发下可能有两个 goroutine 同时判定要重建，结果只是多丢一
// 次防抖状态，无正确性影响。
type accessTokenTouchGate struct {
	entries    atomic.Pointer[sync.Map] // token -> *atomic.Int64（unix nano）
	size       atomic.Int64
	maxEntries int
	interval   time.Duration
	now        func() time.Time
}

func newAccessTokenTouchGate(interval time.Duration, maxEntries int, now func() time.Time) *accessTokenTouchGate {
	gate := &accessTokenTouchGate{
		maxEntries: maxEntries,
		interval:   interval,
		now:        now,
	}
	gate.entries.Store(&sync.Map{})
	return gate
}

// claim 判断此刻是否轮到该令牌落库。返回 true 的同时已把「上次落库时间」推到
// 当前时刻，因此并发的同令牌请求里只有一个能拿到 true。
//
// 认领成功但随后入队失败（队列满）时不回滚时间戳：那次 touch 就此丢弃，等下一
// 个窗口再来。这样「每令牌每分钟最多落库一次」是严格上界，代价是队列拥塞时
// last_used_at 会稍旧——对一个「最近使用时间」字段完全可以接受。
func (g *accessTokenTouchGate) claim(token string) bool {
	entries := g.entries.Load()
	value, loaded := entries.LoadOrStore(token, &atomic.Int64{})
	if !loaded && g.size.Add(1) > int64(g.maxEntries) {
		g.entries.Store(&sync.Map{})
		g.size.Store(0)
	}

	last, ok := value.(*atomic.Int64)
	if !ok {
		return false
	}
	now := g.now()
	for {
		previous := last.Load()
		if previous != 0 && now.Sub(time.Unix(0, previous)) < g.interval {
			return false
		}
		if last.CompareAndSwap(previous, now.UnixNano()) {
			return true
		}
	}
}

// UserAccessTokenService 管理用户级 access token（sat-…）的生成、轮换、撤销、
// 查询与认证。刻意不做任何认证缓存：令牌数量等于用户数、Open API 走独立 RPM
// 限流，直读数据库既够快，也让撤销立即生效（§15.2）。
type UserAccessTokenService struct {
	repo  UserAccessTokenRepository
	users accessTokenUserLookup

	gate         *accessTokenTouchGate
	touchQueue   chan accessTokenTouchJob
	writeTimeout time.Duration
	workers      int
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
}

// NewUserAccessTokenService 构造服务并启动常驻 touch worker。
func NewUserAccessTokenService(repo UserAccessTokenRepository, userService *UserService) *UserAccessTokenService {
	// 显式判空：把 nil *UserService 装进接口会得到非 nil 接口值，调用即 panic。
	var users accessTokenUserLookup
	if userService != nil {
		users = userService
	}
	return newUserAccessTokenService(repo, users, UserAccessTokenServiceOptions{})
}

func newUserAccessTokenService(
	repo UserAccessTokenRepository,
	users accessTokenUserLookup,
	opts UserAccessTokenServiceOptions,
) *UserAccessTokenService {
	opts = opts.normalized()
	s := &UserAccessTokenService{
		repo:         repo,
		users:        users,
		gate:         newAccessTokenTouchGate(opts.TouchInterval, opts.TouchGateMaxEntries, opts.Now),
		touchQueue:   make(chan accessTokenTouchJob, opts.TouchQueueSize),
		writeTimeout: opts.TouchWriteTimeout,
		workers:      opts.TouchWorkers,
		stopCh:       make(chan struct{}),
	}
	s.start()
	return s
}

func (s *UserAccessTokenService) start() {
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.touchWorker()
	}
}

// Stop 停掉常驻 worker，照 EmailQueueService.Stop 的既有形状。生产进程里没有调
// 用点（同 EmailQueueService：worker 随进程存活即可，队列里的 touch 是尽力而为
// 的非关键写），它存在是为了让单测收回 goroutine。可重复调用。
func (s *UserAccessTokenService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *UserAccessTokenService) touchWorker() {
	defer s.wg.Done()
	for {
		select {
		case job := <-s.touchQueue:
			s.flushTouch(job)
		case <-s.stopCh:
			return
		}
	}
}

func (s *UserAccessTokenService) flushTouch(job accessTokenTouchJob) {
	ctx, cancel := context.WithTimeout(job.ctx, s.writeTimeout)
	defer cancel()

	// 双匹配 (user_id, token) 在仓储层完成：轮换过的旧令牌不会把 last_used_at
	// 盖到当前令牌上，0 行受影响也不算错误。
	if err := s.repo.TouchLastUsed(ctx, job.userID, job.token); err != nil {
		logger.C(ctx).Warn("user_access_token_touch_failed",
			zap.Int64("user_id", job.userID),
			zap.Error(err),
		)
	}
}

// generateAccessToken 生成 sat- + 32 字节随机数的小写 hex（总长 68）。
func generateAccessToken() (string, error) {
	buf := make([]byte, accessTokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate access token random bytes: %w", err)
	}
	return accessTokenPrefix + hex.EncodeToString(buf), nil
}

// IsValidAccessTokenFormat 等价于 ^sat-[0-9a-f]{64}$。中间件在查库之前用它挡住
// 畸形、超长、以及 sk- 网关 Key 之类的其它凭据（§15.2）。
func IsValidAccessTokenFormat(token string) bool {
	if len(token) != len(accessTokenPrefix)+accessTokenHexLength {
		return false
	}
	if !strings.HasPrefix(token, accessTokenPrefix) {
		return false
	}
	for i := len(accessTokenPrefix); i < len(token); i++ {
		c := token[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// verifyAccessTokenPassword 照 verifyPasskeyPassword 的形状用账户密码给「查看 /
// 轮换 / 撤销明文令牌」加一道门，错误换成本功能的契约（400 / 403）。
func verifyAccessTokenPassword(user *User, password string) error {
	if password == "" {
		return ErrPasswordRequired
	}
	if user == nil || !user.CheckPassword(password) {
		return ErrAccessTokenPasswordIncorrect
	}
	return nil
}

// Get 返回用户当前的令牌；用户没有令牌时返回 (nil, nil)，不是错误——handler 据
// 此把 token 渲染成 null。
func (s *UserAccessTokenService) Get(ctx context.Context, userID int64) (*UserAccessToken, error) {
	record, err := s.repo.GetByUserID(ctx, userID)
	if errors.Is(err, ErrAccessTokenNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user access token: %w", err)
	}
	return record, nil
}

// Rotate 是用户自助路径：校验账户密码后生成新令牌并覆盖旧令牌。首次生成与轮换
// 共用此方法（Upsert 的主键是 user_id，一用户恒一枚）。
func (s *UserAccessTokenService) Rotate(ctx context.Context, userID int64, password string) (*UserAccessToken, error) {
	if err := s.verifyOwnerPassword(ctx, userID, password); err != nil {
		return nil, err
	}
	return s.issue(ctx, userID)
}

// Revoke 是用户自助路径：校验账户密码后删除令牌；无令牌时返回 ErrAccessTokenNotFound。
func (s *UserAccessTokenService) Revoke(ctx context.Context, userID int64, password string) error {
	if err := s.verifyOwnerPassword(ctx, userID, password); err != nil {
		return err
	}
	return s.delete(ctx, userID)
}

// RotateForAdmin 是管理员路径：**不校验目标用户密码**（管理员不知道对方密码，
// §15.1）。调用方的权限与 step-up 由路由中间件负责。
func (s *UserAccessTokenService) RotateForAdmin(ctx context.Context, targetUserID int64) (*UserAccessToken, error) {
	return s.issue(ctx, targetUserID)
}

// RevokeForAdmin 是管理员路径，同样不校验目标用户密码。
func (s *UserAccessTokenService) RevokeForAdmin(ctx context.Context, targetUserID int64) error {
	return s.delete(ctx, targetUserID)
}

// Authenticate 供 Open API 中间件使用：格式校验 → 查库 → 取用户 → 校验状态。
// 任何一步失败都返回 ErrInvalidAccessToken，调用方无法区分原因。
func (s *UserAccessTokenService) Authenticate(ctx context.Context, token string) (*User, error) {
	// 格式校验先于查库：畸形或超长输入绝不进数据库（§15.2）。
	if !IsValidAccessTokenFormat(token) {
		return nil, ErrInvalidAccessToken
	}

	record, err := s.repo.GetByToken(ctx, token)
	if err != nil {
		// 基础设施故障也统一收敛成 401，因此这里补一条日志，避免可观测性丢失。
		if !errors.Is(err, ErrAccessTokenNotFound) {
			logger.C(ctx).Warn("user_access_token_lookup_failed", zap.Error(err))
		}
		return nil, ErrInvalidAccessToken
	}
	if record == nil {
		return nil, ErrInvalidAccessToken
	}

	user, err := s.getUser(ctx, record.UserID)
	if err != nil {
		logger.C(ctx).Warn("user_access_token_user_lookup_failed",
			zap.Int64("user_id", record.UserID),
			zap.Error(err),
		)
		return nil, ErrInvalidAccessToken
	}
	if user == nil || !user.IsActive() {
		return nil, ErrInvalidAccessToken
	}
	return user, nil
}

// TouchLastUsed 以「防抖 + 有界队列 + 后台落库」的方式更新 last_used_at，绝不
// 阻塞请求路径，也绝不每请求起一个 goroutine（§15.2）。
func (s *UserAccessTokenService) TouchLastUsed(ctx context.Context, userID int64, token string) {
	if s == nil || s.repo == nil || userID <= 0 || !IsValidAccessTokenFormat(token) {
		return
	}
	if !s.gate.claim(token) {
		return
	}

	// 请求 ctx 在响应写完的瞬间就被取消，直接沿用会让后台 UPDATE 必然失败；
	// WithoutCancel 保留 trace_id 等值，超时由 worker 单独加。
	job := accessTokenTouchJob{
		ctx:    context.WithoutCancel(ctx),
		userID: userID,
		token:  token,
	}
	select {
	case s.touchQueue <- job:
	default:
		// 队列满：丢弃本次 touch。last_used_at 是尽力而为的观测字段，任何形式的
		// 阻塞或扩容都会把它变成请求路径上的风险。
		logger.C(ctx).Debug("user_access_token_touch_dropped", zap.Int64("user_id", userID))
	}
}

func (s *UserAccessTokenService) verifyOwnerPassword(ctx context.Context, userID int64, password string) error {
	user, err := s.getUser(ctx, userID)
	if err != nil {
		return err
	}
	return verifyAccessTokenPassword(user, password)
}

func (s *UserAccessTokenService) getUser(ctx context.Context, userID int64) (*User, error) {
	if s.users == nil {
		return nil, fmt.Errorf("get user %d: user lookup is not configured", userID)
	}
	return s.users.GetByID(ctx, userID)
}

func (s *UserAccessTokenService) issue(ctx context.Context, userID int64) (*UserAccessToken, error) {
	token, err := generateAccessToken()
	if err != nil {
		return nil, err
	}
	record, err := s.repo.Upsert(ctx, userID, token)
	if err != nil {
		return nil, fmt.Errorf("issue user access token: %w", err)
	}
	return record, nil
}

func (s *UserAccessTokenService) delete(ctx context.Context, userID int64) error {
	if err := s.repo.Delete(ctx, userID); err != nil {
		if errors.Is(err, ErrAccessTokenNotFound) {
			return err
		}
		return fmt.Errorf("revoke user access token: %w", err)
	}
	return nil
}
