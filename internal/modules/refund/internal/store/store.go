package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/refund/internal/domain"
)

const table = "refunds"

var ErrDuplicate = errors.New("duplicate refund")

type Store struct{}

func New() *Store { return &Store{} }

func (s *Store) Insert(q *gorm.DB, r *domain.Refund) error {
	err := q.Table(table).Create(r).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) Save(q *gorm.DB, r *domain.Refund) error {
	return q.Table(table).Where("id = ?", r.ID).Select("*").Updates(r).Error
}

func (s *Store) Get(q *gorm.DB, id string, forUpdate bool) (*domain.Refund, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, refunds.ErrNotFound
	}
	if forUpdate {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var r domain.Refund
	err := q.Table(table).Where("id = ?", id).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, refunds.ErrNotFound
	}
	return &r, err
}

func (s *Store) FindByIdempotencyKey(q *gorm.DB, merchantID, key string) (*domain.Refund, error) {
	var r domain.Refund
	err := q.Table(table).Where("merchant_id = ? AND idempotency_key = ?", merchantID, key).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

// CommittedAmount sums refunds that are pending or succeeded for a payment.
func (s *Store) CommittedAmount(q *gorm.DB, paymentID string) (int64, error) {
	var total int64
	err := q.Table(table).Select("COALESCE(SUM(amount), 0)").
		Where("payment_id = ? AND status IN ?", paymentID, []string{refunds.StatusPending, refunds.StatusSucceeded}).
		Scan(&total).Error
	return total, err
}

// StalePending returns pending refunds not updated since before cutoff.
func (s *Store) StalePending(q *gorm.DB, cutoff time.Time, limit int) ([]domain.Refund, error) {
	var out []domain.Refund
	err := q.Table(table).Where("status = ? AND updated_at < ?", refunds.StatusPending, cutoff).
		Order("updated_at").Limit(limit).Find(&out).Error
	return out, err
}

// LockPayment serialises refund creation per payment (transaction-scoped).
func (s *Store) LockPayment(q *gorm.DB, paymentID string) error {
	return q.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "refund:"+paymentID).Error
}
