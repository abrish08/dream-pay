package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/contracts/disputes"
	"paygate/internal/modules/dispute/internal/domain"
	"paygate/internal/platform/apperror"
)

type disputeRec struct {
	ID                  string    `gorm:"type:uuid;primaryKey"`
	PaymentID           string    `gorm:"type:uuid;not null;index"`
	MerchantID          string    `gorm:"type:uuid;not null;index:dispute_merchant,priority:1"`
	Environment         string    `gorm:"not null"`
	Provider            string    `gorm:"not null"`
	Amount              int64     `gorm:"not null"`
	Currency            string    `gorm:"not null"`
	Status              string    `gorm:"not null;index:dispute_status_due,priority:1"`
	ReasonCode          string    `gorm:"not null"`
	Reason              string    `gorm:"not null;default:''"`
	ProviderCaseID      string    `gorm:"not null;default:''"`
	EvidenceDueBy       time.Time `gorm:"not null;index:dispute_status_due,priority:2"`
	EvidenceText        string    `gorm:"not null;default:''"`
	EvidenceSubmittedAt *time.Time
	Outcome             string `gorm:"not null;default:''"`
	Fee                 int64  `gorm:"not null;default:0"`
	Note                string `gorm:"not null;default:''"`
	ClosedBy            string `gorm:"not null;default:''"`
	ClosedAt            *time.Time
	CreatedAt           time.Time `gorm:"not null;index:dispute_merchant,priority:2"`
	UpdatedAt           time.Time `gorm:"not null"`
}

func (disputeRec) TableName() string { return "disputes" }

// File: one piece of evidence (receipt, delivery proof, chat log...).
type File struct {
	ID          string    `gorm:"type:uuid;primaryKey" json:"id"`
	DisputeID   string    `gorm:"type:uuid;not null;index" json:"-"`
	Name        string    `gorm:"not null" json:"name"`
	ContentType string    `gorm:"not null" json:"content_type"`
	Size        int       `gorm:"not null" json:"size"`
	SHA256      string    `gorm:"column:sha256;not null" json:"sha256"`
	Data        []byte    `gorm:"not null" json:"-"`
	UploadedBy  string    `gorm:"not null" json:"uploaded_by"`
	CreatedAt   time.Time `gorm:"not null" json:"created_at"`
}

func (File) TableName() string { return "dispute_files" }

// Event: the dispute's history (who did what).
type Event struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	DisputeID string    `gorm:"type:uuid;not null;index" json:"-"`
	Action    string    `gorm:"not null" json:"action"`
	Actor     string    `gorm:"not null" json:"actor"`
	Note      string    `gorm:"not null;default:''" json:"note,omitempty"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

func (Event) TableName() string { return "dispute_events" }

func Models() []any { return []any{&disputeRec{}, &File{}, &Event{}} }

var ErrNotFound = apperror.NotFound("dispute_not_found", "dispute not found")

type Store struct{}

func New() *Store { return &Store{} }

func (s *Store) Insert(q *gorm.DB, d *domain.Dispute) error {
	r := disputeRec(*d)
	return q.Create(&r).Error
}

func (s *Store) Save(q *gorm.DB, d *domain.Dispute) error {
	r := disputeRec(*d)
	return q.Select("*").Updates(&r).Error
}

func (s *Store) Get(q *gorm.DB, id string, lock bool) (*domain.Dispute, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var r disputeRec
	err := q.Where("id = ?", id).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	d := domain.Dispute(r)
	return &d, err
}

func (s *Store) List(q *gorm.DB, merchantID, status string, limit int) ([]domain.Dispute, error) {
	if merchantID != "" {
		q = q.Where("merchant_id = ?", merchantID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var rs []disputeRec
	if err := q.Order("created_at DESC").Limit(limit).Find(&rs).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Dispute, len(rs))
	for i, r := range rs {
		out[i] = domain.Dispute(r)
	}
	return out, nil
}

// DisputedAmount: what is already disputed on a payment (open or lost).
func (s *Store) DisputedAmount(q *gorm.DB, paymentID string) (int64, error) {
	var v int64
	err := q.Model(&disputeRec{}).Select("COALESCE(SUM(amount), 0)").
		Where("payment_id = ? AND status <> ?", paymentID, disputes.StatusWon).Scan(&v).Error
	return v, err
}

// Overdue: disputes still waiting for evidence after their deadline.
func (s *Store) Overdue(q *gorm.DB, now time.Time, limit int) ([]string, error) {
	var ids []string
	err := q.Model(&disputeRec{}).Where("status = ? AND evidence_due_by <= ?", disputes.StatusNeedsResponse, now).
		Order("evidence_due_by").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}

func (s *Store) AddFile(q *gorm.DB, f *File) error { return q.Create(f).Error }

func (s *Store) Files(q *gorm.DB, disputeID string) ([]File, error) {
	var out []File
	return out, q.Omit("data").Where("dispute_id = ?", disputeID).Order("created_at").Find(&out).Error
}

func (s *Store) File(q *gorm.DB, disputeID, fileID string) (*File, error) {
	if _, err := uuid.Parse(fileID); err != nil {
		return nil, apperror.NotFound("file_not_found", "file not found")
	}
	var f File
	err := q.Where("id = ? AND dispute_id = ?", fileID, disputeID).Take(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperror.NotFound("file_not_found", "file not found")
	}
	return &f, err
}

func (s *Store) Record(q *gorm.DB, disputeID, action, actor, note string, now time.Time) error {
	return q.Create(&Event{DisputeID: disputeID, Action: action, Actor: actor, Note: note, CreatedAt: now}).Error
}

func (s *Store) History(q *gorm.DB, disputeID string) ([]Event, error) {
	var out []Event
	return out, q.Where("dispute_id = ?", disputeID).Order("id").Find(&out).Error
}
