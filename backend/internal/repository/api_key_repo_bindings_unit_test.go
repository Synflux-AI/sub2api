package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// bindingSQLRecorder 是一个"永远匹配并顺序记录"的 sqlmock QueryMatcher。
// 断言对象是被记录下来的 SQL 文本本身，因此不需要在期望里重复写一遍正则。
type bindingSQLRecorder struct {
	statements *[]string
}

func (m bindingSQLRecorder) Match(_, actual string) error {
	*m.statements = append(*m.statements, actual)
	return nil
}

func newAPIKeyRepoSQLMockForBindings(t *testing.T) (*apiKeyRepository, sqlmock.Sqlmock, *[]string) {
	t.Helper()

	statements := &[]string{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(bindingSQLRecorder{statements: statements}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })

	return newAPIKeyRepositoryWithSQL(client, db), mock, statements
}

func uniqueViolation() error {
	return &pq.Error{Code: "23505", Constraint: "idx_api_key_groups_key_platform"}
}

// --- ReplaceBindings：整体替换语义 ---

func TestReplaceBindingsDeletesEveryRowBeforeInserting(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("delete all bindings").
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 3))
	// 插入顺序固定为 (platform, group_id) 升序，与 service.SortGroupBindings 一致。
	mock.ExpectExec("insert bindings").
		WithArgs(
			int64(7), sqlmock.AnyArg(), int64(2), "anthropic",
			int64(7), sqlmock.AnyArg(), int64(3), "openai",
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	err := repo.ReplaceBindings(context.Background(), 7, []service.GroupBinding{
		{GroupID: 3, Platform: "openai"},
		{GroupID: 2, Platform: "anthropic"},
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Len(t, *statements, 2, "整体替换应当恰好是一条 DELETE + 一条批量 INSERT")

	del := (*statements)[0]
	require.Contains(t, del, `DELETE FROM "api_key_groups"`)
	require.Contains(t, del, `"api_key_groups"."api_key_id" = $1`)
	// 关键不变量：DELETE 只按 api_key_id 过滤，不带 group_id / platform 条件，
	// 否则残留的（指向已软删分组的）绑定行不会被清掉，后续插入会撞 23505。
	require.NotContains(t, del, "group_id")
	require.NotContains(t, del, "platform")

	ins := (*statements)[1]
	require.Contains(t, ins, `INSERT INTO "api_key_groups"`)
	// 禁止退化成 upsert / 增量合并。
	require.NotContains(t, ins, "ON CONFLICT")
}

func TestReplaceBindingsWithEmptySetClearsAllRows(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("delete all bindings").
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	require.NoError(t, repo.ReplaceBindings(context.Background(), 7, nil))
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, *statements, 1, "空集合只应产生 DELETE，不应有 INSERT")
}

func TestReplaceBindingsTranslatesUniqueViolationAndRollsBack(t *testing.T) {
	repo, mock, _ := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("delete all bindings").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("insert bindings").WillReturnError(uniqueViolation())
	mock.ExpectRollback()

	err := repo.ReplaceBindings(context.Background(), 7, []service.GroupBinding{
		{GroupID: 2, Platform: "anthropic"},
		{GroupID: 3, Platform: "anthropic"},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrAPIKeyGroupBindingConflict)
	require.NoError(t, mock.ExpectationsWereMet(), "唯一冲突必须回滚，不能留下半个替换")
}

func TestReplaceBindingsPropagatesDeleteFailureWithoutInserting(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("delete all bindings").WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	err := repo.ReplaceBindings(context.Background(), 7, []service.GroupBinding{
		{GroupID: 2, Platform: "anthropic"},
	})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, *statements, 1, "DELETE 失败后不得继续 INSERT")
}

func TestReplaceBindingsReusesAmbientTransaction(t *testing.T) {
	repo, mock, _ := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("delete all bindings").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("insert bindings").WillReturnResult(sqlmock.NewResult(0, 1))
	// 只有测试自己发起的 Rollback，没有第二次 BEGIN / COMMIT ——
	// 证明 ReplaceBindings 复用了 ctx 上的事务而不是另开一个。
	mock.ExpectRollback()

	tx, err := repo.client.Tx(context.Background())
	require.NoError(t, err)
	ctx := dbent.NewTxContext(context.Background(), tx)

	require.NoError(t, repo.ReplaceBindings(ctx, 7, []service.GroupBinding{{GroupID: 2, Platform: "anthropic"}}))
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- Create：主表 + 关联表同事务（C11）---

func TestCreateWritesMainRowAndBindingsInSingleTransaction(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectQuery("insert api_keys").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(11)))
	mock.ExpectExec("delete all bindings").
		WithArgs(int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("insert bindings").
		WithArgs(int64(11), sqlmock.AnyArg(), int64(2), "anthropic").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	groupID := int64(2)
	key := &service.APIKey{
		UserID:      1,
		Key:         "sk-create-bindings",
		Name:        "k",
		Status:      service.StatusActive,
		GroupID:     &groupID,
		BoundGroups: []*service.Group{{ID: 2, Platform: service.PlatformAnthropic}},
	}
	require.NoError(t, repo.Create(context.Background(), key))
	require.Equal(t, int64(11), key.ID)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Len(t, *statements, 3)
	require.Contains(t, (*statements)[0], `INSERT INTO "api_keys"`)
	require.Contains(t, (*statements)[1], `DELETE FROM "api_key_groups"`)
	require.Contains(t, (*statements)[2], `INSERT INTO "api_key_groups"`)
}

func TestCreateRollsBackMainRowWhenBindingWriteFails(t *testing.T) {
	repo, mock, _ := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectQuery("insert api_keys").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(11)))
	mock.ExpectExec("delete all bindings").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("insert bindings").WillReturnError(errors.New("binding write exploded"))
	// 没有 COMMIT：主表插入必须随关联表失败一起回滚，不留中间态。
	mock.ExpectRollback()

	key := &service.APIKey{
		UserID:      1,
		Key:         "sk-create-rollback",
		Name:        "k",
		Status:      service.StatusActive,
		BoundGroups: []*service.Group{{ID: 2, Platform: service.PlatformAnthropic}},
	}
	err := repo.Create(context.Background(), key)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCreateWithoutBoundGroupsSkipsTransaction(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	// 不设置 ExpectBegin：一旦 Create 无谓地开事务，sqlmock 会报"未预期的 Begin"。
	mock.ExpectQuery("insert api_keys").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(12)))

	key := &service.APIKey{UserID: 1, Key: "sk-no-bindings", Name: "k", Status: service.StatusActive}
	require.NoError(t, repo.Create(context.Background(), key))
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, *statements, 1)
}

