package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// AccountErrorRateMonitorService 按渠道(账号)独立巡检上游错误率:某个账号窗口内的上游错误率
// 破阈值即可单独告警,并(可开关地)调用 SetTempUnschedulable 把它临时移出调度列表,
// 避免单个故障渠道被整体平均掩盖、影响客户。与整体 upstream_error_rate 告警相互独立、互不干扰。
//
// 运行骨架对标 OpsAlertEvaluatorService(ticker + 分布式 leader lock + job heartbeat),
// 但状态机天然 per-account(key = account_id)。
const (
	accountErrorRateMonitorJobName = "account_error_rate_monitor"

	accountErrorRateMonitorTimeout       = 45 * time.Second
	accountErrorRateMonitorLeaderLockKey = "ops:account_errrate:monitor:leader"
	accountErrorRateMonitorLeaderLockTTL = 90 * time.Second
	accountErrorRateMonitorInterval      = 60 * time.Second

	// accountErrorRateMonitorStartupDelay 让首轮巡检延后启动,错开进程启动瞬间多个 ops
	// 后台服务(评估器/采集器/本服务)同时打 DB 的惊群,避开冷缓存期的查询尾延迟。
	accountErrorRateMonitorStartupDelay = 30 * time.Second

	// accountErrorRateMonitorNotifyTimeout 限制单个账号一轮通知(Lark+邮件)的耗时上界。
	accountErrorRateMonitorNotifyTimeout = 10 * time.Second

	// 「保持下线需人工恢复」模式下,把临时下线时间设到很远的将来(约 100 年)近似永久,
	// 仍可由 admin ClearTempUnschedulable 或人工改 schedulable 恢复。
	accountErrorRateMonitorManualHoldDuration = 100 * 365 * 24 * time.Hour
)

var accountErrorRateMonitorReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

// accountErrRateMonitorRepo 是本服务所需的账号写操作子集(便于测试 stub)。
type accountErrRateMonitorRepo interface {
	SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error
}

type AccountErrorRateMonitorService struct {
	opsService   *OpsService
	opsRepo      OpsRepository
	accountRepo  accountErrRateMonitorRepo
	emailService *EmailService
	larkService  *LarkService

	redisClient *redis.Client
	cfg         *config.Config
	instanceID  string

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup

	mu     sync.Mutex
	states map[int64]*accountBreachState

	skipLogMu sync.Mutex
	skipLogAt time.Time

	warnNoRedisOnce sync.Once
}

// accountBreachState 单账号的连续破阈值/告警/剥离状态。
type accountBreachState struct {
	LastEvaluatedAt     time.Time
	ConsecutiveBreaches int
	Firing              bool
	LastAlertAt         time.Time
	// DetachedUntil 记录本服务上次把账号剥离到的截止时间;now 越过它后允许再次剥离
	// (自愈模式下到期后若仍高错误率会重新剥离;人工模式下设到很远将来,不会自动再剥离)。
	DetachedUntil time.Time
}

func NewAccountErrorRateMonitorService(
	opsService *OpsService,
	opsRepo OpsRepository,
	accountRepo accountErrRateMonitorRepo,
	emailService *EmailService,
	larkService *LarkService,
	redisClient *redis.Client,
	cfg *config.Config,
) *AccountErrorRateMonitorService {
	return &AccountErrorRateMonitorService{
		opsService:   opsService,
		opsRepo:      opsRepo,
		accountRepo:  accountRepo,
		emailService: emailService,
		larkService:  larkService,
		redisClient:  redisClient,
		cfg:          cfg,
		instanceID:   uuid.NewString(),
		states:       map[int64]*accountBreachState{},
	}
}

func (s *AccountErrorRateMonitorService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		if s.stopCh == nil {
			s.stopCh = make(chan struct{})
		}
		s.wg.Add(1)
		go s.run()
	})
}

func (s *AccountErrorRateMonitorService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
	})
	s.wg.Wait()
}

func (s *AccountErrorRateMonitorService) run() {
	defer s.wg.Done()

	timer := time.NewTimer(accountErrorRateMonitorStartupDelay)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			s.runOnceSafely()
			timer.Reset(accountErrorRateMonitorInterval)
		case <-s.stopCh:
			return
		}
	}
}

