package service

import (
	"context"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/billingentity"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrBillingEntityNotFound = infraerrors.NotFound("BILLING_ENTITY_NOT_FOUND", "billing entity not found")
	ErrBillingEntityExists   = infraerrors.Conflict("BILLING_ENTITY_EXISTS", "billing entity name already exists")
	ErrBillingEntityInUse    = infraerrors.Conflict("BILLING_ENTITY_IN_USE", "billing entity is assigned to users")
)

type BillingEntity struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Currency  string    `json:"currency"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateBillingEntityInput struct {
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

type UpdateBillingEntityInput struct {
	Name     *string `json:"name"`
	Currency *string `json:"currency"`
	Status   *string `json:"status"`
}

type BillingEntityService struct {
	client *dbent.Client
}

func NewBillingEntityService(client *dbent.Client) *BillingEntityService {
	return &BillingEntityService{client: client}
}

func (s *BillingEntityService) List(ctx context.Context) ([]BillingEntity, error) {
	rows, err := s.client.BillingEntity.Query().Order(dbent.Asc(billingentity.FieldName)).All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]BillingEntity, 0, len(rows))
	for _, row := range rows {
		result = append(result, billingEntityFromEnt(row))
	}
	return result, nil
}

func (s *BillingEntityService) Create(ctx context.Context, input CreateBillingEntityInput) (*BillingEntity, error) {
	name, currency, err := normalizeBillingEntity(input.Name, input.Currency)
	if err != nil {
		return nil, err
	}
	row, err := s.client.BillingEntity.Create().SetName(name).SetCurrency(currency).Save(ctx)
	if dbent.IsConstraintError(err) {
		return nil, ErrBillingEntityExists
	}
	if err != nil {
		return nil, err
	}
	result := billingEntityFromEnt(row)
	return &result, nil
}

func (s *BillingEntityService) Update(ctx context.Context, id int64, input UpdateBillingEntityInput) (*BillingEntity, error) {
	row, err := s.client.BillingEntity.Get(ctx, id)
	if dbent.IsNotFound(err) {
		return nil, ErrBillingEntityNotFound
	}
	if err != nil {
		return nil, err
	}
	update := row.Update()
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			return nil, infraerrors.BadRequest("BILLING_ENTITY_NAME_REQUIRED", "billing entity name is required")
		}
		update.SetName(name)
	}
	if input.Currency != nil {
		currency, err := normalizeBillingCurrency(*input.Currency)
		if err != nil {
			return nil, err
		}
		update.SetCurrency(currency)
	}
	if input.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*input.Status))
		if status != "active" && status != "inactive" {
			return nil, infraerrors.BadRequest("BILLING_ENTITY_STATUS_INVALID", "billing entity status must be active or inactive")
		}
		update.SetStatus(status)
	}
	updated, err := update.Save(ctx)
	if dbent.IsConstraintError(err) {
		return nil, ErrBillingEntityExists
	}
	if err != nil {
		return nil, err
	}
	result := billingEntityFromEnt(updated)
	return &result, nil
}

func (s *BillingEntityService) Delete(ctx context.Context, id int64) error {
	row, err := s.client.BillingEntity.Get(ctx, id)
	if dbent.IsNotFound(err) {
		return ErrBillingEntityNotFound
	}
	if err != nil {
		return err
	}
	inUse, err := row.QueryUsers().Exist(ctx)
	if err != nil {
		return err
	}
	if inUse {
		return ErrBillingEntityInUse
	}
	return s.client.BillingEntity.DeleteOneID(id).Exec(ctx)
}

func normalizeBillingEntity(rawName, rawCurrency string) (string, string, error) {
	name := strings.TrimSpace(rawName)
	if name == "" {
		return "", "", infraerrors.BadRequest("BILLING_ENTITY_NAME_REQUIRED", "billing entity name is required")
	}
	currency, err := normalizeBillingCurrency(rawCurrency)
	return name, currency, err
}

func normalizeBillingCurrency(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", infraerrors.BadRequest("BILLING_ENTITY_CURRENCY_REQUIRED", "billing entity currency is required")
	}
	currency, err := payment.NormalizePaymentCurrency(raw)
	if err != nil {
		return "", infraerrors.BadRequest("BILLING_ENTITY_CURRENCY_INVALID", "currency must be a 3-letter ISO currency code")
	}
	return currency, nil
}

func billingEntityFromEnt(row *dbent.BillingEntity) BillingEntity {
	return BillingEntity{
		ID: row.ID, Name: row.Name, Currency: row.Currency, Status: row.Status,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