func TestCreateReusesAmbientTransaction(t *testing.T) {
	repo, mock, _ := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectQuery("insert api_keys").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(13)))
	mock.ExpectExec("delete all bindings").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("insert bindings").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	tx, err := repo.client.Tx(context.Background())
	require.NoError(t, err)
	ctx := dbent.NewTxContext(context.Background(), tx)

	key := &service.APIKey{
		UserID:      1,
		Key:         "sk-ambient",
		Name:        "k",
		Status:      service.StatusActive,
		BoundGroups: []*service.Group{{ID: 2, Platform: service.PlatformAnthropic}},
	}
	require.NoError(t, repo.Create(ctx, key))
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- Update：主表 + 关联表同事务（C11）---

func TestUpdateWritesMainRowAndBindingsInSingleTransaction(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("update api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("delete all bindings").
		WithArgs(int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("insert bindings").
		WithArgs(
			int64(9), sqlmock.AnyArg(), int64(4), "anthropic",
			int64(9), sqlmock.AnyArg(), int64(5), "openai",
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	groupID := int64(4)
	key := &service.APIKey{
		ID:      9,
		GroupID: &groupID,
		BoundGroups: []*service.Group{
			{ID: 5, Platform: service.PlatformOpenAI},
			{ID: 4, Platform: service.PlatformAnthropic},
		},
	}
	err := repo.Update(context.Background(), key, service.APIKeyUpdateFields{GroupID: true, BoundGroups: true})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Len(t, *statements, 3)
	require.Contains(t, (*statements)[0], `UPDATE "api_keys"`)
	require.Contains(t, (*statements)[0], `"group_id" = $`)
	require.Contains(t, (*statements)[1], `DELETE FROM "api_key_groups"`)
	require.Contains(t, (*statements)[2], `INSERT INTO "api_key_groups"`)
}

func TestUpdateRollsBackMainRowWhenBindingWriteFails(t *testing.T) {
	repo, mock, _ := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("update api_keys").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("delete all bindings").WillReturnError(errors.New("binding delete exploded"))
	mock.ExpectRollback()

	key := &service.APIKey{
		ID:          9,
		Name:        "renamed",
		BoundGroups: []*service.Group{{ID: 4, Platform: service.PlatformAnthropic}},
	}
	err := repo.Update(context.Background(), key, service.APIKeyUpdateFields{Name: true, BoundGroups: true})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet(), "关联表写入失败时主表更新必须一起回滚")
}

func TestUpdateSkipsBindingWriteAndTransactionWhenMaskNotSet(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	// 只置位 GroupID：只写 api_keys.group_id 这一列，不碰关联表、不开事务。
	mock.ExpectExec("update api_keys").WillReturnResult(sqlmock.NewResult(0, 1))

	groupID := int64(4)
	key := &service.APIKey{ID: 9, GroupID: &groupID}
	require.NoError(t, repo.Update(context.Background(), key, service.APIKeyUpdateFields{GroupID: true}))
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, *statements, 1)
	require.NotContains(t, (*statements)[0], "api_key_groups")
}

func TestUpdateNotFoundSkipsBindingWrite(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("update api_keys").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	key := &service.APIKey{
		ID:          9,
		Name:        "renamed",
		BoundGroups: []*service.Group{{ID: 4, Platform: service.PlatformAnthropic}},
	}
	err := repo.Update(context.Background(), key, service.APIKeyUpdateFields{Name: true, BoundGroups: true})
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, *statements, 1, "主表未命中时不应改动关联表")
}

