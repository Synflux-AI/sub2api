package admin

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const maxMultiFilterValues = 100

func parsePositiveIDListQuery(c *gin.Context, name string) ([]int64, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, nil
	}
	seen := make(map[int64]struct{})
	ids := make([]int64, 0)
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid %s", name)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) > maxMultiFilterValues {
			return nil, fmt.Errorf("too many %s values", name)
		}
	}
	return ids, nil
}

func parseStringListQuery(c *gin.Context, name string) ([]string, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, fmt.Errorf("invalid %s", name)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
		if len(values) > maxMultiFilterValues {
			return nil, fmt.Errorf("too many %s values", name)
		}
	}
	return values, nil
}

func applyUsageMultiFilters(c *gin.Context, filters *usagestats.UsageLogFilters) error {
	var err error
	if filters.UserIDs, err = parsePositiveIDListQuery(c, "user_ids"); err != nil {
		return err
	}
	if filters.AccountIDs, err = parsePositiveIDListQuery(c, "account_ids"); err != nil {
		return err
	}
	if filters.GroupIDs, err = parsePositiveIDListQuery(c, "group_ids"); err != nil {
		return err
	}
	filters.Models, err = parseStringListQuery(c, "models")
	return err
}

func applyOpsLogMultiFilters(c *gin.Context, filter *service.OpsErrorLogFilter) error {
	var err error
	if filter.UserIDs, err = parsePositiveIDListQuery(c, "user_ids"); err != nil {
		return err
	}
	if filter.AccountIDs, err = parsePositiveIDListQuery(c, "account_ids"); err != nil {
		return err
	}
	if filter.GroupIDs, err = parsePositiveIDListQuery(c, "group_ids"); err != nil {
		return err
	}
	filter.Models, err = parseStringListQuery(c, "models")
	return err
}

func applyOpsDashboardMultiFilters(c *gin.Context, filter *service.OpsDashboardFilter) error {
	var err error
	if filter.UserIDs, err = parsePositiveIDListQuery(c, "user_ids"); err != nil {
		return err
	}
	if filter.AccountIDs, err = parsePositiveIDListQuery(c, "account_ids"); err != nil {
		return err
	}
	if filter.GroupIDs, err = parsePositiveIDListQuery(c, "group_ids"); err != nil {
		return err
	}
	filter.Models, err = parseStringListQuery(c, "models")
	return err
}
