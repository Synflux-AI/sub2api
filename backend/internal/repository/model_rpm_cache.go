package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// 模型维度 RPM 计数器 Redis 实现。
//
// 设计说明：
//   - key 形式：rpm:mu:{ruleID}:{userID}:{minute}（scope=user）、rpm:mg:{ruleID}:{minute}（scope=global）
//   - 用 ruleID 而非模型名做 key：模型名含 `[1m]`、`/` 等字符；通配规则也没有单一模型名可用；
//     用 ruleID 还能让「改限额」立即生效，而「改模型匹配」自然换桶。
//   - 时间来源：rdb.Time()（Redis 服务端时间），与 user_rpm_cache 一致，避免多实例时钟漂移。
//   - 原子操作：TxPipeline (MULTI/EXEC) 执行 INCR+EXPIRE，兼容 Redis Cluster。
//   - TTL：120s，覆盖当前分钟窗口 + 少量冗余。
const (
	modelRuleUserRPMKeyPrefix   = "rpm:mu:"
	modelRuleGlobalRPMKeyPrefix = "rpm:mg:"

	modelRPMKeyTTL = 120 * time.Second
)

type modelRPMCacheImpl struct {
	rdb *redis.Client
}

// NewModelRPMCache 创建模型维度 RPM 计数器。
func NewModelRPMCache(rdb *redis.Client) service.ModelRPMCache {
	return &modelRPMCacheImpl{rdb: rdb}
}

// MinuteTimestamp 返回 Redis 服务端当前分钟戳。
func (c *modelRPMCacheImpl) MinuteTimestamp(ctx context.Context) (int64, error) {
	t, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return 0, fmt.Errorf("redis TIME: %w", err)
	}
	return t.Unix() / 60, nil
}

// modelRPMKey 拼装计数键；userID <= 0 走全局桶。
func modelRPMKey(ruleID, userID, minute int64) string {
	if userID <= 0 {
		return fmt.Sprintf("%s%d:%d", modelRuleGlobalRPMKeyPrefix, ruleID, minute)
	}
	return fmt.Sprintf("%s%d:%d:%d", modelRuleUserRPMKeyPrefix, ruleID, userID, minute)
}

// IncrementRuleRPM 原子 INCR+EXPIRE 指定规则在给定分钟窗口的计数。
func (c *modelRPMCacheImpl) IncrementRuleRPM(ctx context.Context, ruleID, userID, minute int64) (int, error) {
	key := modelRPMKey(ruleID, userID, minute)
	pipe := c.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, modelRPMKeyTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("model rpm increment: %w", err)
	}
	return int(incr.Val()), nil
}

// GetRuleRPM 读取指定规则当前分钟的已用计数（只读）。
func (c *modelRPMCacheImpl) GetRuleRPM(ctx context.Context, ruleID, userID, minute int64) (int, error) {
	val, err := c.rdb.Get(ctx, modelRPMKey(ruleID, userID, minute)).Int()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("model rpm get: %w", err)
	}
	return val, nil
}
