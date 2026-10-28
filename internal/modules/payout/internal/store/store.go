// Package store: the payout service's tables, defined as GORM models and
// auto-migrated on start (additive only, see platform/database).
package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/contracts/payouts"
	"paygate/internal/modules/payout/internal/domain"
	"paygate/internal/platform/apperror"
)

var (
	ErrNotFound  = apperror.NotFound("payout_not_found", "payout not found")
	ErrDuplicate = errors.New("duplicate payout")
)

// payoutRec is the payouts table.
type payoutRec struct {
	ID             string     `gorm:"type:uuid;primaryKey"`
	MerchantID     string     `gorm:"type:uuid;not null;uniqueIndex:payouts_merchant_idem_key,priority:1;index:payouts_merchant_created_idx,priority:1"`
	Environment    string     `gorm:"type:text;not null"`
	Amount         int64      `gorm:"not null"`
	Currency       string     `gorm:"type:char(3);not null"`
	Status         string     `gorm:"type:text;not null;index:payouts_status_created_idx,priority:1"`
	BankName       string     `gorm:"type:text;not null"`
	AccountNumber  string     `gorm:"type:text;not null"`
	AccountName    string     `gorm:"type:text;not null"`
	Description    string     `gorm:"type:text;not null;default:''"`
	BankReference  string     `gorm:"type:text;not null;default:''"`
	FailureReason  string     `gorm:"type:text;not null;default:''"`
	IdempotencyKey string     `gorm:"type:text;not null;uniqueIndex:payouts_merchant_idem_key,priority:2"`
	RequestHash    string     `gorm:"type:text;not null"`
	RequestedBy    string     `gorm:"type:text;not null;default:''"`
	ProcessedBy    string     `gorm:"type:text;not null;default:''"`
	PaidAt         *time.Time `gorm:"type:timestamptz"`
	CreatedAt      time.Time  `gorm:"type:timestamptz;not null;index:payouts_merchant_created_idx,priority:2,sort:desc;index:payouts_status_created_idx,priority:2"`
	UpdatedAt      time.Time  `gorm:"type:timestamptz;not null"`
}

func (payoutRec) TableName() string { return "payouts" }

// Models lists every table of the payout service.
func Models() []any { return []any{&payoutRec{}} }

func toRec(p *domain.Payout) payoutRec     { return payoutRec(*p) }
func (r payoutRec) domain() *domain.Payout { p := domain.Payout(r); return &p }

type Store struct{}

func New() *Store { return &Store{} }

func (s *Store) Insert(q *gorm.DB, p *domain.Payout) error {
	rec := toRec(p)
	err := q.Create(&rec).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) Save(q *gorm.DB, p *domain.Payout) error {
	rec := toRec(p)
	return q.Model(&payoutRec{}).Where("id = ?", p.ID).Select("*").Updates(&rec).Error
}

func (s *Store) Get(q *gorm.DB, id string, lock bool) (*domain.Payout, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var rec payoutRec
	err := q.Where("id = ?", id).Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return rec.domain(), nil
}

func (s *Store) ByIdempotencyKey(q *gorm.DB, merchantID, key string) (*domain.Payout, error) {
	var rec payoutRec
	err := q.Where("merchant_id = ? AND idempotency_key = ?", merchantID, key).Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec.domain(), nil
}

// Stuck: payouts still reserving after cutoff (the ledger was unreachable).
func (s *Store) Stuck(q *gorm.DB, cutoff time.Time, limit int) ([]*domain.Payout, error) {
	var recs []payoutRec
	err := q.Where("status = ? AND updated_at < ?", payouts.StatusReserving, cutoff).Order("updated_at").Limit(limit).Find(&recs).Error
	return toDomain(recs), err
}

// List: merchantID "" lists all merchants (platform staff). Newest first.
func (s *Store) List(q *gorm.DB, merchantID, status, startingAfter string, limit int) ([]*domain.Payout, bool, error) {
	if merchantID != "" {
		q = q.Where("merchant_id = ?", merchantID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if startingAfter != "" {
		if _, err := uuid.Parse(startingAfter); err != nil {
			return nil, false, apperror.Invalid("invalid_cursor", "starting_after must be a payout id")
		}
		q = q.Where("(created_at, id) < (SELECT created_at, id FROM payouts WHERE id = ?)", startingAfter)
	}
	var recs []payoutRec
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&recs).Error; err != nil {
		return nil, false, err
	}
	more := len(recs) > limit
	if more {
		recs = recs[:limit]
	}
	return toDomain(recs), more, nil
}

func toDomain(recs []payoutRec) []*domain.Payout {
	out := make([]*domain.Payout, len(recs))
	for i, r := range recs {
		out[i] = r.domain()
	}
	return out
}
