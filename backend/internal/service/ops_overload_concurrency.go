package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// OpsAccountConcurrencyReaderKey 存放请求级的账号并发读取器，由 ops 错误日志中间件注入。
	OpsAccountConcurrencyReaderKey = "ops_account_concurrency_reader"
	// opsSelectedAccountConcurrencyKey 存放最近一次选中账号的并发配置快照。
	opsSelectedAccountConcurrencyKey = "ops_selected_account_concurrency"

	opsOverloadConcurrencyReadTimeout = time.Second
)

// OpsAccountConcurrencyReader 读取账号在 Redis 里的实时槽位数。ConcurrencyService 实现了它。
type OpsAccountConcurrencyReader interface {
	GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error)
	GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error)
}

type opsSelectedAccountConcurrency struct {
	accountID   int64
	concurrency int
	loadFactor  *int
}

// AccountConcurrencyReader 返回用于过载快照的并发读取器；未配置时返回 nil。
func (s *OpsService) AccountConcurrencyReader() OpsAccountConcurrencyReader {
	if s == nil || s.concurrencyService == nil {
		return nil
	}
	return s.concurrencyService
}

// SetOpsAccountConcurrencyReader 注入并发读取器；reader 为 nil 时不注入。
func SetOpsAccountConcurrencyReader(c *gin.Context, reader OpsAccountConcurrencyReader) {
	if c == nil || reader == nil {
		return
	}
	c.Set(OpsAccountConcurrencyReaderKey, reader)
}

// SetOpsSelectedAccountConcurrency 记录选中账号当时配置的并发上限，供过载事件快照使用。
func SetOpsSelectedAccountConcurrency(c *gin.Context, account *Account) {
	if c == nil || account == nil || account.ID <= 0 {
		return
	}
	snapshot := opsSelectedAccountConcurrency{accountID: account.ID, concurrency: account.Concurrency}
	if account.LoadFactor != nil {
		loadFactor := *account.LoadFactor
		snapshot.loadFactor = &loadFactor
	}
	c.Set(opsSelectedAccountConcurrencyKey, snapshot)
}

// isOpenAIOverloadUpstreamEvent 判断一次 OpenAI 上游失败是否为 server_is_overloaded 类过载。
// rawBody 必须是脱敏前的原文。
func isOpenAIOverloadUpstreamEvent(ev *OpsUpstreamErrorEvent, rawBody string) bool {
	if ev == nil || ev.Platform != PlatformOpenAI || ev.AccountID <= 0 {
		return false
	}
	if isOpenAICapacityShedMessage(ev.Message) || isOpenAICapacityShedMessage(rawBody) {
		return true
	}
	return strings.Contains(strings.ToLower(rawBody), "server_is_overloaded")
}

// attachOpsOverloadConcurrencySnapshot 在 OpenAI 过载事件上记录账号配置的并发上限与
// 此刻的在途/排队数，并打一条日志，用于判断过载是否与本侧并发有关。
// 在途数取自 Redis 槽位，包含当前请求自身占用的槽位。
func attachOpsOverloadConcurrencySnapshot(c *gin.Context, ev *OpsUpstreamErrorEvent, rawBody string) {
	if c == nil || !isOpenAIOverloadUpstreamEvent(ev, rawBody) {
		return
	}
	if v, ok := c.Get(opsSelectedAccountConcurrencyKey); ok {
		if snapshot, ok := v.(opsSelectedAccountConcurrency); ok && snapshot.accountID == ev.AccountID {
			concurrency := snapshot.concurrency
			ev.AccountConcurrency = &concurrency
			ev.AccountLoadFactor = snapshot.loadFactor
		}
	}
	if v, ok := c.Get(OpsAccountConcurrencyReaderKey); ok {
		if reader, ok := v.(OpsAccountConcurrencyReader); ok && reader != nil {
			// 请求可能已被取消，读 Redis 用独立的短超时上下文。
			ctx, cancel := context.WithTimeout(context.Background(), opsOverloadConcurrencyReadTimeout)
			if counts, err := reader.GetAccountConcurrencyBatch(ctx, []int64{ev.AccountID}); err == nil {
				inflight := counts[ev.AccountID]
				ev.AccountInflight = &inflight
			}
			if waiting, err := reader.GetAccountWaitingCount(ctx, ev.AccountID); err == nil {
				ev.AccountWaiting = &waiting
			}
			cancel()
		}
	}

	logCtx := context.Background()
	if c.Request != nil {
		logCtx = c.Request.Context()
	}
	logger.FromContext(logCtx).Info("openai.upstream_overloaded_account_concurrency",
		zap.Int64("account_id", ev.AccountID),
		zap.String("kind", ev.Kind),
		zap.Int("upstream_status_code", ev.UpstreamStatusCode),
		zap.String("upstream_request_id", ev.UpstreamRequestID),
		optionalIntField("account_concurrency", ev.AccountConcurrency),
		optionalIntField("account_load_factor", ev.AccountLoadFactor),
		optionalIntField("account_inflight", ev.AccountInflight),
		optionalIntField("account_waiting", ev.AccountWaiting),
	)
}

func optionalIntField(key string, value *int) zap.Field {
	if value == nil {
		return zap.Skip()
	}
	return zap.Int(key, *value)
}
