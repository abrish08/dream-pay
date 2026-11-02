package domain

import (
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/kyc"
	"paygate/internal/contracts/merchants"
	"paygate/internal/platform/apperror"
)

type Merchant struct {
	ID         string
	Name       string
	Email      string
	Phone      string
	Website    string
	Status     string
	FeeBPS     int64
	KYCStatus  string
	ApprovedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DefaultFeeBPS applies to self-signed-up merchants until staff change it.
const DefaultFeeBPS = 150

type NewMerchantInput struct {
	Name, Email, Phone, Website string
	FeeBPS                      int64
	Status                      string // active (created by staff) or pending (self-signup)
}

func NewMerchant(in NewMerchantInput, now time.Time) (*Merchant, error) {
	name, email := strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email))
	feeBPS := in.FeeBPS
	if name == "" || len(name) > 200 {
		return nil, apperror.Invalid("invalid_name", "business name is required (max 200 characters)")
	}
	if len(in.Phone) > 30 || len(in.Website) > 200 {
		return nil, apperror.Invalid("invalid_contact", "phone or website too long")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, apperror.Invalid("invalid_email", "a valid email is required")
	}
	if feeBPS < 0 || feeBPS > 10_000 {
		return nil, apperror.Invalid("invalid_fee", "fee_bps must be between 0 and 10000")
	}
	m := &Merchant{
		ID: uuid.NewString(), Name: name, Email: email, Phone: strings.TrimSpace(in.Phone),
		Website: strings.TrimSpace(in.Website), Status: in.Status, FeeBPS: feeBPS,
		CreatedAt: now, UpdatedAt: now,
	}
	switch in.Status {
	case merchants.StatusActive:
		m.ApprovedAt, m.KYCStatus = &now, kyc.StatusNotStarted // usable at once in test mode; live mode needs KYC
	case merchants.StatusPending:
		m.KYCStatus = kyc.StatusNotStarted
	default:
		return nil, apperror.Invalid("invalid_status", "a new merchant is active or pending")
	}
	return m, nil
}

// SetStatus: approve (pending -> active), suspend, reactivate, or reject a
// pending merchant (-> suspended). Nothing goes back to pending.
func (m *Merchant) SetStatus(status string, now time.Time) error {
	if status != merchants.StatusActive && status != merchants.StatusSuspended {
		return apperror.Invalid("invalid_status", "status must be active or suspended")
	}
	// A merchant never approved before (self-signed-up) needs approved KYC to
	// become active, whatever its current status: suspend-then-activate must
	// not skip verification.
	if status == merchants.StatusActive && m.ApprovedAt == nil &&
		m.KYCStatus != kyc.StatusApproved && m.KYCStatus != kyc.StatusWaived {
		return ErrKYCNotApproved
	}
	if status == merchants.StatusActive && m.ApprovedAt == nil {
		m.ApprovedAt = &now
	}
	m.Status, m.UpdatedAt = status, now
	return nil
}

var ErrKYCNotApproved = apperror.Conflict("kyc_not_approved",
	"this merchant's business verification (KYC) is not approved yet; approve it in /admin/kyc")

// OnKYC records the verification status; approval activates a pending merchant.
// Returns true when the merchant changed.
func (m *Merchant) OnKYC(status string, now time.Time) bool {
	changed := m.KYCStatus != status
	m.KYCStatus = status
	if status == kyc.StatusApproved && m.Status == merchants.StatusPending {
		m.Status, m.ApprovedAt, changed = merchants.StatusActive, &now, true
	}
	if changed {
		m.UpdatedAt = now
	}
	return changed
}

func (m *Merchant) SetFee(feeBPS int64, now time.Time) error {
	if feeBPS < 0 || feeBPS > 10_000 {
		return apperror.Invalid("invalid_fee", "fee_bps must be between 0 and 10000")
	}
	m.FeeBPS, m.UpdatedAt = feeBPS, now
	return nil
}
