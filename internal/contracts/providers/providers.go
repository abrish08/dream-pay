// Package providers: messages owned by the provider module, which talks to
// payment service providers (Chapa, telebirr, mock, ...).
package providers

import (
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	ErrUnknownProvider     = apperror.Invalid("unknown_provider", "unknown payment provider")
	ErrTransactionNotFound = apperror.NotFound("provider_transaction_not_found", "transaction not found at provider")
	ErrProviderUnavailable = apperror.Unavailable("provider_unavailable", "payment provider is unavailable")
)

type CheckoutResult struct {
	CheckoutURL string `json:"checkout_url"`
	ProviderRef string `json:"provider_ref"`
}

type Transaction struct {
	Provider    string `json:"provider"`
	TxRef       string `json:"tx_ref"`
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
}

type RefundResult struct {
	Status            string `json:"status"`
	ProviderRefundRef string `json:"provider_refund_ref"`
	FailureReason     string `json:"failure_reason,omitempty"`
}

// ---- Requests ----

// InitiateCheckout starts a checkout. Idempotent per TxRef.
type InitiateCheckout struct {
	bus.Returns[CheckoutResult]
	Provider      string `json:"provider"`
	TxRef         string `json:"tx_ref"` // the payment id
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	CustomerEmail string `json:"customer_email"`
	CustomerName  string `json:"customer_name"`
	ReturnURL     string `json:"return_url"`
}

func (InitiateCheckout) Route() string { return "providers.initiate_checkout" }

// VerifyTransaction asks the provider directly (never trust callbacks alone).
type VerifyTransaction struct {
	bus.Returns[Transaction]
	Provider string `json:"provider"`
	TxRef    string `json:"tx_ref"`
}

func (VerifyTransaction) Route() string { return "providers.verify_transaction" }

// Refund refunds (part of) a transaction. Idempotent per RefundRef.
type Refund struct {
	bus.Returns[RefundResult]
	Provider  string `json:"provider"`
	TxRef     string `json:"tx_ref"`
	RefundRef string `json:"refund_ref"` // the refund id
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
	Reason    string `json:"reason"`
}

func (Refund) Route() string { return "providers.refund" }

// ---- Events ----

const TopicTransactionUpdated = "provider.transaction.updated.v1"

// TransactionUpdated: the provider reported a final status for a checkout.
type TransactionUpdated struct {
	Provider    string    `json:"provider"`
	TxRef       string    `json:"tx_ref"`
	ProviderRef string    `json:"provider_ref"`
	Status      string    `json:"status"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func (TransactionUpdated) EventTopic() string { return TopicTransactionUpdated }
