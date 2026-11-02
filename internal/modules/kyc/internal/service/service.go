// Package service: merchant verification (KYC / KYB).
//
// The merchant fills in the business profile and uploads the documents its
// legal form needs, then submits. Compliance staff review (documents are
// downloaded through the API, every view is recorded) and approve or reject
// with a reason. Approval publishes kyc.status_changed; the merchant service
// then activates the merchant (live payments, live keys, payouts).
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/kyc"
	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/kyc/internal/domain"
	"paygate/internal/modules/kyc/internal/fayda"
	"paygate/internal/modules/kyc/internal/storage"
	"paygate/internal/modules/kyc/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/mail"
	"paygate/internal/platform/reqctx"
	"paygate/internal/platform/secrets"
)

const maxDocuments = 30

// LinkTTL: download links are built on every read and expire quickly.
const LinkTTL = 5 * time.Minute

type Service struct {
	store    *store.Store
	files    storage.Files
	fayda    *fayda.Client // nil: Fayda verification not configured
	box      *secrets.Box  // encrypts the PKCE verifier of a Fayda sign-in
	dashURL  string        // where the owner returns after Fayda
	events   events.Publisher
	bus      bus.Bus
	mailer   mail.Sender
	maxBytes int64
	log      *slog.Logger
	now      func() time.Time
}

type Options struct {
	Fayda        *fayda.Client
	Box          *secrets.Box
	DashboardURL string
}

func New(st *store.Store, files storage.Files, pub events.Publisher, b bus.Bus, mailer mail.Sender, opt Options, log *slog.Logger) *Service {
	return &Service{store: st, files: files, fayda: opt.Fayda, box: opt.Box, dashURL: strings.TrimRight(opt.DashboardURL, "/"),
		events: pub, bus: b, mailer: mailer, maxBytes: domain.MaxFileBytes, log: log,
		now: func() time.Time { return time.Now().UTC() }}
}

// ---- Views ----

type DocumentView struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	FileName    string    `json:"file_name"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	UploadedAt  time.Time `json:"uploaded_at"`
	// URL: a download link built from the object key on this request; it
	// expires at URLExpiresAt and is never stored.
	URL          string     `json:"url,omitempty"`
	URLExpiresAt *time.Time `json:"url_expires_at,omitempty"`
}

// OwnerIdentity: how the owner's identity was established.
type OwnerIdentity struct {
	Method       string     `json:"method"` // fayda | document
	Verified     bool       `json:"verified"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	Name         string     `json:"name,omitempty"`
	BirthDate    string     `json:"birthdate,omitempty"`
	Gender       string     `json:"gender,omitempty"`
	Phone        string     `json:"phone_number,omitempty"`
	Nationality  string     `json:"nationality,omitempty"`
	Address      string     `json:"address,omitempty"`
	IndividualID string     `json:"individual_id,omitempty"` // masked
	PhotoURL     string     `json:"photo_url,omitempty"`     // staff only, built on read
	FaydaEnabled bool       `json:"fayda_available"`
}

type View struct {
	MerchantID        string               `json:"merchant_id"`
	Status            string               `json:"status"`
	Profile           domain.Profile       `json:"profile"`
	Documents         []DocumentView       `json:"documents"`
	RequiredDocuments []string             `json:"required_documents"`
	Missing           []string             `json:"missing"`
	OwnerIdentity     OwnerIdentity        `json:"owner_identity"`
	SubmittedAt       *time.Time           `json:"submitted_at,omitempty"`
	ReviewedAt        *time.Time           `json:"reviewed_at,omitempty"`
	RejectionReason   string               `json:"rejection_reason,omitempty"`
	History           []store.HistoryEntry `json:"history,omitempty"` // staff only
}

