package repository

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const accountHealthPrefix = "sched:health:"

// accountHealthApplyScript 原子地完成"读 → 按 ts 衰减 → 加 delta → clamp[0,100] → 写回 + EXPIRE"。
// 时间由调用方传入（Redis Lua 内取时间不可复制安全）。分数回到 100 时删除 key。
var accountHealthApplyScript = redis.NewScript(`
	local key = KEYS[1]
	local delta = tonumber(ARGV[1])
	local now = tonumber(ARGV[2])
	local halflife = tonumber(ARGV[3])
	local ttl = tonumber(ARGV[4])

	local score = 100
	local vals = redis.call('HMGET', key, 'score', 'ts')
	if vals[1] then
		local prev = tonumber(vals[1]) or 100
		local ts = tonumber(vals[2]) or now
		local elapsed = (now - ts) / 1000
		if elapsed > 0 and halflife > 0 then
			score = 100 - (100 - prev) * math.pow(0.5, elapsed / halflife)
		else
			score = prev
		end
	end

	score = score + delta
	if score < 0 then score = 0 end
	if score >= 100 then
		redis.call('DEL', key)
		return '100'
	end

	redis.call('HSET', key, 'score', tostring(score), 'ts', tostring(now))
	redis.call('EXPIRE', key, ttl)
	return tostring(score)
`)

type accountHealthCache struct {
	rdb *redis.Client
}

// NewAccountHealthCache 创建账号健康分缓存实例
func NewAccountHealthCache(rdb *redis.Client) service.AccountHealthCache {
	return &accountHealthCache{rdb: rdb}
}

func accountHealthKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountHealthPrefix, accountID)
}

// ApplyDelta 原子更新账号健康分并返回更新后的分数
func (c *accountHealthCache) ApplyDelta(ctx context.Context, accountID int64, delta float64, halflifeSeconds, ttlSeconds int) (float64, error) {
	if ttlSeconds < 60 {
		ttlSeconds = 60
	}
	nowMs := time.Now().UnixMilli()
	result, err := accountHealthApplyScript.Run(ctx, c.rdb,
		[]string{accountHealthKey(accountID)},
		delta, nowMs, halflifeSeconds, ttlSeconds,
	).Text()
	if err != nil {
		return 0, fmt.Errorf("apply account health delta: %w", err)
	}
	score, err := strconv.ParseFloat(result, 64)
	if err != nil {
		return 0, fmt.Errorf("parse account health score: %w", err)
	}
	return score, nil
}

// GetScoresBatch 用 pipeline 批量读取原始分数记录；不存在的账号不出现在结果中。
func (c *accountHealthCache) GetScoresBatch(ctx context.Context, accountIDs []int64) (map[int64]service.HealthScoreEntry, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.SliceCmd, len(accountIDs))
	for i, accountID := range accountIDs {
		cmds[i] = pipe.HMGet(ctx, accountHealthKey(accountID), "score", "ts")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("batch get account health: %w", err)
	}

	entries := make(map[int64]service.HealthScoreEntry)
	for i, cmd := range cmds {
		vals, err := cmd.Result()
		if err != nil || len(vals) < 2 || vals[0] == nil {
			continue
		}
		scoreStr, ok := vals[0].(string)
		if !ok {
			continue
		}
		score, err := strconv.ParseFloat(scoreStr, 64)
		if err != nil {
			continue
		}
		entry := service.HealthScoreEntry{Score: score}
		if tsStr, ok := vals[1].(string); ok {
			if tsMs, err := strconv.ParseInt(tsStr, 10, 64); err == nil {
				entry.UpdatedAt = time.UnixMilli(tsMs)
			}
		}
		entries[accountIDs[i]] = entry
	}
	return entries, nil
}