// runOnceSafely 包裹单轮评估并 recover,确保某一轮的 panic(如通知组件内部异常)
// 只丢这一轮,而不会击穿 goroutine 终止整个进程。
func (s *AccountErrorRateMonitorService) runOnceSafely() {
	defer func() {
		if r := recover(); r != nil {
			logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] evaluate panic recovered: %v", r)
		}
	}()
	s.evaluateOnce(accountErrorRateMonitorInterval)
}

func (s *AccountErrorRateMonitorService) evaluateOnce(interval time.Duration) {
	if s == nil || s.opsRepo == nil || s.opsService == nil {
		return
	}
	if s.cfg != nil && !s.cfg.Ops.Enabled {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), accountErrorRateMonitorTimeout)
	defer cancel()

	if !s.opsService.IsMonitoringEnabled(ctx) {
		return
	}

	advanced, err := s.opsService.GetOpsAdvancedSettings(ctx)
	if err != nil || advanced == nil {
		return
	}
	mcfg := advanced.AccountErrorRateMonitor
	if !mcfg.Enabled {
		// 关闭时清空状态,避免下次开启时残留旧的连续计数。
		s.resetAll()
		return
	}
	normalizeOpsAccountErrorRateMonitorSettings(&mcfg)

	// 分布式 leader lock 复用 ops 的开关与 TTL 约定,但用独立 key,避免多实例重复剥离。
	lock := defaultOpsAlertRuntimeSettings().DistributedLock
	if loaded, lerr := s.opsService.GetOpsAlertRuntimeSettings(ctx); lerr == nil && loaded != nil {
		lock = loaded.DistributedLock
	}
	release, ok := s.tryAcquireLeaderLock(ctx, lock)
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}

	startedAt := time.Now().UTC()
	now := startedAt
	safeEnd := now.Truncate(time.Minute)
	if safeEnd.IsZero() {
		safeEnd = now
	}
	windowStart := safeEnd.Add(-time.Duration(mcfg.WindowMinutes) * time.Minute)

	rows, err := s.opsRepo.GetAccountErrorRates(ctx, windowStart, safeEnd)
	if err != nil {
		s.recordHeartbeatError(startedAt, time.Since(startedAt), err)
		logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] query failed: %v", err)
		return
	}

	s.pruneStates(rows, now)

	required := requiredSustainedBreaches(mcfg.SustainedMinutes, interval)
	evaluated := 0
	alertsSent := 0
	detached := 0

	for _, row := range rows {
		if row.AccountID <= 0 {
			continue
		}
		// 小样本跳过:窗口内请求数不足时错误率噪声大,不判定也不清状态机以外的连续计数。
		if mcfg.MinRequests > 0 && row.Requests < int64(mcfg.MinRequests) {
			s.resetState(row.AccountID, now)
			continue
		}
		evaluated++

		rate := row.ErrorRate()
		breached := rate > mcfg.ErrorRateThreshold
		consecutive := s.updateBreaches(row.AccountID, now, interval, breached)

		if breached && consecutive >= required {
			s.markFiring(row.AccountID)

			// 先止损:自动剥离(到期后允许再次剥离)。用独立的短 context,确保剥离这一关键
			// 动作不会被同一轮里前面账号的慢通知(Lark/SMTP)耗尽主 ctx 预算而失败。
			if mcfg.AutoDetach && s.shouldDetach(row.AccountID, now) {
				until := s.detachUntil(now, mcfg.DetachCooldownMinutes)
				reason := buildAccountDetachReason(rate, mcfg.ErrorRateThreshold, mcfg.WindowMinutes, row, mcfg.DetachCooldownMinutes)
				dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
				derr := s.accountRepo.SetTempUnschedulable(dctx, row.AccountID, until, reason)
				dcancel()
				if derr != nil {
					logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] detach failed (account=%d): %v", row.AccountID, derr)
				} else {
					detached++
					s.markDetached(row.AccountID, until)
					logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] detached account=%d (%s) rate=%.2f%% > %.2f%% until=%s", row.AccountID, row.AccountName, rate, mcfg.ErrorRateThreshold, until.Format(time.RFC3339))
				}
			}

			// 再告警(带冷却)。
			if mcfg.NotifyEnabled && s.alertCooldownPassed(row.AccountID, now, mcfg.AlertCooldownMinutes) {
				if s.notifyAccountBreach(row, rate, mcfg) {
					alertsSent++
					s.markAlerted(row.AccountID, now)
				}
			}
			continue
		}

		// 未破阈值:若此前在 firing,则视为恢复,清状态。
		// 注意:不主动 ClearTempUnschedulable —— 自愈模式靠到期自动回归,人工模式由人决定,
		// 避免与限流/人工下线等其它来源的临时不可调度状态打架。
		if !breached {
			s.resolveIfFiring(row.AccountID)
		}
	}

	result := truncateString(fmt.Sprintf("accounts=%d evaluated=%d threshold=%.2f window=%dm alerts=%d detached=%d auto_detach=%t", len(rows), evaluated, mcfg.ErrorRateThreshold, mcfg.WindowMinutes, alertsSent, detached, mcfg.AutoDetach), 2048)
	s.recordHeartbeatSuccess(startedAt, time.Since(startedAt), result)
}

