package schema

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// BillingEntity is a platform legal entity used to collect and reconcile payments.
type BillingEntity struct {
	ent.Schema
}

func (BillingEntity) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "billing_entities"}}
}

func (BillingEntity) Mixin() []ent.Mixin {
	return []ent.Mixin{mixins.TimeMixin{}}
}

func (BillingEntity) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").MaxLen(200).NotEmpty().Unique(),
		field.String("currency").MaxLen(3).MinLen(3).NotEmpty(),
		field.String("status").Default("active").Validate(func(value string) error {
			if value != "active" && value != "inactive" {
				return fmt.Errorf("must be active or inactive")
			}
			return nil
		}),
	}
}

func (BillingEntity) Edges() []ent.Edge {
	return []ent.Edge{edge.To("users", User.Type)}
}
