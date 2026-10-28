package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/merchant/internal/domain"
	"paygate/internal/platform/apperror"
)

const table = "merchants"

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

// WithTx returns a store that runs inside tx.
func (s *Store) WithTx(tx *gorm.DB) *Store { return &Store{db: tx} }

func (s *Store) Create(ctx context.Context, m *domain.Merchant) error {
	err := s.db.WithContext(ctx).Table(table).Create(m).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return apperror.Conflict("merchant_email_taken", "a merchant with this email already exists")
	}
	return err
}

func (s *Store) Get(ctx context.Context, id string) (*domain.Merchant, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, merchants.ErrNotFound
	}
	var m domain.Merchant
	err := s.db.WithContext(ctx).Table(table).Where("id = ?", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, merchants.ErrNotFound
	}
	return &m, err
}

// GetForUpdate locks the row (status and fee changes).
func (s *Store) GetForUpdate(ctx context.Context, id string) (*domain.Merchant, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, merchants.ErrNotFound
	}
	var m domain.Merchant
	err := s.db.WithContext(ctx).Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, merchants.ErrNotFound
	}
	return &m, err
}

func (s *Store) Save(ctx context.Context, m *domain.Merchant) error {
	return s.db.WithContext(ctx).Table(table).Where("id = ?", m.ID).
		Updates(map[string]any{"status": m.Status, "fee_bps": m.FeeBPS, "approved_at": m.ApprovedAt,
			"updated_at": m.UpdatedAt}).Error
}

// List: newest first, optional status, cursor (created_at, id).
func (s *Store) List(ctx context.Context, status, startingAfter string, limit int) ([]domain.Merchant, bool, error) {
	q := s.db.WithContext(ctx).Table(table)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if startingAfter != "" {
		if _, err := uuid.Parse(startingAfter); err != nil {
			return nil, false, apperror.Invalid("invalid_cursor", "starting_after must be a merchant id")
		}
		q = q.Where("(created_at, id) < (SELECT created_at, id FROM merchants WHERE id = ?)", startingAfter)
	}
	var out []domain.Merchant
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&out).Error; err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
