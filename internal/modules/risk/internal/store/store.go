package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/platform/apperror"
)

// BlockEntry: an email, phone or IP that may not pay.
type BlockEntry struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	Kind      string    `gorm:"not null;uniqueIndex:risk_block_value" json:"kind"`
	Value     string    `gorm:"not null;uniqueIndex:risk_block_value" json:"value"`
	Reason    string    `gorm:"not null;default:''" json:"reason"`
	CreatedBy string    `gorm:"not null;default:''" json:"created_by"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

func (BlockEntry) TableName() string { return "blocklist_entries" }

// Attempt: one assessed payment, kept for velocity and audit.
type Attempt struct {
	ID          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	PaymentID   string    `gorm:"type:uuid;not null;uniqueIndex" json:"payment_id"`
	MerchantID  string    `gorm:"type:uuid;not null;index:risk_attempt_merchant,priority:1" json:"merchant_id"`
	Environment string    `gorm:"not null" json:"environment"`
	Amount      int64     `gorm:"not null" json:"amount"`
	Currency    string    `gorm:"not null;index:risk_attempt_merchant,priority:2" json:"currency"`
	Email       string    `gorm:"not null;default:'';index:risk_attempt_email,priority:1" json:"email,omitempty"`
	Phone       string    `gorm:"not null;default:'';index:risk_attempt_phone,priority:1" json:"phone,omitempty"`
	IP          string    `gorm:"not null;default:'';index:risk_attempt_ip,priority:1" json:"ip,omitempty"`
	Action      string    `gorm:"not null" json:"action"`
	Score       int       `gorm:"not null" json:"score"`
	Reasons     string    `gorm:"type:jsonb;not null;default:'[]'" json:"-"`
	Failed      bool      `gorm:"not null;default:false" json:"failed"` // the payment was declined later
	CreatedAt   time.Time `gorm:"not null;index:risk_attempt_merchant,priority:3;index:risk_attempt_email,priority:2;index:risk_attempt_phone,priority:2;index:risk_attempt_ip,priority:2" json:"created_at"`
}

func (Attempt) TableName() string { return "attempts" }

func Models() []any { return []any{&BlockEntry{}, &Attempt{}} }

type Store struct{}

func New() *Store { return &Store{} }

var ErrDuplicateBlock = apperror.Conflict("already_blocked", "this value is already on the blocklist")

func (s *Store) AddBlock(q *gorm.DB, e *BlockEntry) error {
	err := q.Create(e).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return ErrDuplicateBlock
	}
	return err
}

func (s *Store) DeleteBlock(q *gorm.DB, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return apperror.NotFound("block_not_found", "blocklist entry not found")
	}
	res := q.Where("id = ?", id).Delete(&BlockEntry{})
	if res.Error == nil && res.RowsAffected == 0 {
		return apperror.NotFound("block_not_found", "blocklist entry not found")
	}
	return res.Error
}

func (s *Store) ListBlocks(q *gorm.DB, kind string, limit int) ([]BlockEntry, error) {
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var out []BlockEntry
	return out, q.Order("created_at DESC").Limit(limit).Find(&out).Error
}

// Blocked returns the kinds whose value is on the blocklist.
func (s *Store) Blocked(q *gorm.DB, values map[string]string) ([]string, error) {
	var kinds []string
	cond := q.Model(&BlockEntry{})
	n := 0
	for kind, v := range values {
		if v == "" {
			continue
		}
		if n == 0 {
			cond = cond.Where("kind = ? AND value = ?", kind, v)
		} else {
			cond = cond.Or("kind = ? AND value = ?", kind, v)
		}
		n++
	}
	if n == 0 {
		return nil, nil
	}
	return kinds, cond.Distinct("kind").Pluck("kind", &kinds).Error
}

// Count attempts with column = value since t (failedOnly: declined later).
func (s *Store) Count(q *gorm.DB, column, value string, since time.Time, failedOnly bool) (int, error) {
	if value == "" {
		return 0, nil
	}
	q = q.Model(&Attempt{}).Where(column+" = ? AND created_at >= ?", value, since)
	if failedOnly {
		q = q.Where("failed")
	}
	var n int64
	return int(n), q.Count(&n).Error
}

// MerchantAverage: average amount and count of allowed attempts since t.
func (s *Store) MerchantAverage(q *gorm.DB, merchantID, env, currency string, since time.Time) (int64, int, error) {
	var row struct {
		Avg float64
		N   int
	}
	err := q.Model(&Attempt{}).Select("COALESCE(AVG(amount), 0) AS avg, COUNT(*) AS n").
		Where("merchant_id = ? AND environment = ? AND currency = ? AND action = 'allow' AND created_at >= ?", merchantID, env, currency, since).
		Scan(&row).Error
	return int64(row.Avg), row.N, err
}

func (s *Store) Record(q *gorm.DB, a *Attempt) error {
	return q.Where("payment_id = ?", a.PaymentID).FirstOrCreate(a).Error
}

func (s *Store) MarkFailed(q *gorm.DB, paymentID string) error {
	return q.Model(&Attempt{}).Where("payment_id = ?", paymentID).Update("failed", true).Error
}

func (s *Store) Decisions(q *gorm.DB, merchantID, paymentID, action string, limit int) ([]Attempt, error) {
	if merchantID != "" {
		q = q.Where("merchant_id = ?", merchantID)
	}
	if paymentID != "" {
		q = q.Where("payment_id = ?", paymentID)
	}
	if action != "" {
		q = q.Where("action = ?", action)
	}
	var out []Attempt
	return out, q.Order("created_at DESC").Limit(limit).Find(&out).Error
}