func (s *Service) view(ctx context.Context, a *domain.Application, staff bool) (View, error) {
	docs, err := s.store.Documents(ctx, a.MerchantID)
	if err != nil {
		return View{}, err
	}
	now := s.now()
	v := View{MerchantID: a.MerchantID, Status: a.Status, Profile: a.Profile, Documents: []DocumentView{},
		RequiredDocuments: domain.RequiredDocs(a.Profile, a.FaydaVerified()), Missing: []string{},
		SubmittedAt: a.SubmittedAt, ReviewedAt: a.ReviewedAt, RejectionReason: a.RejectionReason,
		OwnerIdentity: OwnerIdentity{Method: "document", FaydaEnabled: s.fayda != nil}}
	for _, d := range docs {
		dv := docView(d)
		if d.ObjectKey != "" { // the link is built now from the key, never read from the database
			if u, err := s.files.URL(ctx, d.ObjectKey, d.FileName, LinkTTL); err == nil {
				exp := now.Add(LinkTTL)
				dv.URL, dv.URLExpiresAt = u, &exp
			} else {
				s.log.Warn("KYC document link", "document_id", d.ID, "err", err)
			}
		}
		v.Documents = append(v.Documents, dv)
	}
	if f := a.Fayda; f != nil {
		at := f.VerifiedAt
		v.OwnerIdentity = OwnerIdentity{Method: "fayda", Verified: true, VerifiedAt: &at, Name: f.Name, BirthDate: f.BirthDate,
			Gender: f.Gender, Phone: f.Phone, Nationality: f.Nationality, Address: f.Address,
			IndividualID: domain.MaskID(f.IndividualID), FaydaEnabled: s.fayda != nil}
		if staff && f.PhotoKey != "" {
			if u, err := s.files.URL(ctx, f.PhotoKey, "fayda-photo.jpg", LinkTTL); err == nil {
				v.OwnerIdentity.PhotoURL = u
			}
		}
	}
	if a.Editable() {
		if m := a.Missing(docs, now); m != nil {
			v.Missing = m
		}
	}
	if staff {
		if v.History, err = s.store.History(ctx, a.MerchantID); err != nil {
			return View{}, err
		}
	}
	return v, nil
}

func docView(d domain.Document) DocumentView {
	return DocumentView{ID: d.ID, Type: d.Type, FileName: d.FileName, ContentType: d.ContentType, Size: d.Size,
		SHA256: d.SHA256, UploadedAt: d.CreatedAt}
}

func (s *Service) load(ctx context.Context, st *store.Store, merchantID string, lock bool) (*domain.Application, error) {
	a, err := st.Application(ctx, merchantID, lock)
	if err != nil || a != nil {
		return a, err
	}
	return domain.NewApplication(merchantID, s.now()), nil
}

// ---- Merchant side ----

func (s *Service) Get(ctx context.Context, merchantID string) (View, error) {
	a, err := s.load(ctx, s.store, merchantID, false)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, a, false)
}

func (s *Service) SaveProfile(ctx context.Context, actor reqctx.Principal, p domain.Profile) (View, error) {
	var a *domain.Application
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		if a, err = s.load(ctx, st, actor.MerchantID, true); err != nil {
			return err
		}
		if err := a.SetProfile(p, s.now()); err != nil {
			return err
		}
		if err := st.SaveApplication(ctx, a); err != nil {
			return err
		}
		return st.Record(ctx, a.MerchantID, "profile_updated", actor.Subject, "", s.now())
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, a, false)
}