// ---------- 通知 ----------

// notifyAccountBreach 用独立的有界 context 发通知:Lark(HTTP)/SMTP 可能很慢,绑到这个
// 短超时上,避免单个账号的慢通知耗尽主巡检 ctx 预算、饿死同一轮后续账号的告警与下一轮巡检。
func (s *AccountErrorRateMonitorService) notifyAccountBreach(row OpsAccountErrorRateRow, rate float64, mcfg OpsAccountErrorRateMonitorSettings) bool {
	ctx, cancel := context.WithTimeout(context.Background(), accountErrorRateMonitorNotifyTimeout)
	defer cancel()
	larkSent := s.sendLarkBreach(ctx, row, rate, mcfg)
	emailSent := s.sendEmailBreach(ctx, row, rate, mcfg)
	return larkSent || emailSent
}

func (s *AccountErrorRateMonitorService) sendLarkBreach(ctx context.Context, row OpsAccountErrorRateRow, rate float64, mcfg OpsAccountErrorRateMonitorSettings) bool {
	if s.larkService == nil || s.opsService == nil {
		return false
	}
	larkCfg, err := s.opsService.GetLarkNotificationConfig(ctx)
	if err != nil || larkCfg == nil || !larkCfg.Enabled || !larkCfg.Alert.Enabled {
		return false
	}
	platform := strings.TrimSpace(row.Platform)
	name := accountDisplayName(row)
	reason := buildAccountBreachReason(rate, mcfg, row)
	if err := s.larkService.SendAccountAnomalyCard(ctx, larkCfg, name, platform, "上游错误率过高", reason); err != nil {
		logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] lark send failed (account=%d): %v", row.AccountID, err)
		return false
	}
	return true
}

func (s *AccountErrorRateMonitorService) sendEmailBreach(ctx context.Context, row OpsAccountErrorRateRow, rate float64, mcfg OpsAccountErrorRateMonitorSettings) bool {
	if s.emailService == nil || s.opsService == nil {
		return false
	}
	emailCfg, err := s.opsService.GetEmailNotificationConfig(ctx)
	if err != nil || emailCfg == nil || !emailCfg.Alert.Enabled || len(emailCfg.Alert.Recipients) == 0 {
		return false
	}
	name := accountDisplayName(row)
	subject := fmt.Sprintf("[Ops Alert][渠道错误率] %s", name)
	body := fmt.Sprintf(`
<h2>渠道(账号)上游错误率过高</h2>
<p><b>渠道</b>: %s</p>
<p><b>平台</b>: %s</p>
<p><b>上游错误率</b>: %.2f%% (阈值 %.2f%%, 最近 %d 分钟)</p>
<p><b>窗口内</b>: 上游错误 %d / 请求 %d</p>
<p><b>处置</b>: %s</p>
`,
		htmlEscape(name),
		htmlEscape(strings.TrimSpace(row.Platform)),
		rate, mcfg.ErrorRateThreshold, mcfg.WindowMinutes,
		row.UpstreamErrors, row.Requests,
		htmlEscape(detachActionText(mcfg)),
	)

	anySent := false
	for _, to := range emailCfg.Alert.Recipients {
		addr := strings.TrimSpace(to)
		if addr == "" {
			continue
		}
		if err := s.emailService.SendEmail(ctx, addr, subject, body); err != nil {
			continue
		}
		anySent = true
	}
	return anySent
}

func accountDisplayName(row OpsAccountErrorRateRow) string {
	name := strings.TrimSpace(row.AccountName)
	if name == "" {
		return fmt.Sprintf("account#%d", row.AccountID)
	}
	return name
}

func buildAccountBreachReason(rate float64, mcfg OpsAccountErrorRateMonitorSettings, row OpsAccountErrorRateRow) string {
	return fmt.Sprintf("上游错误率 %.2f%% > %.2f%%(最近 %d 分钟;上游错误 %d/请求 %d)。%s",
		rate, mcfg.ErrorRateThreshold, mcfg.WindowMinutes, row.UpstreamErrors, row.Requests, detachActionText(mcfg))
}

