package store

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/ledger/internal/domain"
)

type txRecord struct {
	ID          string
	Reference   string
	Currency    string
	Description string
	CreatedAt   time.Time
}

func (txRecord) TableName() string { return "ledger_transactions" }

type entryRecord struct {
	ID            int64
	TransactionID string
	Account       string
	Debit         int64
	Credit        int64
	Currency      string
	CreatedAt     time.Time
}

func (entryRecord) TableName() string { return "ledger_entries" }

type Store struct{}

func New() *Store { return &Store{} }

// Post writes a balanced posting. A second post with the same reference is a
// no-op, so the ledger stays correct even if an event is processed twice.
func (s *Store) Post(q *gorm.DB, p domain.Posting, now time.Time) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	tx := txRecord{ID: uuid.NewString(), Reference: p.Reference, Currency: p.Currency, Description: p.Description, CreatedAt: now}
	res := q.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "reference"}}, DoNothing: true}).Create(&tx)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	entries := make([]entryRecord, len(p.Lines))
	for i, l := range p.Lines {
		entries[i] = entryRecord{TransactionID: tx.ID, Account: l.Account, Debit: l.Debit, Credit: l.Credit, Currency: p.Currency, CreatedAt: now}
	}
	return true, q.Create(&entries).Error
}

// LockAccount serialises balance checks on one account (transaction-scoped).
func (s *Store) LockAccount(q *gorm.DB, account, currency string) error {
	return q.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "ledger:"+account+":"+currency).Error
}

func (s *Store) Posted(q *gorm.DB, reference string) (bool, error) {
	var n int64
	err := q.Model(&txRecord{}).Where("reference = ?", reference).Count(&n).Error
	return n > 0, err
}

// Balance of a credit-normal account (what we owe): credits - debits.
func (s *Store) Balance(q *gorm.DB, account, currency string) (int64, error) {
	var v int64
	err := q.Model(&entryRecord{}).Select("COALESCE(SUM(credit - debit), 0)").
		Where("account = ? AND currency = ?", account, currency).Scan(&v).Error
	return v, err
}

type TrialBalanceRow struct {
	Currency string `json:"currency"`
	Debits   int64  `json:"debits"`
	Credits  int64  `json:"credits"`
	Balanced bool   `json:"balanced"`
}

func (s *Store) TrialBalance(q *gorm.DB) ([]TrialBalanceRow, error) {
	var rows []TrialBalanceRow
	err := q.Model(&entryRecord{}).Select("currency, SUM(debit) AS debits, SUM(credit) AS credits").
		Group("currency").Order("currency").Scan(&rows).Error
	for i := range rows {
		rows[i].Balanced = rows[i].Debits == rows[i].Credits
	}
	return rows, err
}

// PostingTotals: reference, currency and total debits of each posting found.
func (s *Store) PostingTotals(q *gorm.DB, refs []string) ([]PostingTotal, error) {
	var out []PostingTotal
	if len(refs) == 0 {
		return out, nil
	}
	err := q.Table("ledger_transactions AS t").
		Select("t.reference, t.currency, COALESCE(SUM(e.debit), 0) AS amount").
		Joins("JOIN ledger_entries e ON e.transaction_id = t.id").
		Where("t.reference IN ?", refs).Group("t.reference, t.currency").Scan(&out).Error
	return out, err
}

type PostingTotal struct {
	Reference string
	Currency  string
	Amount    int64
}

// ReserveHold: a payment's reserve waiting for its release date.
type ReserveHold struct {
	PaymentID   string `gorm:"primaryKey"`
	MerchantID  string
	Environment string
	Currency    string
	Amount      int64
	ReleaseAt   time.Time
	ReleasedAt  *time.Time
}

func (ReserveHold) TableName() string { return "reserve_holds" }

func (s *Store) AddHold(q *gorm.DB, h *ReserveHold) error {
	return q.Clauses(clause.OnConflict{DoNothing: true}).Create(h).Error
}

// ClaimDueHolds locks due holds (SKIP LOCKED: safe with many replicas).
func (s *Store) ClaimDueHolds(q *gorm.DB, now time.Time, limit int) ([]ReserveHold, error) {
	var out []ReserveHold
	err := q.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("released_at IS NULL AND release_at <= ?", now).Order("release_at").Limit(limit).Find(&out).Error
	return out, err
}

func (s *Store) MarkReleased(q *gorm.DB, paymentID string, now time.Time) error {
	return q.Model(&ReserveHold{}).Where("payment_id = ?", paymentID).Update("released_at", now).Error
}

// StatementLine: one movement of a merchant's available balance.
type StatementLine struct {
	ID          int64
	CreatedAt   time.Time
	Reference   string
	Description string
	Debit       int64
	Credit      int64
}

// OpeningBalance of a credit-normal account before t.
func (s *Store) OpeningBalance(q *gorm.DB, account, currency string, t time.Time) (int64, error) {
	var v int64
	err := q.Model(&entryRecord{}).Select("COALESCE(SUM(credit - debit), 0)").
		Where("account = ? AND currency = ? AND created_at < ?", account, currency, t).Scan(&v).Error
	return v, err
}

// Statement: movements of account in [from, to), oldest first, after id.
func (s *Store) Statement(q *gorm.DB, account, currency string, from, to time.Time, afterID int64, limit int) ([]StatementLine, error) {
	var out []StatementLine
	err := q.Table("ledger_entries AS e").
		Select("e.id, e.created_at, t.reference, t.description, e.debit, e.credit").
		Joins("JOIN ledger_transactions t ON t.id = e.transaction_id").
		Where("e.account = ? AND e.currency = ? AND e.created_at >= ? AND e.created_at < ? AND e.id > ?", account, currency, from, to, afterID).
		Order("e.id").Limit(limit).Scan(&out).Error
	return out, err
}
