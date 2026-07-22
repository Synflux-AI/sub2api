package admin

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

var groupUsageSummaryCache = newSnapshotCache(30 * time.Second)

func (h *GroupHandler) getGroupUsageSummaryCached(ctx context.Context, todayStart time.Time) ([]usagestats.GroupUsageSummary, bool, error) {
	key := todayStart.UTC().Format(time.RFC3339Nano)
	entry, hit, err := groupUsageSummaryCache.GetOrLoad(key, func() (any, error) {
		return h.dashboardService.GetGroupUsageSummary(ctx, todayStart)
	})
	if err != nil {
		return nil, hit, err
	}
	result, err := snapshotPayloadAs[[]usagestats.GroupUsageSummary](entry.Payload)
	return result, hit, err
}
