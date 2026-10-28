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
	ID        string
	Name      string
	Email     string
	Status    string
	FeeBPS    int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func NewMerchant(name, email string, feeBPS int64, now time.Time) (*Merchant, error) {
	name, email = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(email))
	if name == "" {
		return nil, apperror.Invalid("invalid_name", "name is required")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, apperror.Invalid("invalid_email", "a valid email is required")
	}
	if feeBPS < 0 || feeBPS > 10_000 {
		return nil, apperror.Invalid("invalid_fee", "fee_bps must be between 0 and 10000")
	}
	return &Merchant{
		ID: uuid.NewString(), Name: name, Email: email,
		Status: merchants.StatusActive, FeeBPS: feeBPS,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (m *Merchant) SetStatus(status string, now time.Time) error {
	if status != merchants.StatusActive && status != merchants.StatusSuspended {
		return apperror.Invalid("invalid_status", "status must be active or suspended")
	}
	m.Status, m.UpdatedAt = status, now
	return nil
}
