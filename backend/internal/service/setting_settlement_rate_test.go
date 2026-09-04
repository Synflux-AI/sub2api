//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPlatformSettlementRateDefaultsAndPersists(t *testing.T) {
	repo := &bmUpdateRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	require.Equal(t, 6.8, svc.parseSettings(map[string]string{}).PlatformSettlementRate)
	require.NoError(t, svc.UpdateSettings(context.Background(), &SystemSettings{PlatformSettlementRate: 7.1}))
	require.Equal(t, "7.1", repo.updates[SettingKeyPlatformSettlementRate])
}
