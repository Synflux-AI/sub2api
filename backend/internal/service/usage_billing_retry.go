package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// 计费补扣队列（issue #270）。
//
// 扣费事务失败时（典型场景：同一用户的计费事务排队等 users 行锁，等满
// postUsageBillingTimeout 被取消回滚），原先的处理是把 usage log 的 actual_cost 置 0
// 后放过，这笔请求就永久不计费。现在改为把已经算好的 UsageBillingCommand 落到
// usage_billing_retry_queue，由 UsageBillingRetryService 定时重放，保证最终一致。
//
// 重放安全性依赖 repo.Apply 的 (request_id, api_key_id) 幂等键与指纹校验：
// 原事务如果其实已经提交（提交应答丢失），重放会得到 Applied=false，不会重复扣费。
const (
	// DefaultUsageBillingRetryInterval 是补扣队列的默认重放周期，可由
	// billing.retry_queue.interval_seconds 覆盖。
	DefaultUsageBillingRetryInterval = 30 * time.Minute
	// DefaultUsageBillingRetryMaxAttempts 是同一条命令的默认最大重放次数，可由
	// billing.retry_queue.max_attempts 覆盖。用尽后标记 failed，不再自动重试。
	DefaultUsageBillingRetryMaxAttempts = 10
	usageBillingRetryBatchSize          = 100
	// 领取后的租约：期间其它副本不会再领到同一条；进程中途退出的条目
	// 租约到期后由下一轮重新领取。
	usageBillingRetryLease       = 5 * time.Minute
	usageBillingRetryErrorMaxLen = 1024
)

// UsageBillingRetryItem 是补扣队列中一条待重放的扣费命令。
type UsageBillingRetryItem struct {
	ID       int64
	Command  *UsageBillingCommand
	Attempts int
}

// UsageBillingRetryQueue 是补扣队列的持久化接口，由 UsageBillingRepository 的实现可选提供。
type UsageBillingRetryQueue interface {
	// EnqueueUsageBillingRetry 写入一条待重放命令；同一 (request_id, api_key_id) 重复写入是幂等的。
	EnqueueUsageBillingRetry(ctx context.Context, cmd *UsageBillingCommand, cause string) error
	// ClaimDueUsageBillingRetries 领取 next_attempt_at <= dueBefore 的 pending 命令，
	// 并把它们的 next_attempt_at 推后 lease，避免多副本同时重放同一条。
	ClaimDueUsageBillingRetries(ctx context.Context, dueBefore time.Time, limit int, lease time.Duration) ([]UsageBillingRetryItem, error)
	// CompleteUsageBillingRetry 删除已重放成功（或确认已扣过）的命令。
	CompleteUsageBillingRetry(ctx context.Context, id int64) error
	// RescheduleUsageBillingRetry 记录一次失败：failed=false 时保持 pending、下一轮重放；
	// failed=true 表示确定性失败，不再自动重试。
	RescheduleUsageBillingRetry(ctx context.Context, id int64, attempts int, cause string, failed bool) error
}

// isUsageBillingErrorRetryable 判断扣费失败是否值得进入补扣队列。
// 指纹冲突、缺少 request_id 属于确定性失败，重放只会得到同样的结果。
func isUsageBillingErrorRetryable(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, ErrUsageBillingRequestConflict) &&
		!errors.Is(err, ErrUsageBillingRequestIDRequired) &&
		!errors.Is(err, ErrSimpleModeKeyRateLimitBillingUnavailable)
}

