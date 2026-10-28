// Package domain holds the payment entity and its rules. No database, HTTP or
// other modules in here.
package domain

import (
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/payments"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/database"
	"paygate/internal/platform/money"
)

type Status string

const (
	Pending           Status = payments.StatusPending
	Processing        Status = payments.StatusProcessing
	Succeeded         Status = payments.StatusSucceeded
	Failed            Status = payments.StatusFailed
	PartiallyRefunded Status = payments.StatusPartiallyRefunded
	Refunded          Status = payments.StatusRefunded
	Expired           Status = payments.StatusExpired
)

// The only allowed status changes. Anything else is a bug or a replayed event.
// Expired -> Succeeded: the customer paid after expiry and the provider
// confirmed it; the money was taken, so the payment must be honoured.
var transitions = map[Status][]Status{
	Pending:           {Processing, Failed, Expired},
	Processing:        {Succeeded, Failed, Expired},
	Expired:           {Succeeded, Failed},
	Succeeded:         {PartiallyRefunded, Refunded},
	PartiallyRefunded: {PartiallyRefunded, Refunded},
}

var ErrInvalidTransition = apperror.Conflict("invalid_status_transition", "payment status change not allowed")

type Payment struct {
	ID             string
	MerchantID     string
	Environment    string
	Amount         int64
	Fee            int64
	RefundedAmount int64
	Currency       string
	Status         Status
	Provider       string
	ProviderRef    string
	Reference      string
	Description    string
	Metadata       database.StringMap `gorm:"type:jsonb"`
	CustomerEmail  string
	CustomerName   string
	ReturnURL      string
	CheckoutURL    string
	FailureReason  string
	IdempotencyKey string
	RequestHash    string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type NewPaymentInput struct {
	MerchantID     string
	Environment    string
	IdempotencyKey string
	RequestHash    string
	Amount         int64
	Currency       string
	Provider       string
	Reference      string
	Description    string
	Metadata       map[string]string
	CustomerEmail  string
	CustomerName   string
	ReturnURL      string
	TTL            time.Duration // how long the customer has to pay
}

const (
	MaxMetadataKeys     = 20
	MaxMetadataKeyLen   = 40
	MaxMetadataValueLen = 500
	MinTTL, MaxTTL      = 5 * time.Minute, 24 * time.Hour
)

func NewPayment(in NewPaymentInput, feeBPS int64, now time.Time) (*Payment, error) {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	switch {
	case in.Amount <= 0 || in.Amount > money.MaxAmount:
		return nil, apperror.Invalid("invalid_amount", "amount must be a positive integer in minor units")
	case !money.Supported(in.Currency):
		return nil, apperror.Invalid("unsupported_currency", "currency must be ETB or USD")
	case strings.TrimSpace(in.Provider) == "":
		return nil, apperror.Invalid("invalid_provider", "provider is required")
	case strings.TrimSpace(in.Reference) == "" || len(in.Reference) > 100:
		return nil, apperror.Invalid("invalid_reference", "reference is required (max 100 characters)")
	}
	if in.CustomerEmail != "" {
		if _, err := mail.ParseAddress(in.CustomerEmail); err != nil {
			return nil, apperror.Invalid("invalid_customer_email", "customer email is not valid")
		}
	}
	if len(in.Description) > 255 {
		return nil, apperror.Invalid("invalid_description", "description is at most 255 characters")
	}
	if err := validateMetadata(in.Metadata); err != nil {
		return nil, err
	}
	if in.ReturnURL != "" {
		if u, err := url.Parse(in.ReturnURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, apperror.Invalid("invalid_return_url", "return_url must be an absolute http(s) URL")
		}
	}
	if in.TTL < MinTTL || in.TTL > MaxTTL {
		return nil, apperror.Invalid("invalid_expiry", "expires_in_minutes must be between 5 and 1440")
	}
	return &Payment{
		ID: uuid.NewString(), MerchantID: in.MerchantID, Environment: in.Environment,
		Amount: in.Amount, Fee: CalculateFee(in.Amount, feeBPS), Currency: in.Currency,
		Status: Pending, Provider: in.Provider, Reference: strings.TrimSpace(in.Reference),
		Description: strings.TrimSpace(in.Description), Metadata: in.Metadata,
		CustomerEmail: in.CustomerEmail, CustomerName: in.CustomerName, ReturnURL: in.ReturnURL,
		IdempotencyKey: in.IdempotencyKey, RequestHash: in.RequestHash,
		ExpiresAt: now.Add(in.TTL), CreatedAt: now, UpdatedAt: now,
	}, nil
}

// CalculateFee rounds half up. The fee is fixed at creation time (a snapshot
// of the merchant's rate) so later rate changes never alter past payments.
func CalculateFee(amount, bps int64) int64 { return (amount*bps + 5_000) / 10_000 }

func (p *Payment) TransitionTo(next Status, now time.Time) error {
	if !slices.Contains(transitions[p.Status], next) {
		return ErrInvalidTransition.WithMessage(fmt.Sprintf("cannot move payment from %s to %s", p.Status, next))
	}
	p.Status, p.UpdatedAt = next, now
	return nil
}

func (p *Payment) StartProcessing(providerRef, checkoutURL string, now time.Time) error {
	if err := p.TransitionTo(Processing, now); err != nil {
		return err
	}
	p.ProviderRef, p.CheckoutURL = providerRef, checkoutURL
	return nil
}

func (p *Payment) Fail(reason string, now time.Time) error {
	if err := p.TransitionTo(Failed, now); err != nil {
		return err
	}
	p.FailureReason = reason
	return nil
}

// Captured reports whether the provider has already given a final answer.
// An expired payment can still be captured if the customer paid late.
func (p *Payment) Captured() bool {
	return p.Status != Pending && p.Status != Processing && p.Status != Expired
}

// Expire closes a payment the customer did not complete in time.
func (p *Payment) Expire(now time.Time) error {
	if err := p.TransitionTo(Expired, now); err != nil {
		return err
	}
	p.FailureReason = "expired"
	return nil
}

func validateMetadata(m map[string]string) error {
	if len(m) > MaxMetadataKeys {
		return apperror.Invalid("invalid_metadata", fmt.Sprintf("metadata has at most %d keys", MaxMetadataKeys))
	}
	for k, v := range m {
		if k == "" || len(k) > MaxMetadataKeyLen || len(v) > MaxMetadataValueLen {
			return apperror.Invalid("invalid_metadata",
				fmt.Sprintf("metadata keys are 1-%d characters, values at most %d", MaxMetadataKeyLen, MaxMetadataValueLen))
		}
	}
	return nil
}

func (p *Payment) ApplyRefund(amount int64, now time.Time) error {
	if amount <= 0 || p.RefundedAmount+amount > p.Amount {
		return apperror.Conflict("refund_exceeds_payment", "refund exceeds the payment amount")
	}
	next := PartiallyRefunded
	if p.RefundedAmount+amount == p.Amount {
		next = Refunded
	}
	if err := p.TransitionTo(next, now); err != nil {
		return err
	}
	p.RefundedAmount += amount
	return nil
}
