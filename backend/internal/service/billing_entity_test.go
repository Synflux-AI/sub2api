//go:build unit

package service

import (
	"context"
	"database/sql"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

func TestBillingEntityServiceCRUDAndDeleteProtection(t *testing.T) {
	db, err := sql.Open("sqlite", "file:billing_entity_service?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	svc := NewBillingEntityService(client)
	entity, err := svc.Create(ctx, CreateBillingEntityInput{Name: "  LinkyRouter 上海主体  ", Currency: "cny"})
	require.NoError(t, err)
	require.Equal(t, "LinkyRouter 上海主体", entity.Name)
	require.Equal(t, "CNY", entity.Currency)

	user, err := client.User.Create().SetEmail("billing@example.com").SetPasswordHash("hash").SetBillingEntityID(entity.ID).Save(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, svc.Delete(ctx, entity.ID), ErrBillingEntityInUse)

	_, err = client.User.UpdateOneID(user.ID).ClearBillingEntityID().Save(ctx)
	require.NoError(t, err)
	require.NoError(t, svc.Delete(ctx, entity.ID))
}
