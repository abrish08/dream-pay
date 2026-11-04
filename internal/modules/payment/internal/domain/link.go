package domain

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
)

// PaymentLink: a URL a merchant shares (chat, SMS, invoice, QR on a
// counter); each visit opens a payment on the hosted checkout page.
type PaymentLink struct {
	ID          string
	MerchantID  string
	Environment string
	Amount      int64
	Currency    string
	Description string
	Reference   string
	SingleUse   bool // closed once paid (an invoice); otherwise reusable (a price tag)
	Active      bool
	ExpiresAt   *time.Time
	PaidCount   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (PaymentLink) TableName() string { return "payment_links" }

type NewLinkInput struct {
	MerchantID, Environment, Currency, Description, Reference string
	Amount                                                    int64
	SingleUse                                                 bool
	ExpiresAt                                                 *time.Time
}

func NewLink(in NewLinkInput, now time.Time) (*PaymentLink, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	switch {
	case in.Environment != "live" && in.Environment != "test":
		return nil, apperror.Invalid("invalid_environment", "environment must be live or test")
	case in.Amount <= 0 || in.Amount > money.MaxAmount:
		return nil, apperror.Invalid("invalid_amount", "amount must be a positive integer in minor units")
	case !money.Supported(in.Currency):
		return nil, apperror.Invalid("unsupported_currency", "currency must be ETB or USD")
	case len(in.Description) > 255 || len(in.Reference) > 80:
		return nil, apperror.Invalid("invalid_link", "description is at most 255 characters, reference 80")
	case in.ExpiresAt != nil && !in.ExpiresAt.After(now):
		return nil, apperror.Invalid("invalid_expiry", "expires_at must be in the future")
	}
	b := make([]byte, 16) // 128 bits: the link cannot be guessed
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := "pl_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return &PaymentLink{ID: id, MerchantID: in.MerchantID, Environment: in.Environment, Amount: in.Amount,
		Currency: in.Currency, Description: strings.TrimSpace(in.Description), Reference: strings.TrimSpace(in.Reference),
		SingleUse: in.SingleUse, Active: true, ExpiresAt: in.ExpiresAt, CreatedAt: now, UpdatedAt: now}, nil
}

var ErrLinkClosed = apperror.Conflict("payment_link_closed", "this payment link is no longer active")

// Usable reports whether a customer may pay through the link now.
func (l *PaymentLink) Usable(now time.Time) error {
	if !l.Active || (l.SingleUse && l.PaidCount > 0) || (l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)) {
		return ErrLinkClosed
	}
	return nil
}
