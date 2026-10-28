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
