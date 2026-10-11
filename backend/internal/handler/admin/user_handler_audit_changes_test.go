package admin

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUserChangesAuditExtra(t *testing.T) {
	old, now := 0.3, 0.45
	require.Equal(t, map[string]any{
		"changed":         "status,concurrency,allowed_groups,group_rates",
		"old_status":      "active",
		"old_concurrency": 5,
		"groups_added":    "104",
		"groups_removed":  "101,105",
		"rates_changed":   "101:0.3>,103:>0.45,106:0.3>0.45",
	}, userChangesAuditExtra(service.UserChanges{
		Fields:        []string{"status", "concurrency", "allowed_groups", "group_rates"},
		Old:           map[string]any{"status": "active", "concurrency": 5},
		GroupsAdded:   []int64{104},
		GroupsRemoved: []int64{101, 105},
		Rates: []service.UserGroupRateChange{
			{GroupID: 101, Old: &old},
			{GroupID: 103, New: &now},
			{GroupID: 106, Old: &old, New: &now},
		},
	}))
	// 什么都没变也写 changed，表示这条记录有改前值可比
	require.Equal(t, map[string]any{"changed": ""}, userChangesAuditExtra(service.UserChanges{}))

	// 放不下的分组整项丢掉，不截在 ID 中间
	many := make([]int64, 40)
	for i := range many {
		many[i] = int64(1000 + i)
	}
	listed, ok := userChangesAuditExtra(service.UserChanges{GroupsAdded: many})["groups_added"].(string)
	require.True(t, ok)
	require.LessOrEqual(t, len(listed), 128)
	require.Equal(t, "1000,1001", listed[:9])
	require.Regexp(t, `^(\d{4},)*\d{4}$`, listed)
}

type auditChangesAdminService struct {
	*stubAdminService
	changes service.UserChanges
}

func (s *auditChangesAdminService) UpdateUser(ctx context.Context, id int64, input *service.UpdateUserInput) (*service.User, error) {
	*input.Changes = s.changes
	return s.stubAdminService.UpdateUser(ctx, id, input)
}

type auditCaptureRepo struct {
	mu   sync.Mutex
	logs []*service.AuditLog
}

func (r *auditCaptureRepo) BatchInsert(_ context.Context, logs []*service.AuditLog) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logs...)
	return int64(len(logs)), nil
}
func (r *auditCaptureRepo) Insert(ctx context.Context, log *service.AuditLog) error {
	_, err := r.BatchInsert(ctx, []*service.AuditLog{log})
	return err
}
func (r *auditCaptureRepo) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return &service.AuditLogList{}, nil
}
func (r *auditCaptureRepo) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, service.ErrAuditLogNotFound
}
func (r *auditCaptureRepo) Count(context.Context) (int64, error) { return 0, nil }
func (r *auditCaptureRepo) TruncateAll(context.Context) error    { return nil }
func (r *auditCaptureRepo) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

// 经过真实的审计中间件：新加的键都要在白名单里，否则会被静默丢掉
func TestUpdateUserRecordsChangesInAuditExtra(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &auditCaptureRepo{}
	auditService := service.NewAuditLogService(repo, nil)
	auditService.Start()

	old := 0.3
	h := NewUserHandler(&auditChangesAdminService{stubAdminService: newStubAdminService(), changes: service.UserChanges{
		Fields:        []string{"role", "status", "concurrency", "rpm_limit", "allowed_groups", "group_rates"},
		Old:           map[string]any{"role": "user", "status": "active", "concurrency": 5, "rpm_limit": 60},
		GroupsAdded:   []int64{104},
		GroupsRemoved: []int64{101},
		Rates:         []service.UserGroupRateChange{{GroupID: 101, Old: &old}},
	}}, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
		c.Set(string(middleware.ContextKeyUserRole), "admin")
		c.Next()
	})
	router.Use(gin.HandlerFunc(middleware.NewAuditLogMiddleware(auditService)))
	router.PUT("/api/v1/admin/users/:id", h.Update)

	rec := doJSON(t, router, http.MethodPut, "/api/v1/admin/users/3", map[string]any{"status": "disabled"})
	require.Equal(t, http.StatusOK, rec.Code)
	auditService.Stop()

	repo.mu.Lock()
	defer repo.mu.Unlock()
	require.Len(t, repo.logs, 1)
	extra := repo.logs[0].Extra
	require.Equal(t, "role,status,concurrency,rpm_limit,allowed_groups,group_rates", extra["changed"])
	require.Equal(t, "user", extra["old_role"])
	require.Equal(t, "active", extra["old_status"])
	require.EqualValues(t, 5, extra["old_concurrency"])
	require.EqualValues(t, 60, extra["old_rpm_limit"])
	require.Equal(t, "104", extra["groups_added"])
	require.Equal(t, "101", extra["groups_removed"])
	require.Equal(t, "101:0.3>", extra["rates_changed"])
}
