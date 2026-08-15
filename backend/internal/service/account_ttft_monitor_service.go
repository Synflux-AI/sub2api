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
)

// AccountTTFTMonitorService 周期性按「账号 × 分组 × 实际上游模型」聚合首 Token 时延，
// 与同模型其余账号的基线比较，把「相对明显变慢」换算成健康分扣分。
//
// 设计要点：
//   - **完全不碰请求热路径**。聚合走 usage_logs 的离线查询，扣分走健康分既有的
//     异步 ApplyDelta 管道，因此不会给网关转发增加任何同步开销。
//   - 观测与扣分是两个开关。TTFTMonitorEnabled 只写快照供面板展示（无调度风险），
//     TTFTDegradeEnabled 才真正扣分。两者均默认关闭，按阶段显式开启。
//   - 运行骨架对标 AccountErrorRateMonitorService：ticker + 分布式 leader lock +
//     job heartbeat。leader lock 保证多实例下只有一个实例扣分，
//     否则同一账号会被扣 N 倍（健康分是共享的 Redis 状态）。
const (
	accountTTFTMonitorJobName = "account_ttft_monitor"

	accountTTFTMonitorTimeout       = 45 * time.Second
	accountTTFTMonitorLeaderLockKey = "sched:ttft:monitor:leader"
	accountTTFTMonitorLeaderLockTTL = 90 * time.Second

	// accountTTFTMonitorStartupDelay 让首轮巡检延后启动，错开进程启动瞬间
	// 多个后台服务同时打 DB 的惊群，也给账号快照缓存留出预热时间。
	accountTTFTMonitorStartupDelay = 45 * time.Second

	// accountTTFTSnapshotTTLFactor 快照 TTL 相对巡检周期的倍数。
	// 取 3 倍：偶尔漏跑一轮不至于让面板上的 TTFT 列整片消失，
	// 但巡检真正停摆时陈旧数据也会在两轮内自然过期，不会长期误导。
	accountTTFTSnapshotTTLFactor = 3
)

// AccountTTFTCache 是 TTFT 快照存储。实现见 repository.NewAccountTTFTCache。
type AccountTTFTCache interface {
	SaveSnapshots(ctx context.Context, snapshots map[int64]*AccountTTFTSnapshot, ttlSeconds int) error
	GetSnapshotsBatch(ctx context.Context, accountIDs []int64) (map[int64]*AccountTTFTSnapshot, error)
}

// accountTTFTRepo 是本服务所需的聚合查询子集（便于测试 stub）。
type accountTTFTRepo interface {
	GetAccountModelTTFT(ctx context.Context, start, end time.Time, minSamples int) ([]OpsAccountModelTTFTRow, error)
}

type AccountTTFTMonitorService struct {
	repo          accountTTFTRepo
	cache         AccountTTFTCache
	healthService *AccountHealthService
	// settingService 提供两个开关的 DB 动态覆盖；为 nil 时回退 config 静态值。
	settingService *SettingService
	opsRepo        OpsRepository

	lockCache  LeaderLockCache
	cfg        *config.Config
	instanceID string

	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup

	skipLogMu sync.Mutex
	skipLogAt time.Time

	warnNoLockOnce sync.Once
}

func NewAccountTTFTMonitorService(
	repo accountTTFTRepo,
	cache AccountTTFTCache,
	healthService *AccountHealthService,
	settingService *SettingService,
	opsRepo OpsRepository,
	lockCache LeaderLockCache,
	cfg *config.Config,
) *AccountTTFTMonitorService {
	return &AccountTTFTMonitorService{
		repo:           repo,
		cache:          cache,
		healthService:  healthService,
		settingService: settingService,
		opsRepo:        opsRepo,
		lockCache:      lockCache,
		cfg:            cfg,
		instanceID:     uuid.NewString(),
	}
}

func (s *AccountTTFTMonitorService) schedulingConfig() *config.GatewaySchedulingConfig {
	if s == nil || s.cfg == nil {
		return nil
	}
	return &s.cfg.Gateway.Scheduling
}

