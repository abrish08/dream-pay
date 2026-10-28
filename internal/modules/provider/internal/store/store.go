package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/contracts/providers"
)

// Transaction is the provider module's record of every checkout it started.
type Transaction struct {
	TxRef       string `gorm:"primaryKey"`
	Provider    string
	Method      string
	ProviderRef string
	Amount      int64
	Currency    string
	Status      string
	CheckoutURL string
	ReturnURL   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (Transaction) TableName() string { return "transactions" }

type Refund struct {
	RefundRef         string `gorm:"primaryKey"`
	Provider          string
	TxRef             string
	Amount            int64
	Currency          string
	Status            string
	ProviderRefundRef string
	FailureReason     string
	CreatedAt         time.Time
}

func (Refund) TableName() string { return "refunds" }

type mockTx struct {
	TxRef     string `gorm:"primaryKey"`
	Amount    int64
	Currency  string
	Status    string
	UpdatedAt time.Time
}

func (mockTx) TableName() string { return "mock_transactions" }

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) DB(ctx context.Context) *gorm.DB { return s.db.WithContext(ctx) }

func (s *Store) GetTransaction(q *gorm.DB, txRef string, forUpdate bool) (*Transaction, error) {
	if forUpdate {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var t Transaction
	err := q.Where("tx_ref = ?", txRef).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, providers.ErrTransactionNotFound
	}
	return &t, err
}

func (s *Store) CreateTransaction(q *gorm.DB, t *Transaction) error { return q.Create(t).Error }

func (s *Store) UpdateTransactionStatus(q *gorm.DB, txRef, status string, now time.Time) error {
	return q.Model(&Transaction{}).Where("tx_ref = ?", txRef).
		Updates(map[string]any{"status": status, "updated_at": now}).Error
}

func (s *Store) GetRefund(q *gorm.DB, ref string) (*Refund, error) {
	var r Refund
	err := q.Where("refund_ref = ?", ref).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

func (s *Store) SaveRefund(q *gorm.DB, r *Refund) error { return q.Save(r).Error }

// ---- MockState implementation ----

func (s *Store) SaveMock(ctx context.Context, txRef string, amount int64, currency, status string) error {
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).
		Create(&mockTx{TxRef: txRef, Amount: amount, Currency: currency, Status: status, UpdatedAt: time.Now().UTC()}).Error
}

func (s *Store) GetMock(ctx context.Context, txRef string) (int64, string, string, error) {
	var m mockTx
	err := s.db.WithContext(ctx).Where("tx_ref = ?", txRef).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, "", "", providers.ErrTransactionNotFound
	}
	return m.Amount, m.Currency, m.Status, err
}

func (s *Store) SetMockStatus(ctx context.Context, txRef, status string) error {
	res := s.db.WithContext(ctx).Model(&mockTx{}).Where("tx_ref = ? AND status = ?", txRef, providers.StatusPending).
		Updates(map[string]any{"status": status, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		if _, _, _, err := s.GetMock(ctx, txRef); err != nil {
			return err
		}
	}
	return nil
}