// Upload stores one document. The type is decided from the bytes (PDF, JPEG,
// PNG only), the file is encrypted at rest, and its SHA-256 is kept so a
// reviewer can prove which file was approved.
func (s *Service) Upload(ctx context.Context, actor reqctx.Principal, docType, fileName string, data []byte) (DocumentView, error) {
	if !domain.ValidDocType(docType) {
		return DocumentView{}, apperror.Invalid("invalid_document_type", fmt.Sprintf("type must be one of %v", domain.DocTypes))
	}
	ct, err := domain.SniffFile(data)
	if err != nil {
		return DocumentView{}, err
	}
	sum := sha256.Sum256(data)
	now := s.now()
	d := domain.Document{ID: uuid.NewString(), MerchantID: actor.MerchantID, Type: docType,
		FileName: domain.CleanFileName(fileName, ct), ContentType: ct, Size: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), UploadedBy: actor.Subject, CreatedAt: now}
	d.ObjectKey = domain.ObjectKey(actor.MerchantID, d.ID, ct) // ids only; the URL is built on read
	err = s.store.Tx(ctx, func(st *store.Store) error {
		a, err := s.load(ctx, st, actor.MerchantID, true)
		if err != nil {
			return err
		}
		if !a.Editable() {
			return domain.ErrLocked
		}
		if n, err := st.CountDocuments(ctx, actor.MerchantID); err != nil || n >= maxDocuments {
			if err == nil {
				err = apperror.Unprocessable("too_many_documents", "delete unneeded documents first")
			}
			return err
		}
		if a.Status == kyc.StatusNotStarted {
			a.Status, a.UpdatedAt = kyc.StatusDraft, now
		}
		if err := st.SaveApplication(ctx, a); err != nil {
			return err
		}
		if err := s.files.Put(ctx, d.ObjectKey, data, ct); err != nil { // object first: a row never points at nothing
			return fmt.Errorf("store KYC document: %w", err)
		}
		if err := st.AddDocument(ctx, d); err != nil {
			_ = s.files.Delete(ctx, d.ObjectKey)
			return err
		}
		return st.Record(ctx, d.MerchantID, "document_uploaded", actor.Subject, docType+": "+d.FileName, now)
	})
	if err != nil {
		return DocumentView{}, err
	}
	return docView(d), nil
}

func (s *Service) DeleteDocument(ctx context.Context, actor reqctx.Principal, id string) error {
	var objectKey string
	err := s.store.Tx(ctx, func(st *store.Store) error {
		a, err := s.load(ctx, st, actor.MerchantID, true)
		if err != nil {
			return err
		}
		if !a.Editable() {
			return domain.ErrLocked
		}
		d, err := st.Document(ctx, actor.MerchantID, id)
		if err != nil {
			return err
		}
		objectKey = d.ObjectKey
		if err := st.DeleteDocument(ctx, actor.MerchantID, id); err != nil {
			return err
		}
		return st.Record(ctx, actor.MerchantID, "document_deleted", actor.Subject, id, s.now())
	})
	if err != nil {
		return err
	}
	if objectKey != "" {
		if err := s.files.Delete(ctx, objectKey); err != nil {
			s.log.Warn("delete KYC object", "document_id", id, "err", err)
		}
	}
	return nil
}

func (s *Service) Submit(ctx context.Context, actor reqctx.Principal) (View, error) {
	a, err := s.decide(ctx, actor.MerchantID, actor.Subject, "submitted", "", true, func(a *domain.Application, docs []domain.Document) error {
		return a.Submit(docs, s.now())
	})
	if err != nil {
		return View{}, err
	}
	s.notify(ctx, a.MerchantID, "We received your business verification",
		"Thank you. Our compliance team is reviewing your documents; we will email you when it is done.\n")
	return s.view(ctx, a, false)
}

// ---- Staff side ----

