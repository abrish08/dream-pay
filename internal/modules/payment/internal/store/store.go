package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/contracts/payments"
	"paygate/internal/modules/payment/internal/domain"
	"paygate/internal/platform/apperror"
)

const table = "payments"

var ErrDuplicate = errors.New("duplicate payment")

// Store methods take the query handle (s.db or a transaction) explicitly, so
// the service decides transaction boundaries.
type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Insert(q *gorm.DB, p *domain.Payment) error {
	err := q.Table(table).Create(p).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) Save(q *gorm.DB, p *domain.Payment) error {
	return q.Table(table).Where("id = ?", p.ID).Select("*").Updates(p).Error
}

func (s *Store) Get(q *gorm.DB, id string, forUpdate bool) (*domain.Payment, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, payments.ErrNotFound
	}
	if forUpdate {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var p domain.Payment
	err := q.Table(table).Where("id = ?", id).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, payments.ErrNotFound
	}
	return &p, err
}

// FindByIdempotencyKey returns (nil, nil) when no payment uses the key.
func (s *Store) FindByIdempotencyKey(q *gorm.DB, merchantID, key string) (*domain.Payment, error) {
	var p domain.Payment
	err := q.Table(table).Where("merchant_id = ? AND idempotency_key = ?", merchantID, key).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &p, err
}

// MerchantFilter: a merchant's payments, newest first.
type MerchantFilter struct {
	MerchantID    string
	Environment   string // "" = both
	Statuses      []string
	Reference     string
	CreatedFrom   time.Time
	CreatedTo     time.Time
	StartingAfter string // payment id from the previous page
	Limit         int
}

// ListForMerchant returns one page and whether more rows follow. The cursor
// is (created_at, id), so pages stay stable while new payments arrive.
func (s *Store) ListForMerchant(q *gorm.DB, f MerchantFilter) ([]domain.Payment, bool, error) {
	q = q.Table(table).Where("merchant_id = ?", f.MerchantID)
	if f.Environment != "" {
		q = q.Where("environment = ?", f.Environment)
	}
	if len(f.Statuses) > 0 {
		q = q.Where("status IN ?", f.Statuses)
	}
	if f.Reference != "" {
		q = q.Where("reference = ?", f.Reference)
	}
	if !f.CreatedFrom.IsZero() {
		q = q.Where("created_at >= ?", f.CreatedFrom)
	}
	if !f.CreatedTo.IsZero() {
		q = q.Where("created_at < ?", f.CreatedTo)
	}
	if f.StartingAfter != "" {
		if _, err := uuid.Parse(f.StartingAfter); err != nil {
			return nil, false, ErrBadCursor
		}
		q = q.Where("(created_at, id) < (SELECT created_at, id FROM payments WHERE id = ? AND merchant_id = ?)",
			f.StartingAfter, f.MerchantID)
	}
	var out []domain.Payment
	if err := q.Order("created_at DESC, id DESC").Limit(f.Limit + 1).Find(&out).Error; err != nil {
		return nil, false, err
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}

var ErrBadCursor = apperror.Invalid("invalid_cursor", "starting_after must be a payment id from a previous page")

// DueForExpiry: open payments past expires_at, oldest first.
func (s *Store) DueForExpiry(q *gorm.DB, now time.Time, limit int) ([]domain.Payment, error) {
	var out []domain.Payment
	err := q.Table(table).Where("status IN ? AND expires_at < ?",
		[]string{string(domain.Pending), string(domain.Processing)}, now).
		Order("expires_at").Limit(limit).Find(&out).Error
	return out, err
}

func (s *Store) List(q *gorm.DB, f payments.ListPayments) ([]domain.Payment, error) {
	q = q.Table(table)
	if len(f.Statuses) > 0 {
		q = q.Where("status IN ?", f.Statuses)
	}
	if !f.CreatedFrom.IsZero() {
		q = q.Where("created_at >= ?", f.CreatedFrom)
	}
	if !f.CreatedTo.IsZero() {
		q = q.Where("created_at < ?", f.CreatedTo)
	}
	if f.AfterID != "" {
		q = q.Where("id > ?", f.AfterID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []domain.Payment
	err := q.Order("id").Limit(limit).Find(&out).Error
	return out, err
}