// enqueueUsageBillingRetry 尝试把失败的扣费命令转入补扣队列，返回是否入队成功。
// 调用方的计费 ctx 可能已经超时，这里换一个新的 detached 窗口；
// 写入的是独立的窄表，不会再去争 users 行锁。
func enqueueUsageBillingRetry(ctx context.Context, repo UsageBillingRepository, cmd *UsageBillingCommand, billingErr error) bool {
	if cmd == nil || !isUsageBillingErrorRetryable(billingErr) {
		return false
	}
	queue, ok := repo.(UsageBillingRetryQueue)
	if !ok || queue == nil {
		return false
	}
	enqueueCtx, cancel := detachedBillingContext(ctx)
	defer cancel()
	if err := queue.EnqueueUsageBillingRetry(enqueueCtx, cmd, truncateUsageBillingRetryError(billingErr)); err != nil {
		logger.LegacyPrintf("service.usage_billing_retry",
			"ALERT: enqueue usage billing retry failed request_id=%s api_key_id=%d user_id=%d billing_err=%v enqueue_err=%v",
			cmd.RequestID, cmd.APIKeyID, cmd.UserID, billingErr, err)
		return false
	}
	logger.LegacyPrintf("service.usage_billing_retry",
		"Warning: usage billing failed, deferred to retry queue request_id=%s api_key_id=%d user_id=%d balance_cost=%f subscription_cost=%f err=%v",
		cmd.RequestID, cmd.APIKeyID, cmd.UserID, cmd.BalanceCost, cmd.SubscriptionCost, billingErr)
	return true
}

func truncateUsageBillingRetryError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > usageBillingRetryErrorMaxLen {
		msg = msg[:usageBillingRetryErrorMaxLen]
	}
	return msg
}