type Page struct {
	Data       []View `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// List: the review queue (e.g. ?status=submitted), oldest first.
func (s *Service) List(ctx context.Context, status, startingAfter string, limit int) (Page, error) {
	apps, more, err := s.store.List(ctx, status, startingAfter, limit)
	if err != nil {
		return Page{}, err
	}
	page := Page{Data: make([]View, 0, len(apps)), HasMore: more}
	for _, a := range apps {
		v, err := s.view(ctx, a, false)
		if err != nil {
			return Page{}, err
		}
		page.Data = append(page.Data, v)
	}
	if more {
		page.NextCursor = apps[len(apps)-1].MerchantID
	}
	return page, nil
}

func (s *Service) Review(ctx context.Context, merchantID string) (View, error) {
	a, err := s.store.Application(ctx, merchantID, false)
	if err != nil {
		return View{}, err
	}
	if a == nil {
		return View{}, apperror.NotFound("kyc_not_found", "this merchant has not started verification")
	}
	return s.view(ctx, a, true)
}

// DocumentLink: a short-lived download link for one document, built from its
// object key now. Staff requests are recorded in the history.
type DocumentLink struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	FileName  string    `json:"file_name"`
	SHA256    string    `json:"sha256"`
}

func (s *Service) DocumentLink(ctx context.Context, actor reqctx.Principal, merchantID, id string) (DocumentLink, error) {
	d, err := s.store.Document(ctx, merchantID, id)
	if err != nil {
		return DocumentLink{}, err
	}
	if d.ObjectKey == "" {
		return DocumentLink{}, apperror.NotFound("kyc_document_missing", "this document has no stored file; upload it again")
	}
	u, err := s.files.URL(ctx, d.ObjectKey, d.FileName, LinkTTL)
	if err != nil {
		return DocumentLink{}, err
	}
	if actor.IsPlatform() {
		if err := s.store.Record(ctx, merchantID, "document_viewed", actor.Subject, d.Type, s.now()); err != nil {
			return DocumentLink{}, err
		}
	}
	return DocumentLink{URL: u, ExpiresAt: s.now().Add(LinkTTL), FileName: d.FileName, SHA256: d.SHA256}, nil
}

func (s *Service) Approve(ctx context.Context, actor reqctx.Principal, merchantID, note string) (View, error) {
	a, err := s.decide(ctx, merchantID, actor.Subject, "approved", note, false, func(a *domain.Application, _ []domain.Document) error {
		return a.Approve(actor.Subject, s.now())
	})
	if err != nil {
		return View{}, err
	}
	s.notify(ctx, merchantID, "Your business is verified",
		"Good news: your business verification was approved. Live payments, live API keys and payouts are now available.\n")
	return s.view(ctx, a, true)
}

func (s *Service) Reject(ctx context.Context, actor reqctx.Principal, merchantID, reason string) (View, error) {
	a, err := s.decide(ctx, merchantID, actor.Subject, "rejected", reason, false, func(a *domain.Application, _ []domain.Document) error {
		return a.Reject(actor.Subject, reason, s.now())
	})
	if err != nil {
		return View{}, err
	}
	s.notify(ctx, merchantID, "Your business verification needs changes",
		"We could not approve your business verification yet:\n\n    "+a.RejectionReason+
			"\n\nPlease update your details or documents in the dashboard and submit again.\n")
	return s.view(ctx, a, true)
}

// decide applies a status change under a row lock, records it and publishes
// kyc.status_changed in the same transaction (outbox).
// mayStart: the merchant's own submit works on an empty application too (and
// then reports what is missing); staff decisions need an existing one.
func (s *Service) decide(ctx context.Context, merchantID, actor, action, note string, mayStart bool,
	fn func(*domain.Application, []domain.Document) error) (*domain.Application, error) {
	var a *domain.Application
	err := s.store.Tx(ctx, func(st *store.Store) error {
		var err error
		if a, err = st.Application(ctx, merchantID, true); err != nil {
			return err
		}
		if a == nil && mayStart {
			a = domain.NewApplication(merchantID, s.now())
		}
		if a == nil {
			return apperror.NotFound("kyc_not_found", "this merchant has not started verification")
		}
		docs, err := st.Documents(ctx, merchantID)
		if err != nil {
			return err
		}
		if err := fn(a, docs); err != nil {
			return err
		}
		if err := st.SaveApplication(ctx, a); err != nil {
			return err
		}
		if err := st.Record(ctx, merchantID, action, actor, note, s.now()); err != nil {
			return err
		}
		return s.events.Publish(ctx, st.DB(), kyc.StatusChanged{MerchantID: merchantID, Status: a.Status,
			Reason: a.RejectionReason, OccurredAt: s.now()})
	})
	return a, err
}

// notify emails the merchant's contact address (in the background).
func (s *Service) notify(ctx context.Context, merchantID, subject, text string) {
	if s.mailer == nil {
		return
	}
	m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: merchantID})
	if err != nil {
		s.log.Warn("KYC email: merchant lookup failed", "merchant_id", merchantID, "err", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.mailer.Send(ctx, mail.Message{To: m.Email, Subject: subject, Text: text}); err != nil {
			s.log.Error("KYC email", "merchant_id", merchantID, "err", err)
		}
	}()
}
