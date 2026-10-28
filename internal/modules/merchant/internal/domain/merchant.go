package domain

import (
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

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
		m.ApprovedAt = &now
	case merchants.StatusPending:
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
	if status == merchants.StatusActive && m.ApprovedAt == nil {
		m.ApprovedAt = &now
	}
	m.Status, m.UpdatedAt = status, now
	return nil
}

func (m *Merchant) SetFee(feeBPS int64, now time.Time) error {
	if feeBPS < 0 || feeBPS > 10_000 {
		return apperror.Invalid("invalid_fee", "fee_bps must be between 0 and 10000")
	}
	m.FeeBPS, m.UpdatedAt = feeBPS, now
	return nil
}