// --- 反查与筛选：默认组 UNION 关联表 ---

func TestListKeysByGroupIDCoversDefaultPointerAndAssociationTable(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectQuery("list keys by group").
		WithArgs(int64(42), int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"key"}).AddRow("sk-a").AddRow("sk-b"))

	keys, err := repo.ListKeysByGroupID(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, []string{"sk-a", "sk-b"}, keys)
	require.NoError(t, mock.ExpectationsWereMet())

	require.Len(t, *statements, 1)
	sql := (*statements)[0]
	require.Contains(t, sql, `"api_keys"."group_id" = $1`)
	require.Contains(t, sql, `EXISTS (SELECT "api_key_groups"."api_key_id" FROM "api_key_groups"`)
	require.Contains(t, sql, `"api_key_groups"."group_id" = $2`)
	// 反查不 join groups：分组即使已软删也要命中，才能失效这些 Key 的认证缓存。
	require.NotContains(t, sql, `"groups"`)
}

func TestListKeyIDsByBoundGroupIDCoversDefaultPointerAndAssociationTable(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectQuery("list key ids by group").
		WithArgs(int64(42), int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(3)).AddRow(int64(8)))

	ids, err := repo.ListKeyIDsByBoundGroupID(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, []int64{3, 8}, ids)
	require.NoError(t, mock.ExpectationsWereMet())

	sql := (*statements)[0]
	require.Contains(t, sql, `"api_keys"."group_id" = $1`)
	require.Contains(t, sql, `FROM "api_key_groups"`)
	require.Contains(t, sql, `ORDER BY "api_keys"."id" ASC`)
}

func TestListByUserIDGroupFilterMatchesBindingSetMembership(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("page").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	groupID := int64(42)
	_, _, err := repo.ListByUserID(context.Background(), 5,
		pagination.PaginationParams{Page: 1, PageSize: 10},
		service.APIKeyListFilters{GroupID: &groupID})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	for _, sql := range *statements {
		require.Contains(t, sql, `"api_keys"."group_id" = $2`)
		require.Contains(t, sql, `FROM "api_key_groups"`)
		require.Contains(t, sql, `"api_key_groups"."group_id" = $3`)
	}
}

func TestListByUserIDGroupFilterZeroMeansNoBindingAtAll(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("page").WillReturnRows(sqlmock.NewRows([]string{"id"}))

	groupID := int64(0)
	_, _, err := repo.ListByUserID(context.Background(), 5,
		pagination.PaginationParams{Page: 1, PageSize: 10},
		service.APIKeyListFilters{GroupID: &groupID})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	for _, sql := range *statements {
		require.Contains(t, sql, `"api_keys"."group_id" IS NULL`)
		require.Contains(t, sql, `NOT (EXISTS (SELECT "api_key_groups"."api_key_id" FROM "api_key_groups"`)
	}
}

func TestListBoundGroupIDsReturnsRawAssociationRows(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectQuery("bound group ids").
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(int64(2)).AddRow(int64(9)))

	ids, err := repo.ListBoundGroupIDs(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, []int64{2, 9}, ids)
	require.NoError(t, mock.ExpectationsWereMet())

	sql := (*statements)[0]
	require.Contains(t, sql, `SELECT "api_key_groups"."group_id" FROM "api_key_groups"`)
	require.Contains(t, sql, `ORDER BY "api_key_groups"."group_id" ASC`)
	// 原始绑定集合：不 join groups，也就不过滤 groups.deleted_at。
	require.NotContains(t, sql, `"groups"`)
	require.NotContains(t, sql, "deleted_at")
}

