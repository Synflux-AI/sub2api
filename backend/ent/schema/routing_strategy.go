package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// RoutingStrategy holds the schema definition for the RoutingStrategy entity.
// 智能路由策略：按请求属性（模型 / 客户端类型 / User-Agent）将请求路由到指定账号。
type RoutingStrategy struct {
	ent.Schema
}

func (RoutingStrategy) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "routing_strategies"},
	}
}

func (RoutingStrategy) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (RoutingStrategy) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			MaxLen(128).
			NotEmpty(),
		field.String("description").
			Default("").
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Bool("enabled").
			Default(true).
			Comment("是否启用"),
		field.Int("priority").
			Default(100).
			Comment("评估优先级，数值越小越先评估（first-match-wins）"),
		field.String("platform").
			MaxLen(32).
			Default(domain.PlatformAnthropic).
			Comment("生效平台，空字符串表示任意平台"),
		field.Int64("group_id").
			Optional().
			Nillable().
			Comment("生效分组 ID，NULL 表示全局生效"),
		field.String("match_mode").
			MaxLen(8).
			Default(domain.RoutingStrategyMatchModeAll).
			Comment("策略内多条件的组合方式：all | any"),
		field.JSON("conditions", []domain.RoutingCondition{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("匹配条件列表：[{type,op,value}]"),
		field.String("action").
			MaxLen(16).
			Default(domain.RoutingStrategyActionRestrict).
			Comment("动作：restrict（硬路由）| prefer（软优先回退）"),
		field.JSON("account_ids", []int64{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}).
			Comment("目标账号 ID 列表"),
	}
}

func (RoutingStrategy) Edges() []ent.Edge {
	return nil
}

func (RoutingStrategy) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("enabled", "priority"),
		index.Fields("group_id"),
		index.Fields("deleted_at"),
	}
}
