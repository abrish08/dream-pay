package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/contracts/disputes"
	"paygate/internal/contracts/payments"
	"paygate/internal/modules/dispute/internal/domain"
	"paygate/internal/modules/dispute/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
)

type Service struct {
	db     *gorm.DB
	store  *store.Store
	events events.Publisher
	bus    bus.Bus
	log    *slog.Logger
	now    func() time.Time
	// DeadlineEvery: how often overdue disputes are closed (default 5 minutes).
	DeadlineEvery time.Duration
}

func New(db *gorm.DB, pub events.Publisher, b bus.Bus, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), events: pub, bus: b, log: log, now: func() time.Time { return time.Now().UTC() }}
}

type View struct {
	ID                  string        `json:"id"`
	PaymentID           string        `json:"payment_id"`
	MerchantID          string        `json:"merchant_id"`
	Environment         string        `json:"environment"`
	Amount              int64         `json:"amount"`
	Currency            string        `json:"currency"`
	Status              string        `json:"status"`
	ReasonCode          string        `json:"reason_code"`
	Reason              string        `json:"reason,omitempty"`
	ProviderCaseID      string        `json:"provider_case_id,omitempty"`
	EvidenceDueBy       time.Time     `json:"evidence_due_by"`
	EvidenceText        string        `json:"evidence_text,omitempty"`
	EvidenceSubmittedAt *time.Time    `json:"evidence_submitted_at,omitempty"`
	Files               []store.File  `json:"files"`
	Outcome             string        `json:"outcome,omitempty"`
	Fee                 int64         `json:"fee,omitempty"`
	Note                string        `json:"note,omitempty"`
	ClosedAt            *time.Time    `json:"closed_at,omitempty"`
	CreatedAt           time.Time     `json:"created_at"`
	History             []store.Event `json:"history,omitempty"`
}

func (s *Service) view(q *gorm.DB, d *domain.Dispute, history bool) (View, error) {
	files, err := s.store.Files(q, d.ID)
	if err != nil {
		return View{}, err
	}
	if files == nil {
		files = []store.File{}
	}
	v := View{ID: d.ID, PaymentID: d.PaymentID, MerchantID: d.MerchantID, Environment: d.Environment, Amount: d.Amount,
		Currency: d.Currency, Status: d.Status, ReasonCode: d.ReasonCode, Reason: d.Reason, ProviderCaseID: d.ProviderCaseID,
		EvidenceDueBy: d.EvidenceDueBy, EvidenceText: d.EvidenceText, EvidenceSubmittedAt: d.EvidenceSubmittedAt, Files: files,
		Outcome: d.Outcome, Fee: d.Fee, Note: d.Note, ClosedAt: d.ClosedAt, CreatedAt: d.CreatedAt}
	if history {
		v.History, err = s.store.History(q, d.ID)
	}
	return v, err
}

type OpenInput struct {
	PaymentID      string    `json:"payment_id"`
	Amount         int64     `json:"amount"` // 0 = everything still disputable
	ReasonCode     string    `json:"reason_code"`
	Reason         string    `json:"reason"`
	ProviderCaseID string    `json:"provider_case_id"`
	EvidenceDueBy  time.Time `json:"evidence_due_by"` // default: 7 days
}

