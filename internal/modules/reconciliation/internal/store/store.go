package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/reconciliation/internal/domain"
	"paygate/internal/platform/apperror"
)

var ErrRunNotFound = apperror.NotFound("reconciliation_run_not_found", "reconciliation run not found")

const (
	runs  = "reconciliation_runs"
	items = "reconciliation_items"
)

type Store struct{}

func New() *Store { return &Store{} }

// InsertRun returns false if a daily run for the same period already exists.
func (s *Store) InsertRun(q *gorm.DB, r *domain.Run) (bool, error) {
	res := q.Table(runs).Clauses(clause.OnConflict{DoNothing: true}).Create(r)
	return res.RowsAffected == 1, res.Error
}

func (s *Store) GetRun(q *gorm.DB, id string) (*domain.Run, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrRunNotFound
	}
	var r domain.Run
	err := q.Table(runs).Where("id = ?", id).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRunNotFound
	}
	return &r, err
}

func (s *Store) ListRuns(q *gorm.DB, limit int) ([]domain.Run, error) {
	var out []domain.Run
	err := q.Table(runs).Order("created_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

// ClaimQueued marks one queued run as running. SKIP LOCKED makes it safe
// with several replicas.
func (s *Store) ClaimQueued(q *gorm.DB, now time.Time) (*domain.Run, error) {
	var r domain.Run
	err := q.Transaction(func(tx *gorm.DB) error {
		err := tx.Table(runs).Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", domain.RunQueued).Order("created_at").Take(&r).Error
		if err != nil {
			return err
		}
		r.Status, r.StartedAt = domain.RunRunning, &now
		return tx.Table(runs).Where("id = ?", r.ID).Updates(map[string]any{"status": r.Status, "started_at": now}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

func (s *Store) SaveRun(q *gorm.DB, r *domain.Run) error {
	return q.Table(runs).Where("id = ?", r.ID).Select("*").Updates(r).Error
}

func (s *Store) InsertItem(q *gorm.DB, it *domain.Item) error {
	return q.Table(items).Create(it).Error
}

func (s *Store) Items(q *gorm.DB, runID string) ([]domain.Item, error) {
	var out []domain.Item
	err := q.Table(items).Where("run_id = ?", runID).Order("id").Find(&out).Error
	return out, err
}

var ErrItemNotFound = apperror.NotFound("reconciliation_item_not_found", "reconciliation item not found")

// ItemFilter: the work queue of differences.
type ItemFilter struct {
	Status   string // open (default), auto_resolved, resolved, all
	Issue    string
	Severity string
	Limit    int
}

func (s *Store) ListItems(q *gorm.DB, f ItemFilter) ([]domain.Item, error) {
	q = q.Table(items)
	if f.Status == "" {
		f.Status = domain.ItemOpen
	}
	if f.Status != "all" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Issue != "" {
		q = q.Where("issue = ?", f.Issue)
	}
	if f.Severity != "" {
		q = q.Where("severity = ?", f.Severity)
	}
	var out []domain.Item
	err := q.Order("created_at DESC, id DESC").Limit(f.Limit).Find(&out).Error
	return out, err
}

// Resolve closes an open item with who did it and why.
func (s *Store) Resolve(q *gorm.DB, id int64, status, resolution, by string, now time.Time) (*domain.Item, error) {
	res := q.Table(items).Where("id = ? AND status = ?", id, domain.ItemOpen).
		Updates(map[string]any{"status": status, "resolution": resolution, "resolved_by": by, "resolved_at": now})
	if res.Error != nil {
		return nil, res.Error
	}
	var it domain.Item
	if err := q.Table(items).Where("id = ?", id).Take(&it).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrItemNotFound
	} else if err != nil {
		return nil, err
	}
	if res.RowsAffected == 0 {
		return &it, apperror.Conflict("item_already_resolved", "this item is already "+it.Status)
	}
	return &it, nil
}

// AutoResolveOpen closes open items of an issue for these references (e.g.
// "settlement_missing" once a later settlement contains the payment).
func (s *Store) AutoResolveOpen(q *gorm.DB, issue string, refs []string, resolution string, now time.Time) (int64, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	res := q.Table(items).Where("issue = ? AND status = ? AND reference IN ?", issue, domain.ItemOpen, refs).
		Updates(map[string]any{"status": domain.ItemAutoResolved, "resolution": resolution, "resolved_by": "reconciliation", "resolved_at": now})
	return res.RowsAffected, res.Error
}

// HasOpen: an open item already tracks this issue for the reference (a
// difference found again tomorrow is not reported twice).
func (s *Store) HasOpen(q *gorm.DB, issue, ref string) (bool, error) {
	var n int64
	err := q.Table(items).Where("issue = ? AND status = ? AND reference = ?", issue, domain.ItemOpen, ref).Count(&n).Error
	return n > 0, err
}

// ---- Settlements ----

type Settlement struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	SettlementDate  time.Time `gorm:"type:date" json:"settlement_date"`
	FileName        string    `json:"file_name"`
	SHA256          string    `gorm:"column:sha256" json:"sha256"`
	LineCount       int       `json:"line_count"`
	DepositAmount   *int64    `json:"deposit_amount,omitempty"`
	DepositCurrency *string   `json:"deposit_currency,omitempty"`
	UploadedBy      string    `json:"uploaded_by"`
	CreatedAt       time.Time `json:"created_at"`
}

func (Settlement) TableName() string { return "settlements" }

type SettlementLine struct {
	ID           int64 `gorm:"primaryKey;autoIncrement"`
	SettlementID string
	LineNo       int
	Reference    string
	Kind         string
	Amount       int64
	Fee          int64
	Currency     string
	SettledAt    *time.Time
}

func (SettlementLine) TableName() string { return "settlement_lines" }

var ErrDuplicateSettlement = apperror.Conflict("settlement_already_uploaded", "this settlement file was already uploaded")

// InsertSettlement stores the file's header and lines in one transaction.
func (s *Store) InsertSettlement(q *gorm.DB, st *Settlement, lines []domain.Line) error {
	return q.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(st).Error; errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrDuplicateSettlement
		} else if err != nil {
			return err
		}
		rows := make([]SettlementLine, len(lines))
		for i, l := range lines {
			rows[i] = SettlementLine{SettlementID: st.ID, LineNo: l.LineNo, Reference: l.Reference, Kind: l.Kind,
				Amount: l.Amount, Fee: l.Fee, Currency: l.Currency, SettledAt: l.SettledAt}
		}
		return tx.CreateInBatches(rows, 500).Error
	})
}

func (s *Store) GetSettlement(q *gorm.DB, id string) (*Settlement, error) {
	var st Settlement
	err := q.Where("id = ?", id).Take(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperror.NotFound("settlement_not_found", "settlement not found")
	}
	return &st, err
}

func (s *Store) ListSettlements(q *gorm.DB, limit int) ([]Settlement, error) {
	var out []Settlement
	err := q.Order("created_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (s *Store) SettlementLines(q *gorm.DB, settlementID string) ([]SettlementLine, error) {
	var out []SettlementLine
	err := q.Where("settlement_id = ?", settlementID).Order("line_no").Find(&out).Error
	return out, err
}

// SettledElsewhere: which of refs (lines of kind) appear in other settlements.
func (s *Store) SettledElsewhere(q *gorm.DB, kind string, refs []string, exceptSettlement string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(refs) == 0 {
		return out, nil
	}
	var found []string
	err := q.Model(&SettlementLine{}).Distinct("reference").
		Where("kind = ? AND reference IN ? AND settlement_id <> ?", kind, refs, exceptSettlement).Pluck("reference", &found).Error
	for _, r := range found {
		out[r] = true
	}
	return out, err
}