func TestDeleteBindingsByGroupIDRemovesEveryRowOfThatGroup(t *testing.T) {
	repo, mock, statements := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectExec("delete bindings by group").
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := repo.DeleteBindingsByGroupID(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, int64(3), n)
	require.NoError(t, mock.ExpectationsWereMet())

	sql := (*statements)[0]
	require.Contains(t, sql, `DELETE FROM "api_key_groups"`)
	require.Contains(t, sql, `"api_key_groups"."group_id" = $1`)
}

func TestDeleteBindingsByGroupIDUsesAmbientTransaction(t *testing.T) {
	repo, mock, _ := newAPIKeyRepoSQLMockForBindings(t)

	mock.ExpectBegin()
	mock.ExpectExec("delete bindings by group").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	tx, err := repo.client.Tx(context.Background())
	require.NoError(t, err)
	ctx := dbent.NewTxContext(context.Background(), tx)

	n, err := repo.DeleteBindingsByGroupID(ctx, 42)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- boundGroupsFromEntity：读路径组装（纯内存，无 DB）---

func TestBoundGroupsFromEntityMergesDefaultGroupAndSortsStably(t *testing.T) {
	m := &dbent.APIKey{ID: 1}
	m.Edges.Group = &dbent.Group{ID: 5, Name: "default-openai", Platform: service.PlatformOpenAI}
	m.Edges.BoundGroups = []*dbent.Group{
		{ID: 9, Name: "anthropic-b", Platform: service.PlatformAnthropic},
		{ID: 5, Name: "default-openai", Platform: service.PlatformOpenAI}, // 与默认组重复
		{ID: 4, Name: "anthropic-a", Platform: service.PlatformAnthropic},
	}

	got := boundGroupsFromEntity(m)
	require.Len(t, got, 3, "默认组与关联行重复时必须去重")
	// 先按 platform 字典序，再按 group id 升序。
	require.Equal(t, int64(4), got[0].ID)
	require.Equal(t, int64(9), got[1].ID)
	require.Equal(t, int64(5), got[2].ID)
	require.Equal(t, service.PlatformAnthropic, got[0].Platform)
	require.Equal(t, service.PlatformOpenAI, got[2].Platform)
}

func TestBoundGroupsFromEntityDropsSoftDeletedGroups(t *testing.T) {
	deletedAt := time.Now()
	m := &dbent.APIKey{ID: 1}
	m.Edges.BoundGroups = []*dbent.Group{
		{ID: 4, Platform: service.PlatformAnthropic},
		{ID: 7, Platform: service.PlatformOpenAI, DeletedAt: &deletedAt},
	}

	got := boundGroupsFromEntity(m)
	require.Len(t, got, 1, "指向已软删分组的残留绑定行在读路径必须被剔除")
	require.Equal(t, int64(4), got[0].ID)
}

func TestBoundGroupsFromEntityReturnsNilWhenEdgesNotLoaded(t *testing.T) {
	require.Nil(t, boundGroupsFromEntity(&dbent.APIKey{ID: 1}))
}

func TestBoundGroupsFromEntityFallsBackToDefaultGroupOnly(t *testing.T) {
	m := &dbent.APIKey{ID: 1}
	m.Edges.Group = &dbent.Group{ID: 5, Platform: service.PlatformOpenAI}

	got := boundGroupsFromEntity(m)
	require.Len(t, got, 1, "只 WithGroup() 的查询也应把默认组当作绑定集合成员")
	require.Equal(t, int64(5), got[0].ID)
}

// --- service 层派生辅助 ---

func TestGroupBindingsFromGroupsDedupesAndSorts(t *testing.T) {
	bindings := service.GroupBindingsFromGroups([]*service.Group{
		{ID: 9, Platform: service.PlatformOpenAI},
		nil,
		{ID: 4, Platform: service.PlatformAnthropic},
		{ID: 9, Platform: service.PlatformOpenAI},
	})
	require.Equal(t, []service.GroupBinding{
		{GroupID: 4, Platform: service.PlatformAnthropic},
		{GroupID: 9, Platform: service.PlatformOpenAI},
	}, bindings)
}

func TestGroupBindingsFromGroupsEmptyInputYieldsNil(t *testing.T) {
	require.Nil(t, service.GroupBindingsFromGroups(nil))
	require.Nil(t, service.GroupBindingsFromGroups([]*service.Group{}))
}