func buildAccountDetachReason(rate, threshold float64, windowMinutes int, row OpsAccountErrorRateRow, cooldownMinutes int) string {
	if cooldownMinutes > 0 {
		return fmt.Sprintf("auto-detached by error-rate monitor: %.2f%% > %.2f%% (last %dm), auto-recover in %dm", rate, threshold, windowMinutes, cooldownMinutes)
	}
	return fmt.Sprintf("auto-detached by error-rate monitor: %.2f%% > %.2f%% (last %dm), manual recovery required", rate, threshold, windowMinutes)
}

func detachActionText(mcfg OpsAccountErrorRateMonitorSettings) string {
	if !mcfg.AutoDetach {
		return "仅告警(未自动剥离)"
	}
	if mcfg.DetachCooldownMinutes > 0 {
		return fmt.Sprintf("已自动剥离,%d 分钟后自动回归", mcfg.DetachCooldownMinutes)
	}
	return "已自动剥离,需人工恢复"
}

// ---------- 状态机 ----------

// ensureStateLocked 返回账号状态,不存在则新建。调用方必须已持有 s.mu。
func (s *AccountErrorRateMonitorService) ensureStateLocked(accountID int64) *accountBreachState {
	state, ok := s.states[accountID]
	if !ok {
		state = &accountBreachState{}
		s.states[accountID] = state
	}
	return state
}

func (s *AccountErrorRateMonitorService) updateBreaches(accountID int64, now time.Time, interval time.Duration, breached bool) int {
	if accountID <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.states[accountID]
	if !ok {
		state = &accountBreachState{}
		s.states[accountID] = state
	}
	// 评估间隔异常拉长(>3x)时重置连续计数,避免跨大段空窗(进程暂停/leader 切换)误判持续。
	// 取 3x 而非 2x 留出余量:单轮评估含 DB 查询与可能较慢的通知,正常周期偶尔会接近 2x。
	if !state.LastEvaluatedAt.IsZero() && interval > 0 {
		if now.Sub(state.LastEvaluatedAt) > interval*3 {
			state.ConsecutiveBreaches = 0
		}
	}
	state.LastEvaluatedAt = now
	if breached {
		state.ConsecutiveBreaches++
	} else {
		state.ConsecutiveBreaches = 0
	}
	return state.ConsecutiveBreaches
}

func (s *AccountErrorRateMonitorService) resetState(accountID int64, now time.Time) {
	if accountID <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[accountID]
	if !ok {
		state = &accountBreachState{}
		s.states[accountID] = state
	}
	state.LastEvaluatedAt = now
	state.ConsecutiveBreaches = 0
	state.Firing = false
}

// resolveIfFiring 在账号错误率回落、且此前处于 firing 时,视为一次事件结束:
// 清空 Firing 以及告警/剥离的冷却记忆(LastAlertAt/DetachedUntil),这样若该账号稍后再次
// 破阈值(新一轮事件),能立即重新告警并重新剥离,而不会被上一轮残留的冷却窗口误压制。
func (s *AccountErrorRateMonitorService) resolveIfFiring(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[accountID]
	if !ok || !state.Firing {
		return
	}
	state.Firing = false
	state.LastAlertAt = time.Time{}
	state.DetachedUntil = time.Time{}
}

func (s *AccountErrorRateMonitorService) alertCooldownPassed(accountID int64, now time.Time, cooldownMinutes int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[accountID]
	if !ok {
		return true
	}
	if state.LastAlertAt.IsZero() {
		return true
	}
	return now.Sub(state.LastAlertAt) >= time.Duration(cooldownMinutes)*time.Minute
}

// markFiring 在锁内把账号标记为 firing(状态机的所有写入都走加锁 setter,保持一致)。
func (s *AccountErrorRateMonitorService) markFiring(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureStateLocked(accountID).Firing = true
}

func (s *AccountErrorRateMonitorService) markAlerted(accountID int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureStateLocked(accountID).LastAlertAt = now
}

// shouldDetach 仅在上次剥离已到期(或从未剥离)时返回 true,避免每个周期重复写库。
func (s *AccountErrorRateMonitorService) shouldDetach(accountID int64, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[accountID]
	if !ok {
		return true
	}
	return state.DetachedUntil.IsZero() || !now.Before(state.DetachedUntil)
}