// runtimeToggles 返回两个开关的生效值：settingService 存在时走 DB 动态覆盖，否则回退 config。
func (s *AccountTTFTMonitorService) runtimeToggles() (monitorEnabled, degradeEnabled bool) {
	cfg := s.schedulingConfig()
	if cfg == nil {
		return false, false
	}
	if s.settingService != nil {
		rt := s.settingService.GetSchedulingHealthRuntime(context.Background())
		return rt.TTFTMonitorEnabled, rt.TTFTDegradeEnabled
	}
	return cfg.TTFTMonitorEnabled, cfg.TTFTDegradeEnabled
}

func (s *AccountTTFTMonitorService) interval() time.Duration {
	cfg := s.schedulingConfig()
	if cfg == nil || cfg.TTFTEvalIntervalSeconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(cfg.TTFTEvalIntervalSeconds) * time.Second
}

func (s *AccountTTFTMonitorService) Start() {
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

func (s *AccountTTFTMonitorService) Stop() {
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

func (s *AccountTTFTMonitorService) run() {
	defer s.wg.Done()

	timer := time.NewTimer(accountTTFTMonitorStartupDelay)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			s.runOnceSafely()
			timer.Reset(s.interval())
		case <-s.stopCh:
			return
		}
	}
}

// runOnceSafely 包裹单轮巡检并 recover，确保某一轮的 panic 只丢这一轮，
// 而不会击穿 goroutine 终止整个进程。
func (s *AccountTTFTMonitorService) runOnceSafely() {
	defer func() {
		if r := recover(); r != nil {
			logger.LegacyPrintf("service.account_ttft_monitor", "[AccountTTFTMonitor] evaluate panic recovered: %v", r)
		}
	}()
	s.EvaluateOnce(context.Background())
}

// EvaluateOnce 执行一轮巡检。导出以便测试直接驱动，无需等 ticker。
func (s *AccountTTFTMonitorService) EvaluateOnce(parent context.Context) {
	if s == nil || s.repo == nil {
		return
	}
	cfg := s.schedulingConfig()
	if cfg == nil {
		return
	}
	monitorEnabled, degradeEnabled := s.runtimeToggles()
	if !monitorEnabled {
		return
	}

	ctx, cancel := context.WithTimeout(parent, accountTTFTMonitorTimeout)
	defer cancel()

	release, ok := s.tryAcquireLeaderLock(ctx)
	if !ok {
		return
	}
	if release != nil {
		defer release()
	}

	startedAt := time.Now().UTC()
	// 对齐到分钟边界，避免窗口右端切在正在写入的当前分钟上导致样本抖动。
	end := startedAt.Truncate(time.Minute)
	start := end.Add(-time.Duration(cfg.TTFTWindowMinutes) * time.Minute)

	rows, err := s.repo.GetAccountModelTTFT(ctx, start, end, cfg.TTFTMinSamples)
	if err != nil {
		s.recordHeartbeatError(startedAt, time.Since(startedAt), err)
		logger.LegacyPrintf("service.account_ttft_monitor", "[AccountTTFTMonitor] query failed: %v", err)
		return
	}

	snapshots := AssessAccountTTFT(rows, AccountTTFTEvalConfig{
		MinSamples:          cfg.TTFTMinSamples,
		MinBaselineAccounts: cfg.TTFTMinBaselineAccounts,
		DegradeRatio:        cfg.TTFTDegradeRatio,
		MaxModelsPerAccount: 5,
	}, startedAt)

	degraded := 0
	penalized := 0
	for accountID, snapshot := range snapshots {
		if !snapshot.Degraded {
			continue
		}
		degraded++
		if !degradeEnabled {
			continue
		}
		// 扣分是共享状态的写操作，已被 leader lock 保护为单实例执行。
		s.healthService.RecordTTFTDegradation(accountID, snapshot.Ratio)
		penalized++
	}

	if s.cache != nil && len(snapshots) > 0 {
		ttl := int(s.interval().Seconds()) * accountTTFTSnapshotTTLFactor
		if err := s.cache.SaveSnapshots(ctx, snapshots, ttl); err != nil {
			logger.LegacyPrintf("service.account_ttft_monitor", "[AccountTTFTMonitor] save snapshots failed: %v", err)
		}
	}

	result := fmt.Sprintf("rows=%d accounts=%d degraded=%d penalized=%d window=%dm",
		len(rows), len(snapshots), degraded, penalized, cfg.TTFTWindowMinutes)
	s.recordHeartbeatSuccess(startedAt, time.Since(startedAt), result)

	if degraded > 0 {
		logger.LegacyPrintf("service.account_ttft_monitor",
			"[AccountTTFTMonitor] %s degrade_enabled=%v ratio_threshold=%.2f",
			result, degradeEnabled, cfg.TTFTDegradeRatio)
	}
}

