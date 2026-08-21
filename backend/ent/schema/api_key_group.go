package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// APIKeyGroup holds the edge schema definition for the api_key_groups relationship.
// 一个 API Key 可以绑定多个已有分组，请求按命中的分组独立计费。
//
// platform 是写入时从 groups.platform 取的快照列，配合 (api_key_id, platform) 唯一索引
// 保证「同一个 Key 在同一平台下只能绑定一个分组」；api_keys.group_id 保留为默认分组指针。
type APIKeyGroup struct {
	ent.Schema
}

func (APIKeyGroup) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "api_key_groups"},
		// Composite primary key: (api_key_id, group_id).
		field.ID("api_key_id", "group_id"),
	}
}

func (APIKeyGroup) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("api_key_id"),
		field.Int64("group_id"),
		field.String("platform").
			MaxLen(50).
			Comment("Snapshot of groups.platform at bind time; one binding per (api_key, platform)"),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (APIKeyGroup) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("api_key", APIKey.Type).
			Unique().
			Required().
			Field("api_key_id"),
		edge.To("group", Group.Type).
			Unique().
			Required().
			Field("group_id"),
	}
}

func (APIKeyGroup) Indexes() []ent.Index {
	return []ent.Index{
		// 反向查询：按分组找绑定了它的 API Key。
		index.Fields("group_id"),
		// 同一个 Key 在同一平台下只能绑定一个分组。
		index.Fields("api_key_id", "platform").
			Unique(),
	}
}
