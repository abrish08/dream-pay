package store

import (
	"encoding/json"
	"errors"
	"strings"
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

// Insert creates the payment and the first entry of its history together.
func (s *Store) Insert(q *gorm.DB, p *domain.Payment, by domain.Actor) error {
	return q.Transaction(func(tx *gorm.DB) error {
		err := tx.Table(table).Create(p).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrDuplicate
		}
		if err != nil {
			return err
		}
		return s.writeHistory(tx, p, by)
	})
}

// Save stores the payment and appends its pending changes to the history.
// Call it inside the transaction that made the change, so the history can
// never disagree with the payment.
func (s *Store) Save(q *gorm.DB, p *domain.Payment, by domain.Actor) error {
	if err := q.Table(table).Where("id = ?", p.ID).Select("*").Omit("changes").Updates(p).Error; err != nil {
		return err
	}
	return s.writeHistory(q, p, by)
}

// Event is one row of payment_events (append-only).
type Event struct {
	ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	PaymentID  string    `json:"-"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Source     string    `json:"source"`
	Reason     string    `json:"reason,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Details    string    `gorm:"type:jsonb" json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

func (Event) TableName() string { return "payment_events" }

func (s *Store) writeHistory(q *gorm.DB, p *domain.Payment, by domain.Actor) error {
	changes := p.Changes()
	if len(changes) == 0 {
		return nil
	}
	details := "{}"
	if len(by.Details) > 0 {
		b, err := json.Marshal(by.Details)
		if err != nil {
			return err
		}
		details = string(b)
	}
	provider := by.Provider
	if provider == "" {
		provider = p.Provider
	}
	rows := make([]Event, 0, len(changes))
	for _, c := range changes {
		reason := by.Reason
		switch {
		case c.Note != "":
			reason = c.Note
		case reason == "" && (c.To == domain.Failed || c.To == domain.Expired):
			reason = p.FailureReason
		}
		source := by.Source
		if source == "" {
			source = domain.SourceSystem
		}
		rows = append(rows, Event{PaymentID: p.ID, FromStatus: string(c.From), ToStatus: string(c.To), Source: source,
			Reason: reason, Provider: provider, Details: details, CreatedAt: c.At})
	}
	if err := q.Create(&rows).Error; err != nil {
		return err
	}
	p.ClearChanges()
	return nil
}

// History returns a payment's events, oldest first.
func (s *Store) History(q *gorm.DB, paymentID string) ([]Event, error) {
	var out []Event
	err := q.Where("payment_id = ?", paymentID).Order("id").Find(&out).Error
	return out, err
}

// GetByIDs returns the payments with these ids (unknown ids are skipped).
func (s *Store) GetByIDs(q *gorm.DB, ids []string) ([]domain.Payment, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			valid = append(valid, id)
		}
	}
	var out []domain.Payment
	if len(valid) == 0 {
		return out, nil
	}
	err := q.Table(table).Where("id IN ?", valid).Find(&out).Error
	return out, err
}

// DueCheck is an open payment whose next status check is due.
type DueCheck struct {
	ID       string
	Provider string
}

// ClaimDueChecks takes up to limit payments whose status check is due and
// pushes their next_check_at forward by lease, so another replica does not
// check them at the same time (FOR UPDATE SKIP LOCKED).
func (s *Store) ClaimDueChecks(q *gorm.DB, now time.Time, lease time.Duration, limit int) ([]DueCheck, error) {
	var out []DueCheck
	err := q.Raw(`UPDATE payments SET next_check_at = $1
		WHERE id IN (SELECT id FROM payments WHERE status = 'processing' AND next_check_at <= $2
		             ORDER BY next_check_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING id, provider`, now.Add(lease), now, limit).Scan(&out).Error
	return out, err
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
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
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

// Volume: total and count of a merchant's payments since t that can still
// take or have taken money (failed and expired ones do not count).
func (s *Store) Volume(q *gorm.DB, merchantID, env, currency string, since time.Time) (int64, int64, error) {
	var row struct {
		Total int64
		N     int64
	}
	err := q.Table(table).Select("COALESCE(SUM(amount), 0) AS total, COUNT(*) AS n").
		Where("merchant_id = ? AND environment = ? AND currency = ? AND created_at >= ? AND status IN ?", merchantID, env, currency, since,
			[]string{string(domain.Pending), string(domain.Processing), string(domain.Succeeded),
				string(domain.PartiallyRefunded), string(domain.Refunded)}).Scan(&row).Error
	return row.Total, row.N, err
}

// ---- Payment links ----

var ErrLinkNotFound = apperror.NotFound("payment_link_not_found", "payment link not found")

func (s *Store) InsertLink(q *gorm.DB, l *domain.PaymentLink) error { return q.Create(l).Error }

func (s *Store) SaveLink(q *gorm.DB, l *domain.PaymentLink) error {
	return q.Select("*").Updates(l).Error
}

func (s *Store) GetLink(q *gorm.DB, id string, lock bool) (*domain.PaymentLink, error) {
	if !strings.HasPrefix(id, "pl_") || len(id) > 40 {
		return nil, ErrLinkNotFound
	}
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var l domain.PaymentLink
	err := q.Where("id = ?", id).Take(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLinkNotFound
	}
	return &l, err
}

func (s *Store) ListLinks(q *gorm.DB, merchantID, env string, limit int) ([]domain.PaymentLink, error) {
	q = q.Where("merchant_id = ?", merchantID)
	if env != "" {
		q = q.Where("environment = ?", env)
	}
	var out []domain.PaymentLink
	return out, q.Order("created_at DESC").Limit(limit).Find(&out).Error
}

// OpenPaymentForLink: a payment of the link that a customer may still pay.
func (s *Store) OpenPaymentForLink(q *gorm.DB, linkID string, now time.Time) (*domain.Payment, error) {
	var p domain.Payment
	err := q.Table(table).Where("link_id = ? AND status IN ? AND expires_at > ?", linkID,
		[]string{string(domain.Pending), string(domain.Processing)}, now).Order("created_at DESC").Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &p, err
}

func (s *Store) SetLinkID(q *gorm.DB, paymentID, linkID string) error {
	return q.Table(table).Where("id = ?", paymentID).Update("link_id", linkID).Error
}

// LinkPaid counts a captured payment; a single-use link then closes.
func (s *Store) LinkPaid(q *gorm.DB, linkID string, now time.Time) error {
	return q.Model(&domain.PaymentLink{}).Where("id = ?", linkID).Updates(map[string]any{
		"paid_count": gorm.Expr("paid_count + 1"), "active": gorm.Expr("CASE WHEN single_use THEN false ELSE active END"),
		"updated_at": now}).Error
}