// tryAcquireLeaderLock 获取分布式 leader 锁。未配置锁缓存时直接放行
// （单实例部署没有重复扣分的问题）。
func (s *AccountTTFTMonitorService) tryAcquireLeaderLock(ctx context.Context) (func(), bool) {
	if s.lockCache == nil {
		s.warnNoLockOnce.Do(func() {
			logger.LegacyPrintf("service.account_ttft_monitor", "[AccountTTFTMonitor] leader lock cache not configured; running without distributed lock")
		})
		return nil, true
	}
	key := accountTTFTMonitorLeaderLockKey
	// 锁 TTL 取 max(巡检周期 + 余量, 默认值)，保证锁不会在一轮还没跑完时就过期，
	// 也不会在实例崩溃后长期占用。
	ttl := s.interval() + accountTTFTMonitorTimeout
	if ttl < accountTTFTMonitorLeaderLockTTL {
		ttl = accountTTFTMonitorLeaderLockTTL
	}

	ok, err := s.lockCache.TryAcquireLeaderLock(ctx, key, s.instanceID, ttl)
	if err != nil {
		s.warnNoLockOnce.Do(func() {
			logger.LegacyPrintf("service.account_ttft_monitor", "[AccountTTFTMonitor] leader lock acquire failed; skipping this cycle: %v", err)
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
		_ = s.lockCache.ReleaseLeaderLock(releaseCtx, key, s.instanceID)
	}, true
}

func (s *AccountTTFTMonitorService) maybeLogSkip(key string) {
	s.skipLogMu.Lock()
	defer s.skipLogMu.Unlock()
	now := time.Now()
	if !s.skipLogAt.IsZero() && now.Sub(s.skipLogAt) < time.Minute {
		return
	}
	s.skipLogAt = now
	logger.LegacyPrintf("service.account_ttft_monitor", "[AccountTTFTMonitor] leader lock held by another instance; skipping (key=%q)", key)
}

func (s *AccountTTFTMonitorService) recordHeartbeatSuccess(runAt time.Time, duration time.Duration, result string) {
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
		JobName:        accountTTFTMonitorJobName,
		LastRunAt:      &runAt,
		LastSuccessAt:  &now,
		LastDurationMs: &durMs,
		LastResult:     &msg,
	})
}

func (s *AccountTTFTMonitorService) recordHeartbeatError(runAt time.Time, duration time.Duration, err error) {
	if s == nil || s.opsRepo == nil || err == nil {
		return
	}
	now := time.Now().UTC()
	durMs := duration.Milliseconds()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg := truncateString(err.Error(), 2048)
	_ = s.opsRepo.UpsertJobHeartbeat(ctx, &OpsUpsertJobHeartbeatInput{
		JobName:        accountTTFTMonitorJobName,
		LastRunAt:      &runAt,
		LastErrorAt:    &now,
		LastError:      &msg,
		LastDurationMs: &durMs,
	})
}
