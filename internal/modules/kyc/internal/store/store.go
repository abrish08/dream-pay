// Package store: the KYC service's tables (GORM models, auto-migrated).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/kyc/internal/domain"
	"paygate/internal/platform/apperror"
)

var ErrDocNotFound = apperror.NotFound("kyc_document_not_found", "document not found")

// kyc_applications: one per merchant. Personal data of the owner is stored
// encrypted (owner_encrypted); business data is plain for search and review.
type applicationRec struct {
	MerchantID         string     `gorm:"type:uuid;primaryKey"`
	Status             string     `gorm:"type:text;not null;index:kyc_applications_status_idx,priority:1"`
	BusinessType       string     `gorm:"type:text;not null;default:''"`
	LegalName          string     `gorm:"type:text;not null;default:''"`
	TradeName          string     `gorm:"type:text;not null;default:''"`
	RegistrationNumber string     `gorm:"type:text;not null;default:''"`
	TIN                string     `gorm:"column:tin;type:text;not null;default:''"`
	TradeLicenseNumber string     `gorm:"type:text;not null;default:''"`
	Industry           string     `gorm:"type:text;not null;default:''"`
	Website            string     `gorm:"type:text;not null;default:''"`
	Description        string     `gorm:"type:text;not null;default:''"`
	MonthlyVolumeETB   int64      `gorm:"column:monthly_volume_etb;not null;default:0"`
	Address            string     `gorm:"type:jsonb;not null;default:'{}'"`
	Details            string     `gorm:"type:jsonb;not null;default:'{}'"` // license expiry, VAT, applicant role, flags
	OwnerEncrypted     string     `gorm:"type:text;not null;default:''"`
	FaydaEncrypted     string     `gorm:"type:text;not null;default:''"` // identity confirmed by Fayda (encrypted)
	FaydaSubject       string     `gorm:"type:text;not null;default:'';index:kyc_applications_fayda_sub_idx"`
	FaydaVerifiedAt    *time.Time `gorm:"type:timestamptz"`
	SubmittedAt        *time.Time `gorm:"type:timestamptz;index:kyc_applications_status_idx,priority:2"`
	ReviewedAt         *time.Time `gorm:"type:timestamptz"`
	ReviewedBy         string     `gorm:"type:text;not null;default:''"`
	RejectionReason    string     `gorm:"type:text;not null;default:''"`
	CreatedAt          time.Time  `gorm:"type:timestamptz;not null"`
	UpdatedAt          time.Time  `gorm:"type:timestamptz;not null"`
}

func (applicationRec) TableName() string { return "kyc_applications" }

// kyc_documents: metadata only; the (encrypted) file lives in file storage
// under the document id.
type documentRec struct {
	ID          string    `gorm:"type:uuid;primaryKey"`
	MerchantID  string    `gorm:"type:uuid;not null;index:kyc_documents_merchant_idx"`
	Type        string    `gorm:"type:text;not null"`
	FileName    string    `gorm:"type:text;not null"`
	ContentType string    `gorm:"type:text;not null"`
	Size        int64     `gorm:"not null"`
	SHA256      string    `gorm:"column:sha256;type:text;not null"`
	ObjectKey   string    `gorm:"type:text;not null;default:''"` // object storage key; never a URL
	UploadedBy  string    `gorm:"type:text;not null;default:''"`
	CreatedAt   time.Time `gorm:"type:timestamptz;not null"`
}

func (documentRec) TableName() string { return "kyc_documents" }

// kyc_history: who did what, when (compliance trail; never edited).
type historyRec struct {
	ID         string    `gorm:"type:uuid;primaryKey"`
	MerchantID string    `gorm:"type:uuid;not null;index:kyc_history_merchant_idx"`
	Action     string    `gorm:"type:text;not null"`
	Actor      string    `gorm:"type:text;not null;default:''"`
	Note       string    `gorm:"type:text;not null;default:''"`
	CreatedAt  time.Time `gorm:"type:timestamptz;not null"`
}

func (historyRec) TableName() string { return "kyc_history" }

// fayda_sessions: one sign-in attempt at Fayda. Only a hash of the state is
// stored; the PKCE verifier is encrypted; single use, 10 minutes.
type faydaSessionRec struct {
	StateHash   string     `gorm:"type:text;primaryKey"`
	MerchantID  string     `gorm:"type:uuid;not null"`
	UserID      string     `gorm:"type:text;not null"`
	VerifierEnc string     `gorm:"type:text;not null"`
	Nonce       string     `gorm:"type:text;not null"`
	ExpiresAt   time.Time  `gorm:"type:timestamptz;not null"`
	UsedAt      *time.Time `gorm:"type:timestamptz"`
	CreatedAt   time.Time  `gorm:"type:timestamptz;not null"`
}

func (faydaSessionRec) TableName() string { return "fayda_sessions" }

