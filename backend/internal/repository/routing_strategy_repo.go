package repository

import (
	"context"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/routingstrategy"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type routingStrategyRepository struct {
	client *dbent.Client
}

// NewRoutingStrategyRepository 创建路由策略仓储。
func NewRoutingStrategyRepository(client *dbent.Client) service.RoutingStrategyRepository {
	return &routingStrategyRepository{client: client}
}

func (r *routingStrategyRepository) Create(ctx context.Context, s *service.RoutingStrategy) error {
	client := clientFromContext(ctx, r.client)
	builder := client.RoutingStrategy.Create().
		SetName(s.Name).
		SetDescription(s.Description).
		SetEnabled(s.Enabled).
		SetPriority(s.Priority).
		SetPlatform(s.Platform).
		SetMatchMode(s.MatchMode).
		SetConditions(s.Conditions).
		SetAction(s.Action).
		SetAccountIds(s.AccountIDs).
		SetAccountPriorities(s.AccountPriorities)
	if s.GroupID != nil {
		builder.SetGroupID(*s.GroupID)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return err
	}
	applyRoutingStrategyEntity(s, created)
	return nil
}

func (r *routingStrategyRepository) GetByID(ctx context.Context, id int64) (*service.RoutingStrategy, error) {
	m, err := r.client.RoutingStrategy.Query().
		Where(routingstrategy.IDEQ(id)).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrRoutingStrategyNotFound, nil)
	}
	return routingStrategyEntityToService(m), nil
}

func (r *routingStrategyRepository) Update(ctx context.Context, s *service.RoutingStrategy) error {
	client := clientFromContext(ctx, r.client)
	builder := client.RoutingStrategy.UpdateOneID(s.ID).
		SetName(s.Name).
		SetDescription(s.Description).
		SetEnabled(s.Enabled).
		SetPriority(s.Priority).
		SetPlatform(s.Platform).
		SetMatchMode(s.MatchMode).
		SetConditions(s.Conditions).
		SetAction(s.Action).
		SetAccountIds(s.AccountIDs).
		SetAccountPriorities(s.AccountPriorities)
	if s.GroupID != nil {
		builder.SetGroupID(*s.GroupID)
	} else {
		builder.ClearGroupID()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrRoutingStrategyNotFound, nil)
	}
	s.UpdatedAt = updated.UpdatedAt
	return nil
}

func (r *routingStrategyRepository) Delete(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.RoutingStrategy.Delete().Where(routingstrategy.IDEQ(id)).Exec(ctx)
	return err
}

func (r *routingStrategyRepository) List(ctx context.Context) ([]service.RoutingStrategy, error) {
	items, err := r.client.RoutingStrategy.Query().
		Order(dbent.Asc(routingstrategy.FieldPriority), dbent.Asc(routingstrategy.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return routingStrategyEntitiesToService(items), nil
}

func (r *routingStrategyRepository) ListEnabled(ctx context.Context) ([]service.RoutingStrategy, error) {
	items, err := r.client.RoutingStrategy.Query().
		Where(routingstrategy.EnabledEQ(true)).
		Order(dbent.Asc(routingstrategy.FieldPriority), dbent.Asc(routingstrategy.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return routingStrategyEntitiesToService(items), nil
}

func applyRoutingStrategyEntity(dst *service.RoutingStrategy, src *dbent.RoutingStrategy) {
	if dst == nil || src == nil {
		return
	}
	dst.ID = src.ID
	dst.CreatedAt = src.CreatedAt
	dst.UpdatedAt = src.UpdatedAt
}

func routingStrategyEntityToService(m *dbent.RoutingStrategy) *service.RoutingStrategy {
	if m == nil {
		return nil
	}
	return &service.RoutingStrategy{
		ID:                m.ID,
		Name:              m.Name,
		Description:       m.Description,
		Enabled:           m.Enabled,
		Priority:          m.Priority,
		Platform:          m.Platform,
		GroupID:           m.GroupID,
		MatchMode:         m.MatchMode,
		Conditions:        m.Conditions,
		Action:            m.Action,
		AccountIDs:        m.AccountIds,
		AccountPriorities: m.AccountPriorities,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}

func routingStrategyEntitiesToService(models []*dbent.RoutingStrategy) []service.RoutingStrategy {
	out := make([]service.RoutingStrategy, 0, len(models))
	for i := range models {
		if s := routingStrategyEntityToService(models[i]); s != nil {
			out = append(out, *s)
		}
	}
	return out
}