// Open records a dispute the provider notified us of (staff). The amount
// leaves the merchant's balance at once (DisputeOpened → ledger).
func (s *Service) Open(ctx context.Context, by string, in OpenInput) (View, error) {
	ps, err := bus.Ask(ctx, s.bus, payments.GetPaymentsByID{IDs: []string{in.PaymentID}})
	if err != nil {
		return View{}, err
	}
	if len(ps) == 0 {
		return View{}, payments.ErrNotFound
	}
	p := ps[0]
	if p.Status != payments.StatusSucceeded && p.Status != payments.StatusPartiallyRefunded {
		return View{}, apperror.Unprocessable("payment_not_disputable", "only a captured payment (not fully refunded) can be disputed")
	}
	if in.EvidenceDueBy.IsZero() {
		in.EvidenceDueBy = s.now().AddDate(0, 0, 7)
	}
	var d *domain.Dispute
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// One payment at a time: concurrent disputes cannot exceed it.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "dispute:"+p.ID).Error; err != nil {
			return err
		}
		disputed, err := s.store.DisputedAmount(tx, p.ID)
		if err != nil {
			return err
		}
		left := p.Amount - p.RefundedAmount - disputed
		if in.Amount == 0 {
			in.Amount = left
		}
		if d, err = domain.Open(domain.OpenInput{PaymentID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
			Provider: p.Provider, Currency: p.Currency, Amount: in.Amount, Disputable: left, ReasonCode: in.ReasonCode,
			Reason: in.Reason, ProviderCaseID: in.ProviderCaseID, EvidenceDueBy: in.EvidenceDueBy.UTC()}, s.now()); err != nil {
			return err
		}
		if err := s.store.Insert(tx, d); err != nil {
			return err
		}
		if err := s.store.Record(tx, d.ID, "opened", by, d.ReasonCode+": "+d.Reason, s.now()); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, disputes.DisputeOpened{DisputeID: d.ID, PaymentID: d.PaymentID, MerchantID: d.MerchantID,
			Environment: d.Environment, Amount: d.Amount, Currency: d.Currency, Provider: d.Provider, Reason: d.ReasonCode,
			EvidenceDueBy: d.EvidenceDueBy, OccurredAt: s.now()})
	})
	if err != nil {
		return View{}, err
	}
	return s.view(s.db.WithContext(ctx), d, true)
}

// Get: merchantID "" = staff (any merchant).
func (s *Service) Get(ctx context.Context, merchantID, id string) (View, error) {
	d, err := s.load(ctx, s.db.WithContext(ctx), merchantID, id, false)
	if err != nil {
		return View{}, err
	}
	return s.view(s.db.WithContext(ctx), d, true)
}

func (s *Service) List(ctx context.Context, merchantID, status string, limit int) ([]View, error) {
	ds, err := s.store.List(s.db.WithContext(ctx), merchantID, status, limit)
	if err != nil {
		return nil, err
	}
	out := make([]View, len(ds))
	for i := range ds {
		if out[i], err = s.view(s.db.WithContext(ctx), &ds[i], false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) load(ctx context.Context, q *gorm.DB, merchantID, id string, lock bool) (*domain.Dispute, error) {
	d, err := s.store.Get(q, id, lock)
	if err != nil {
		return nil, err
	}
	if merchantID != "" && d.MerchantID != merchantID {
		return nil, store.ErrNotFound
	}
	return d, nil
}

// Upload is one evidence file.
type Upload struct {
	Name string
	Data []byte
}

// AddEvidence: the merchant's explanation and files (receipts, delivery
// proof...). Can be repeated until submitted or the deadline.
func (s *Service) AddEvidence(ctx context.Context, merchantID, by, id, text string, files []Upload) (View, error) {
	var d *domain.Dispute
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if d, err = s.load(ctx, tx, merchantID, id, true); err != nil {
			return err
		}
		if err := d.AddEvidence(text, s.now()); err != nil {
			return err
		}
		have, err := s.store.Files(tx, d.ID)
		if err != nil {
			return err
		}
		if len(have)+len(files) > domain.MaxFiles {
			return apperror.Invalid("too_many_files", "at most 5 evidence files per dispute")
		}
		for _, f := range files {
			ct := http.DetectContentType(f.Data)
			if ct != "application/pdf" && ct != "image/jpeg" && ct != "image/png" {
				return apperror.Invalid("invalid_file", f.Name+": evidence files are PDF, JPEG or PNG")
			}
			if len(f.Data) > domain.MaxFileBytes {
				return apperror.TooLarge("file_too_large", f.Name+": files are at most 5 MB")
			}
			sum := sha256.Sum256(f.Data)
			name := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(filepath.Base(f.Name))
			if err := s.store.AddFile(tx, &store.File{ID: uuid.NewString(), DisputeID: d.ID, Name: name, ContentType: ct,
				Size: len(f.Data), SHA256: hex.EncodeToString(sum[:]), Data: f.Data, UploadedBy: by, CreatedAt: s.now()}); err != nil {
				return err
			}
		}
		if err := s.store.Save(tx, d); err != nil {
			return err
		}
		return s.store.Record(tx, d.ID, "evidence_added", by, "", s.now())
	})
	if err != nil {
		return View{}, err
	}
	return s.view(s.db.WithContext(ctx), d, true)
}