func Models() []any {
	return []any{&applicationRec{}, &documentRec{}, &historyRec{}, &faydaSessionRec{}}
}

type details struct {
	TradeLicenseExpiry string `json:"trade_license_expiry"`
	VATRegistered      bool   `json:"vat_registered"`
	VATNumber          string `json:"vat_number"`
	ApplicantRole      string `json:"applicant_role"`
	ForeignOwned       bool   `json:"foreign_owned"`
	RegulatedSector    bool   `json:"regulated_sector"`
}

// Codec encrypts the owner's personal data.
type Codec interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type Store struct {
	db    *gorm.DB
	codec Codec
}

func New(db *gorm.DB, codec Codec) *Store { return &Store{db: db, codec: codec} }

func (s *Store) Tx(ctx context.Context, fn func(*Store) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return fn(&Store{db: tx, codec: s.codec}) })
}

func (s *Store) DB() *gorm.DB { return s.db }

func (s *Store) q(ctx context.Context) *gorm.DB { return s.db.WithContext(ctx) }

func (s *Store) toRec(a *domain.Application) (applicationRec, error) {
	addr, _ := json.Marshal(a.Profile.Address)
	det, _ := json.Marshal(details{a.Profile.TradeLicenseExpiry, a.Profile.VATRegistered, a.Profile.VATNumber,
		a.Profile.ApplicantRole, a.Profile.ForeignOwned, a.Profile.RegulatedSector})
	owner, _ := json.Marshal(a.Profile.Owner)
	enc, err := s.codec.Encrypt(string(owner))
	if err != nil {
		return applicationRec{}, err
	}
	var faydaEnc, faydaSub string
	var faydaAt *time.Time
	if a.Fayda != nil {
		fb, _ := json.Marshal(a.Fayda)
		if faydaEnc, err = s.codec.Encrypt(string(fb)); err != nil {
			return applicationRec{}, err
		}
		faydaSub, faydaAt = a.Fayda.Subject, &a.Fayda.VerifiedAt
	}
	p := a.Profile
	return applicationRec{MerchantID: a.MerchantID, Status: a.Status, BusinessType: p.BusinessType, LegalName: p.LegalName,
		TradeName: p.TradeName, RegistrationNumber: p.RegistrationNumber, TIN: p.TIN, TradeLicenseNumber: p.TradeLicenseNumber,
		Industry: p.Industry, Website: p.Website, Description: p.Description, MonthlyVolumeETB: p.MonthlyVolumeETB,
		Address: string(addr), Details: string(det), OwnerEncrypted: enc, FaydaEncrypted: faydaEnc, FaydaSubject: faydaSub,
		FaydaVerifiedAt: faydaAt, SubmittedAt: a.SubmittedAt, ReviewedAt: a.ReviewedAt,
		ReviewedBy: a.ReviewedBy, RejectionReason: a.RejectionReason, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}, nil
}

func (s *Store) fromRec(r applicationRec) (*domain.Application, error) {
	a := &domain.Application{MerchantID: r.MerchantID, Status: r.Status, SubmittedAt: r.SubmittedAt, ReviewedAt: r.ReviewedAt,
		ReviewedBy: r.ReviewedBy, RejectionReason: r.RejectionReason, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		Profile: domain.Profile{BusinessType: r.BusinessType, LegalName: r.LegalName, TradeName: r.TradeName,
			RegistrationNumber: r.RegistrationNumber, TIN: r.TIN, TradeLicenseNumber: r.TradeLicenseNumber,
			Industry: r.Industry, Website: r.Website, Description: r.Description, MonthlyVolumeETB: r.MonthlyVolumeETB}}
	_ = json.Unmarshal([]byte(r.Address), &a.Profile.Address)
	var d details
	_ = json.Unmarshal([]byte(r.Details), &d)
	a.Profile.TradeLicenseExpiry, a.Profile.VATRegistered, a.Profile.VATNumber = d.TradeLicenseExpiry, d.VATRegistered, d.VATNumber
	a.Profile.ApplicantRole, a.Profile.ForeignOwned, a.Profile.RegulatedSector = d.ApplicantRole, d.ForeignOwned, d.RegulatedSector
	if r.OwnerEncrypted != "" {
		plain, err := s.codec.Decrypt(r.OwnerEncrypted)
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(plain), &a.Profile.Owner)
	}
	if r.FaydaEncrypted != "" {
		plain, err := s.codec.Decrypt(r.FaydaEncrypted)
		if err != nil {
			return nil, err
		}
		var id domain.FaydaIdentity
		if err := json.Unmarshal([]byte(plain), &id); err != nil {
			return nil, err
		}
		a.Fayda = &id
		a.Profile.OwnerIdentityLocked = true
	}
	return a, nil
}

