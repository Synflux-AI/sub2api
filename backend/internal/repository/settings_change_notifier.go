package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const settingsUpdatedChannel = "settings_updated"

type settingsChangeNotifier struct {
	rdb        *redis.Client
	instanceID string
}

// NewSettingsChangeNotifier broadcasts settings saves over Redis pub/sub.
// Each process tags its own messages so it does not reload on its own save.
func NewSettingsChangeNotifier(rdb *redis.Client) service.SettingsChangeNotifier {
	if rdb == nil {
		return nil
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("generate settings notifier instance id: %v", err))
	}
	return &settingsChangeNotifier{rdb: rdb, instanceID: hex.EncodeToString(buf)}
}

func (n *settingsChangeNotifier) PublishSettingsUpdated(ctx context.Context) error {
	return n.rdb.Publish(ctx, settingsUpdatedChannel, n.instanceID).Err()
}

func (n *settingsChangeNotifier) SubscribeSettingsUpdates(ctx context.Context, handler func()) error {
	pubsub := n.rdb.Subscribe(ctx, settingsUpdatedChannel)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return fmt.Errorf("subscribe to settings updates: %w", err)
	}

	go func() {
		defer func() {
			if err := pubsub.Close(); err != nil {
				slog.Warn("close settings update pubsub failed", "error", err)
			}
		}()
		ch := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				if msg == nil || msg.Payload == n.instanceID {
					continue
				}
				handler()
			}
		}
	}()
	return nil
}