// Submit sends the merchant's evidence for review.
func (s *Service) Submit(ctx context.Context, merchantID, by, id string) (View, error) {
	return s.change(ctx, merchantID, id, "submitted", by, "", func(tx *gorm.DB, d *domain.Dispute) (bus.Event, error) {
		files, err := s.store.Files(tx, d.ID)
		if err != nil {
			return nil, err
		}
		return nil, d.Submit(len(files), s.now())
	})
}

// Accept: the merchant does not contest; the dispute is lost.
func (s *Service) Accept(ctx context.Context, merchantID, by, id string) (View, error) {
	return s.Close(ctx, merchantID, by, id, disputes.OutcomeLost, 0, "accepted by the merchant")
}

// Close records the issuer's decision (staff), or an accepted / overdue loss.
func (s *Service) Close(ctx context.Context, merchantID, by, id, outcome string, fee int64, note string) (View, error) {
	return s.change(ctx, merchantID, id, outcome, by, note, func(_ *gorm.DB, d *domain.Dispute) (bus.Event, error) {
		if err := d.Close(outcome, fee, note, by, s.now()); err != nil {
			return nil, err
		}
		return disputes.DisputeClosed{DisputeID: d.ID, PaymentID: d.PaymentID, MerchantID: d.MerchantID, Environment: d.Environment,
			Amount: d.Amount, Currency: d.Currency, Provider: d.Provider, Outcome: outcome, Fee: fee, Note: d.Note, OccurredAt: s.now()}, nil
	})
}

func (s *Service) change(ctx context.Context, merchantID, id, action, by, note string,
	fn func(*gorm.DB, *domain.Dispute) (bus.Event, error)) (View, error) {
	var d *domain.Dispute
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if d, err = s.load(ctx, tx, merchantID, id, true); err != nil {
			return err
		}
		evt, err := fn(tx, d)
		if err != nil {
			return err
		}
		if err := s.store.Save(tx, d); err != nil {
			return err
		}
		if err := s.store.Record(tx, d.ID, action, by, note, s.now()); err != nil {
			return err
		}
		if evt != nil {
			return s.events.Publish(ctx, tx, evt)
		}
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.view(s.db.WithContext(ctx), d, true)
}

// File returns an evidence file (merchant: own disputes; staff: any).
func (s *Service) File(ctx context.Context, merchantID, id, fileID string) (*store.File, error) {
	d, err := s.load(ctx, s.db.WithContext(ctx), merchantID, id, false)
	if err != nil {
		return nil, err
	}
	return s.store.File(s.db.WithContext(ctx), d.ID, fileID)
}

// DeadlineWorker: a dispute without evidence by its deadline is lost (as
// card networks rule).
func (s *Service) DeadlineWorker(ctx context.Context) {
	every := s.DeadlineEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ids, err := s.store.Overdue(s.db.WithContext(ctx), s.now(), 50)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("overdue disputes", "err", err)
			}
			continue
		}
		for _, id := range ids {
			if _, err := s.Close(ctx, "", "system", id, disputes.OutcomeLost, 0, "no evidence before the deadline"); err != nil {
				s.log.Warn("close overdue dispute", "dispute_id", id, "err", err)
			}
		}
	}
}
