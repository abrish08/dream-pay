package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

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

func (s *Store) Save(ctx context.Context, m *domain.Merchant) error {
	return s.db.WithContext(ctx).Table(table).Where("id = ?", m.ID).
		Updates(map[string]any{"status": m.Status, "updated_at": m.UpdatedAt}).Error
}
