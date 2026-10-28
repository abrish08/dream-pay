// Package payments: messages owned by the payment module.
package payments

import (
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

const (
	StatusPending           = "pending"
	StatusProcessing        = "processing"
	StatusSucceeded         = "succeeded"
	StatusFailed            = "failed"
	StatusPartiallyRefunded = "partially_refunded"
	StatusRefunded          = "refunded"
	StatusExpired           = "expired" // no final answer from the provider before expires_at
)

var (
	ErrNotFound            = apperror.NotFound("payment_not_found", "payment not found")
	ErrIdempotencyConflict = apperror.Conflict("idempotency_key_reused",
		"this Idempotency-Key was already used with a different request body")
)

type Payment struct {
	ID             string            `json:"id"`
	MerchantID     string            `json:"merchant_id"`
	Environment    string            `json:"environment"`
	Amount         int64             `json:"amount"`
	Fee            int64             `json:"fee"`
	RefundedAmount int64             `json:"refunded_amount"`
	Currency       string            `json:"currency"`
	Status         string            `json:"status"`
	PaymentMethod  string            `json:"payment_method,omitempty"`
	Provider       string            `json:"provider,omitempty"`
	ProviderRef    string            `json:"provider_ref,omitempty"`
	Reference      string            `json:"reference"`
	Description    string            `json:"description,omitempty"`
	Metadata       map[string]string `json:"metadata"`
	CustomerEmail  string            `json:"customer_email,omitempty"`
	CustomerName   string            `json:"customer_name,omitempty"`
	ReturnURL      string            `json:"return_url,omitempty"`
	CheckoutURL    string            `json:"checkout_url,omitempty"`
	FailureReason  string            `json:"failure_reason,omitempty"`
	ExpiresAt      time.Time         `json:"expires_at"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// ---- Requests ----

// GetPayment returns a payment owned by MerchantID.
type GetPayment struct {
	bus.Returns[Payment]
	MerchantID string `json:"merchant_id"`
	PaymentID  string `json:"payment_id"`
}

func (GetPayment) Route() string { return "payments.get" }

// ListPayments is for back-office modules (e.g. reconciliation), across merchants.
type ListPayments struct {
	bus.Returns[[]Payment]
	Statuses    []string  `json:"statuses"`
	CreatedFrom time.Time `json:"created_from"`
	CreatedTo   time.Time `json:"created_to"`
	AfterID     string    `json:"after_id"` // cursor: results are ordered by id
	Limit       int       `json:"limit"`
}

func (ListPayments) Route() string { return "payments.list" }

// ---- Events ----

const (
	TopicPaymentSucceeded = "payment.succeeded.v1"
	TopicPaymentFailed    = "payment.failed.v1"
	TopicPaymentExpired   = "payment.expired.v1"
)

type PaymentSucceeded struct {
	PaymentID   string            `json:"payment_id"`
	MerchantID  string            `json:"merchant_id"`
	Environment string            `json:"environment"`
	Amount      int64             `json:"amount"`
	Fee         int64             `json:"fee"`
	Currency    string            `json:"currency"`
	Provider    string            `json:"provider"`
	ProviderRef string            `json:"provider_ref"`
	Reference   string            `json:"reference"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at"`
}

func (PaymentSucceeded) EventTopic() string { return TopicPaymentSucceeded }

type PaymentFailed struct {
	PaymentID   string            `json:"payment_id"`
	MerchantID  string            `json:"merchant_id"`
	Environment string            `json:"environment"`
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Provider    string            `json:"provider"`
	Reference   string            `json:"reference"`
	Reason      string            `json:"reason"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at"`
}

func (PaymentFailed) EventTopic() string { return TopicPaymentFailed }

// PaymentExpired: the customer did not complete payment before expires_at.
// If the provider later confirms the payment anyway, PaymentSucceeded follows.
type PaymentExpired struct {
	PaymentID   string            `json:"payment_id"`
	MerchantID  string            `json:"merchant_id"`
	Environment string            `json:"environment"`
	Amount      int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Reference   string            `json:"reference"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at"`
}

func (PaymentExpired) EventTopic() string { return TopicPaymentExpired }
