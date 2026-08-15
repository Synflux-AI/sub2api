package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const accountTTFTPrefix = "sched:ttft:"

// accountTTFTCache 存放 TTFT 巡检每轮算出的账号快照。
//
// 与健康分不同，这里不需要原子读改写：快照由单一 leader 实例整体覆盖写，
// 读侧（账号列表接口）只做批量读。因此用最简单的 SET + TTL，值为 JSON。
// TTL 由写入方按巡检周期给出，过期即视为「本轮没有数据」，
// 避免巡检停摆后 UI 仍展示陈旧的 TTFT 判定。
type accountTTFTCache struct {
	rdb *redis.Client
}

// NewAccountTTFTCache 创建账号 TTFT 快照缓存实例。
func NewAccountTTFTCache(rdb *redis.Client) service.AccountTTFTCache {
	return &accountTTFTCache{rdb: rdb}
}

func accountTTFTKey(accountID int64) string {
	return fmt.Sprintf("%s%d", accountTTFTPrefix, accountID)
}

// SaveSnapshots 用 pipeline 批量覆盖写快照。单个账号序列化失败不影响其余账号。
func (c *accountTTFTCache) SaveSnapshots(ctx context.Context, snapshots map[int64]*service.AccountTTFTSnapshot, ttlSeconds int) error {
	if c == nil || c.rdb == nil || len(snapshots) == 0 {
		return nil
	}
	if ttlSeconds < 60 {
		ttlSeconds = 60
	}
	ttl := time.Duration(ttlSeconds) * time.Second

	pipe := c.rdb.Pipeline()
	queued := 0
	for accountID, snapshot := range snapshots {
		if accountID <= 0 || snapshot == nil {
			continue
		}
		payload, err := json.Marshal(snapshot)
		if err != nil {
			continue
		}
		pipe.Set(ctx, accountTTFTKey(accountID), payload, ttl)
		queued++
	}
	if queued == 0 {
		return nil
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return fmt.Errorf("save account ttft snapshots: %w", err)
	}
	return nil
}

// GetSnapshotsBatch 批量读取快照；不存在或解析失败的账号不出现在结果中。
func (c *accountTTFTCache) GetSnapshotsBatch(ctx context.Context, accountIDs []int64) (map[int64]*service.AccountTTFTSnapshot, error) {
	if c == nil || c.rdb == nil || len(accountIDs) == 0 {
		return nil, nil
	}
	pipe := c.rdb.Pipeline()
	cmds := make([]*redis.StringCmd, len(accountIDs))
	for i, accountID := range accountIDs {
		cmds[i] = pipe.Get(ctx, accountTTFTKey(accountID))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("batch get account ttft: %w", err)
	}

	out := make(map[int64]*service.AccountTTFTSnapshot)
	for i, cmd := range cmds {
		payload, err := cmd.Bytes()
		if err != nil {
			continue
		}
		var snapshot service.AccountTTFTSnapshot
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			continue
		}
		out[accountIDs[i]] = &snapshot
	}
	return out, nil
}