// Application returns the merchant's application (nil if none); lock for updates.
func (s *Store) Application(ctx context.Context, merchantID string, lock bool) (*domain.Application, error) {
	if _, err := uuid.Parse(merchantID); err != nil {
		return nil, nil
	}
	q := s.q(ctx)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var r applicationRec
	err := q.Where("merchant_id = ?", merchantID).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.fromRec(r)
}

func (s *Store) SaveApplication(ctx context.Context, a *domain.Application) error {
	r, err := s.toRec(a)
	if err != nil {
		return err
	}
	return s.q(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error
}

// List: applications by status, oldest submission first (review queue).
func (s *Store) List(ctx context.Context, status, startingAfter string, limit int) ([]*domain.Application, bool, error) {
	q := s.q(ctx)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if startingAfter != "" {
		if _, err := uuid.Parse(startingAfter); err != nil {
			return nil, false, apperror.Invalid("invalid_cursor", "starting_after must be a merchant id")
		}
		q = q.Where("(updated_at, merchant_id) > (SELECT updated_at, merchant_id FROM kyc_applications WHERE merchant_id = ?)", startingAfter)
	}
	var recs []applicationRec
	if err := q.Order("updated_at, merchant_id").Limit(limit + 1).Find(&recs).Error; err != nil {
		return nil, false, err
	}
	more := len(recs) > limit
	if more {
		recs = recs[:limit]
	}
	out := make([]*domain.Application, 0, len(recs))
	for _, r := range recs {
		a, err := s.fromRec(r)
		if err != nil {
			return nil, false, err
		}
		out = append(out, a)
	}
	return out, more, nil
}

func (s *Store) Documents(ctx context.Context, merchantID string) ([]domain.Document, error) {
	var recs []documentRec
	if err := s.q(ctx).Where("merchant_id = ?", merchantID).Order("created_at").Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Document, len(recs))
	for i, r := range recs {
		out[i] = domain.Document(r)
	}
	return out, nil
}

func (s *Store) Document(ctx context.Context, merchantID, id string) (*domain.Document, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrDocNotFound
	}
	var r documentRec
	err := s.q(ctx).Where("id = ? AND merchant_id = ?", id, merchantID).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrDocNotFound
	}
	if err != nil {
		return nil, err
	}
	d := domain.Document(r)
	return &d, nil
}

func (s *Store) AddDocument(ctx context.Context, d domain.Document) error {
	r := documentRec(d)
	return s.q(ctx).Create(&r).Error
}

func (s *Store) DeleteDocument(ctx context.Context, merchantID, id string) error {
	res := s.q(ctx).Where("id = ? AND merchant_id = ?", id, merchantID).Delete(&documentRec{})
	if res.Error == nil && res.RowsAffected == 0 {
		return ErrDocNotFound
	}
	return res.Error
}

func (s *Store) CountDocuments(ctx context.Context, merchantID string) (int64, error) {
	var n int64
	return n, s.q(ctx).Model(&documentRec{}).Where("merchant_id = ?", merchantID).Count(&n).Error
}

type HistoryEntry struct {
	Action    string    `json:"action"`
	Actor     string    `json:"actor,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) Record(ctx context.Context, merchantID, action, actor, note string, now time.Time) error {
	return s.q(ctx).Create(&historyRec{ID: uuid.NewString(), MerchantID: merchantID, Action: action, Actor: actor,
		Note: note, CreatedAt: now}).Error
}

func (s *Store) History(ctx context.Context, merchantID string) ([]HistoryEntry, error) {
	var recs []historyRec
	if err := s.q(ctx).Where("merchant_id = ?", merchantID).Order("created_at").Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]HistoryEntry, len(recs))
	for i, r := range recs {
		out[i] = HistoryEntry{Action: r.Action, Actor: r.Actor, Note: r.Note, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

// ---- Fayda sign-in sessions ----

type FaydaSession struct {
	MerchantID, UserID, VerifierEnc, Nonce string
}

func (s *Store) CreateFaydaSession(ctx context.Context, stateHash string, fs FaydaSession, expires, now time.Time) error {
	return s.q(ctx).Create(&faydaSessionRec{StateHash: stateHash, MerchantID: fs.MerchantID, UserID: fs.UserID,
		VerifierEnc: fs.VerifierEnc, Nonce: fs.Nonce, ExpiresAt: expires, CreatedAt: now}).Error
}

// UseFaydaSession spends an open session (single use); nil if none.
func (s *Store) UseFaydaSession(ctx context.Context, stateHash string, now time.Time) (*FaydaSession, error) {
	var r faydaSessionRec
	err := s.q(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("state_hash = ? AND used_at IS NULL AND expires_at > ?", stateHash, now).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.q(ctx).Model(&faydaSessionRec{}).Where("state_hash = ?", stateHash).Update("used_at", now).Error; err != nil {
		return nil, err
	}
	return &FaydaSession{MerchantID: r.MerchantID, UserID: r.UserID, VerifierEnc: r.VerifierEnc, Nonce: r.Nonce}, nil
}
