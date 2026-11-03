// Package domain holds the payment entity and its rules. No database, HTTP or
// other modules in here.
package domain

import (
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
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
	Pending:    {Processing, Failed, Expired},
	Processing: {Succeeded, Failed, Expired},
	Expired:    {Succeeded, Failed},
	// A card payment can be declined and then paid with another card in the
	// same checkout: the provider's verified capture wins, or the customer's
	// money would be taken for a "failed" payment.
	Failed:            {Succeeded},
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
	PaymentMethod  string
	Provider       string
	ProviderRef    string
	Reference      string
	Description    string
	Metadata       database.StringMap `gorm:"type:jsonb"`
	CustomerEmail  string
	CustomerName   string
	CustomerPhone  string
	CustomerIP     string
	ReturnURL      string
	CheckoutURL    string
	FailureReason  string
	Reserve        int64 // held from the merchant's share for ReserveDays (refund / chargeback cover)
	ReserveDays    int
	IdempotencyKey string
	RequestHash    string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	NextCheckAt    *time.Time // next status check with the provider (open payments only)
	CheckCount     int

	changes []Change `gorm:"-"` // recorded in payment_events when saved
}

// ---- History ----

// Sources of a change, recorded in the payment's history.
const (
	SourceMerchantAPI    = "merchant_api"   // the merchant's server (API key)
	SourceCustomer       = "customer"       // the customer on the hosted page
	SourceProvider       = "provider"       // the provider's notification (verified with the provider)
	SourceStatusCheck    = "status_check"   // our scheduled check with the provider
	SourceExpiry         = "expiry"         // the payment's time ran out
	SourceRefund         = "refund"         // a refund was paid back
	SourceReconciliation = "reconciliation" // the daily reconciliation found a difference
	SourceRisk           = "risk"           // declined by the fraud rules
	SourceDispute        = "dispute"        // a chargeback was opened or decided
	SourceSystem         = "system"
)

// Change is one entry of the payment's history.
type Change struct {
	From, To Status
	Note     string // for a note without status change (From == To)
	At       time.Time
}

// Actor says who caused the changes being saved and why.
type Actor struct {
	Source   string
	Reason   string
	Provider string
	Details  map[string]any // e.g. what the provider answered
}

// Changes returns the changes not yet saved.
func (p *Payment) Changes() []Change { return p.changes }

// ClearChanges is called once they are saved.
func (p *Payment) ClearChanges() { p.changes = nil }

// Note records an event without a status change (e.g. "method chosen").
func (p *Payment) Note(note string, now time.Time) {
	p.changes = append(p.changes, Change{From: p.Status, To: p.Status, Note: note, At: now})
}