func (s *AccountErrorRateMonitorService) markDetached(accountID int64, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureStateLocked(accountID).DetachedUntil = until
}

func (s *AccountErrorRateMonitorService) detachUntil(now time.Time, cooldownMinutes int) time.Time {
	if cooldownMinutes > 0 {
		return now.Add(time.Duration(cooldownMinutes) * time.Minute)
	}
	return now.Add(accountErrorRateMonitorManualHoldDuration)
}

// pruneStates 清理"本轮结果集里已消失"的账号状态,但保留仍处于剥离窗口内
// (DetachedUntil 在未来)的账号——被剥离的渠道没有流量、会从结果集消失,
// 若此时清掉其 DetachedUntil,会导致剥离窗口内若有零星请求被重复写库剥离。
func (s *AccountErrorRateMonitorService) pruneStates(rows []OpsAccountErrorRateRow, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		if row.AccountID > 0 {
			live[row.AccountID] = struct{}{}
		}
	}
	for id, state := range s.states {
		if _, ok := live[id]; ok {
			continue
		}
		if state != nil && !state.DetachedUntil.IsZero() && now.Before(state.DetachedUntil) {
			continue
		}
		delete(s.states, id)
	}
}

func (s *AccountErrorRateMonitorService) resetAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states = map[int64]*accountBreachState{}
}

// ---------- leader lock / heartbeat(对标 OpsAlertEvaluatorService) ----------

func (s *AccountErrorRateMonitorService) tryAcquireLeaderLock(ctx context.Context, lock OpsDistributedLockSettings) (func(), bool) {
	if !lock.Enabled {
		return nil, true
	}
	if s.redisClient == nil {
		s.warnNoRedisOnce.Do(func() {
			logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] redis not configured; running without distributed lock")
		})
		return nil, true
	}
	key := accountErrorRateMonitorLeaderLockKey
	ttl := time.Duration(lock.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = accountErrorRateMonitorLeaderLockTTL
	}

	ok, err := s.redisClient.SetNX(ctx, key, s.instanceID, ttl).Result()
	if err != nil {
		s.warnNoRedisOnce.Do(func() {
			logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] leader lock SetNX failed; skipping this cycle: %v", err)
		})
		return nil, false
	}
	if !ok {
		s.maybeLogSkip(key)
		return nil, false
	}
	return func() {
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		_, _ = accountErrorRateMonitorReleaseScript.Run(releaseCtx, s.redisClient, []string{key}, s.instanceID).Result()
	}, true
}

func (s *AccountErrorRateMonitorService) maybeLogSkip(key string) {
	s.skipLogMu.Lock()
	defer s.skipLogMu.Unlock()
	now := time.Now()
	if !s.skipLogAt.IsZero() && now.Sub(s.skipLogAt) < time.Minute {
		return
	}
	s.skipLogAt = now
	logger.LegacyPrintf("service.account_error_rate_monitor", "[AccountErrRateMonitor] leader lock held by another instance; skipping (key=%q)", key)
}

func (s *AccountErrorRateMonitorService) recordHeartbeatSuccess(runAt time.Time, duration time.Duration, result string) {
	if s == nil || s.opsRepo == nil {
		return
	}
	now := time.Now().UTC()
	durMs := duration.Milliseconds()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg := strings.TrimSpace(result)
	if msg == "" {
		msg = "ok"
	}
	msg = truncateString(msg, 2048)
	_ = s.opsRepo.UpsertJobHeartbeat(ctx, &OpsUpsertJobHeartbeatInput{
		JobName:        accountErrorRateMonitorJobName,
		LastRunAt:      &runAt,
		LastSuccessAt:  &now,
		LastDurationMs: &durMs,
		LastResult:     &msg,
	})
}

func (s *AccountErrorRateMonitorService) recordHeartbeatError(runAt time.Time, duration time.Duration, err error) {
	if s == nil || s.opsRepo == nil || err == nil {
		return
	}
	now := time.Now().UTC()
	durMs := duration.Milliseconds()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg := truncateString(err.Error(), 2048)
	_ = s.opsRepo.UpsertJobHeartbeat(ctx, &OpsUpsertJobHeartbeatInput{
		JobName:        accountErrorRateMonitorJobName,
		LastRunAt:      &runAt,
		LastErrorAt:    &now,
		LastError:      &msg,
		LastDurationMs: &durMs,
	})
}