// UsageBillingRetryService 定时重放补扣队列中的扣费命令。
//
// 每轮把「本轮开始前已到期」的 pending 命令全部重放一遍；仍失败的保持 pending，
// 等下一轮再试。同一条命令重放满 maxAttempts 次仍失败、或遇到确定性失败时，
// 标记 failed 停止自动重试，留给人工对账。
// 多副本安全：领取靠 FOR UPDATE SKIP LOCKED + 租约，重放本身靠计费幂等键兜底。
type UsageBillingRetryService struct {
	queue                UsageBillingRetryQueue
	repo                 UsageBillingRepository
	billingCacheService  *BillingCacheService
	authCacheInvalidator APIKeyAuthCacheInvalidator
	interval             time.Duration
	maxAttempts          int
	now                  func() time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewUsageBillingRetryService(
	repo UsageBillingRepository,
	billingCacheService *BillingCacheService,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
	interval time.Duration,
	maxAttempts int,
) *UsageBillingRetryService {
	queue, _ := repo.(UsageBillingRetryQueue)
	if maxAttempts <= 0 {
		maxAttempts = DefaultUsageBillingRetryMaxAttempts
	}
	return &UsageBillingRetryService{
		queue:                queue,
		repo:                 repo,
		billingCacheService:  billingCacheService,
		authCacheInvalidator: authCacheInvalidator,
		interval:             interval,
		maxAttempts:          maxAttempts,
		now:                  time.Now,
		stopCh:               make(chan struct{}),
	}
}

func (s *UsageBillingRetryService) Start() {
	if s == nil || s.queue == nil || s.repo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		// 启动即跑一轮：发版重启不必再等满一个周期。
		s.runOnce(context.Background())
		for {
			select {
			case <-ticker.C:
				s.runOnce(context.Background())
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *UsageBillingRetryService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

// runOnce 重放本轮开始前已到期的全部命令，返回本轮处理的条数。
//
// 领取条件固定为 next_attempt_at <= 本轮开始时刻：本轮重放失败的条目会被
// 重排到数据库的「现在」，晚于本轮开始时刻，因此不会在同一轮里被反复领取。
// 另外按 id 去重兜底应用与数据库时钟不一致的情况：同一轮内领到已处理过的条目就收尾。
func (s *UsageBillingRetryService) runOnce(ctx context.Context) int {
	defer func() {
		if r := recover(); r != nil {
			logger.LegacyPrintf("service.usage_billing_retry", "ALERT: panic in usage billing retry: %v", r)
		}
	}()

	dueBefore := s.now()
	processed, settled := 0, 0
	seen := make(map[int64]struct{})
	for {
		select {
		case <-s.stopCh:
			// 已领取未处理的条目租约到期后会被重新领取。
			return processed
		default:
		}

		claimCtx, cancel := context.WithTimeout(ctx, postUsageBillingTimeout)
		items, err := s.queue.ClaimDueUsageBillingRetries(claimCtx, dueBefore, usageBillingRetryBatchSize, usageBillingRetryLease)
		cancel()
		if err != nil {
			logger.LegacyPrintf("service.usage_billing_retry", "Warning: claim usage billing retries failed: %v", err)
			break
		}
		fresh := 0
		for _, item := range items {
			if _, dup := seen[item.ID]; dup {
				continue
			}
			seen[item.ID] = struct{}{}
			fresh++
			if s.replay(ctx, item) {
				settled++
			}
		}
		processed += fresh
		if len(items) < usageBillingRetryBatchSize || fresh == 0 {
			break
		}
	}
	if processed > 0 {
		logger.LegacyPrintf("service.usage_billing_retry",
			"usage billing retry round finished processed=%d settled=%d pending=%d", processed, settled, processed-settled)
	}
	return processed
}

// replay 重放一条命令，返回是否已结清（扣费成功或确认已扣过）。
func (s *UsageBillingRetryService) replay(ctx context.Context, item UsageBillingRetryItem) bool {
	cmd := item.Command
	if cmd == nil {
		return false
	}

	applyCtx, cancel := context.WithTimeout(ctx, postUsageBillingTimeout)
	result, applyErr := s.repo.Apply(applyCtx, cmd)
	cancel()

	bookkeepingCtx, bookkeepingCancel := context.WithTimeout(ctx, postUsageBillingTimeout)
	defer bookkeepingCancel()

	if applyErr == nil {
		applied := result != nil && result.Applied
		if applied {
			s.syncCachesAfterReplay(bookkeepingCtx, cmd, result)
		}
		if err := s.queue.CompleteUsageBillingRetry(bookkeepingCtx, item.ID); err != nil {
			// 删除失败只会导致下一轮再重放一次，幂等键保证不会重复扣费。
			logger.LegacyPrintf("service.usage_billing_retry", "Warning: complete usage billing retry failed id=%d request_id=%s: %v", item.ID, cmd.RequestID, err)
		}
		logger.LegacyPrintf("service.usage_billing_retry",
			"usage billing retry settled id=%d request_id=%s api_key_id=%d user_id=%d attempts=%d applied=%t",
			item.ID, cmd.RequestID, cmd.APIKeyID, cmd.UserID, item.Attempts+1, applied)
		return true
	}

	attempts := item.Attempts + 1
	failed := !isUsageBillingErrorRetryable(applyErr) || attempts >= s.maxAttempts
	if err := s.queue.RescheduleUsageBillingRetry(bookkeepingCtx, item.ID, attempts, truncateUsageBillingRetryError(applyErr), failed); err != nil {
		logger.LegacyPrintf("service.usage_billing_retry", "Warning: reschedule usage billing retry failed id=%d request_id=%s: %v", item.ID, cmd.RequestID, err)
	}
	if failed {
		logger.LegacyPrintf("service.usage_billing_retry",
			"ALERT: usage billing retry gave up id=%d request_id=%s api_key_id=%d user_id=%d attempts=%d/%d balance_cost=%f subscription_cost=%f err=%v",
			item.ID, cmd.RequestID, cmd.APIKeyID, cmd.UserID, attempts, s.maxAttempts, cmd.BalanceCost, cmd.SubscriptionCost, applyErr)
		return false
	}
	logger.LegacyPrintf("service.usage_billing_retry",
		"Warning: usage billing retry failed, will retry next round id=%d request_id=%s attempts=%d/%d err=%v",
		item.ID, cmd.RequestID, attempts, s.maxAttempts, applyErr)
	return false
}

// syncCachesAfterReplay 在重放落库后对齐缓存。
//
// 入队时已经按「已扣」更新过余额 / 订阅 / 限速缓存（见 applyUsageBilling），
// 这里只需让余额缓存从数据库重新加载，消除入队到重放之间缓存过期回源读到旧余额的偏差。
// Key 额度在重放时才真正耗尽的话，按用户失效认证缓存（队列里不保存 Key 明文）。
func (s *UsageBillingRetryService) syncCachesAfterReplay(ctx context.Context, cmd *UsageBillingCommand, result *UsageBillingApplyResult) {
	if cmd.BalanceCost > 0 && s.billingCacheService != nil {
		if err := s.billingCacheService.InvalidateUserBalance(ctx, cmd.UserID); err != nil {
			logger.LegacyPrintf("service.usage_billing_retry", "Warning: invalidate balance cache after retry failed user_id=%d: %v", cmd.UserID, err)
		}
	}
	if result.APIKeyQuotaExhausted && s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, cmd.UserID)
	}
}