type NewPaymentInput struct {
	MerchantID     string
	Environment    string
	IdempotencyKey string
	RequestHash    string
	Amount         int64
	Currency       string
	Method         string // optional: empty = the customer picks on the hosted page
	Provider       string // optional, legacy: explicit provider
	Reference      string
	Description    string
	Metadata       map[string]string
	CustomerEmail  string
	CustomerName   string
	CustomerPhone  string
	CustomerIP     string
	ReturnURL      string
	TTL            time.Duration // how long the customer has to pay
	ReserveBPS     int64         // the merchant's reserve rate now (snapshot)
	ReserveDays    int
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
	case in.Method != "" && !providers.ValidMethod(in.Method):
		return nil, apperror.Invalid("invalid_payment_method",
			"payment_method must be one of: "+strings.Join(providers.Methods, ", "))
	case strings.TrimSpace(in.Reference) == "" || len(in.Reference) > 100:
		return nil, apperror.Invalid("invalid_reference", "reference is required (max 100 characters)")
	}
	if in.CustomerEmail != "" {
		if _, err := mail.ParseAddress(in.CustomerEmail); err != nil {
			return nil, apperror.Invalid("invalid_customer_email", "customer email is not valid")
		}
	}
	if len(in.CustomerPhone) > 20 {
		return nil, apperror.Invalid("invalid_customer_phone", "customer phone is too long")
	}
	if in.CustomerIP != "" {
		if _, err := netip.ParseAddr(in.CustomerIP); err != nil {
			return nil, apperror.Invalid("invalid_customer_ip", "customer ip must be an IPv4 or IPv6 address")
		}
	}
	if len(in.Description) > 255 {
		return nil, apperror.Invalid("invalid_description", "description is at most 255 characters")
	}
	if err := validateMetadata(in.Metadata); err != nil {
		return nil, err
	}
	if in.Metadata == nil {
		in.Metadata = map[string]string{}
	}
	if in.ReturnURL != "" {
		if u, err := url.Parse(in.ReturnURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, apperror.Invalid("invalid_return_url", "return_url must be an absolute http(s) URL")
		}
	}
	if in.TTL < MinTTL || in.TTL > MaxTTL {
		return nil, apperror.Invalid("invalid_expiry", "expires_in_minutes must be between 5 and 1440")
	}
	fee := CalculateFee(in.Amount, feeBPS)
	reserve := int64(0)
	if in.ReserveBPS > 0 && in.ReserveDays > 0 {
		reserve = CalculateFee(in.Amount-fee, in.ReserveBPS) // of the merchant's share
	}
	return &Payment{
		Reserve: reserve, ReserveDays: in.ReserveDays,
		CustomerPhone: strings.TrimSpace(in.CustomerPhone), CustomerIP: strings.TrimSpace(in.CustomerIP),
		changes: []Change{{From: "", To: Pending, At: now}},
		ID:      uuid.NewString(), MerchantID: in.MerchantID, Environment: in.Environment,
		Amount: in.Amount, Fee: fee, Currency: in.Currency,
		Status: Pending, PaymentMethod: in.Method, Provider: in.Provider, Reference: strings.TrimSpace(in.Reference),
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
	p.changes = append(p.changes, Change{From: p.Status, To: next, At: now})
	p.Status, p.UpdatedAt = next, now
	if next != Processing {
		p.NextCheckAt = nil // only open payments are checked
	}
	return nil
}

// StartProcessing records where the checkout was routed and schedules the
// first status check with the provider.
func (p *Payment) StartProcessing(provider, providerRef, checkoutURL string, firstCheck time.Duration, now time.Time) error {
	if err := p.TransitionTo(Processing, now); err != nil {
		return err
	}
	p.Provider, p.ProviderRef, p.CheckoutURL = provider, providerRef, checkoutURL
	if firstCheck > 0 {
		at := now.Add(firstCheck)
		p.NextCheckAt, p.CheckCount = &at, 0
	}
	return nil
}

// ScheduleCheck sets the next status check after a check that found the
// payment still open; nil when the schedule is used up (expiry takes over).
func (p *Payment) ScheduleCheck(schedule []time.Duration, now time.Time) {
	p.CheckCount++
	if p.Status != Processing || p.CheckCount >= len(schedule) {
		p.NextCheckAt = nil
		return
	}
	at := now.Add(schedule[p.CheckCount])
	p.NextCheckAt = &at
}

// ChooseMethod: the customer picked a method on the hosted checkout page.
func (p *Payment) ChooseMethod(method string, now time.Time) error {
	switch {
	case p.Status != Pending:
		return apperror.Conflict("payment_not_open", "this payment is no longer waiting for a payment method")
	case !now.Before(p.ExpiresAt):
		return apperror.Conflict("payment_expired", "this payment has expired")
	case !providers.ValidMethod(method):
		return apperror.Invalid("invalid_payment_method", "unknown payment method")
	}
	p.PaymentMethod, p.UpdatedAt = method, now
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

// Settled: money was taken; nothing the provider reports can undo that.
func (p *Payment) Settled() bool {
	return p.Status == Succeeded || p.Status == PartiallyRefunded || p.Status == Refunded
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
