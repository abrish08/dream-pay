// Package providers: messages owned by the provider module, which talks to
// payment providers (telebirr, CBE Birr, M-Pesa, card switch, banks, mock).
//
// Merchants ask for a payment METHOD (what the customer pays with). The
// provider module ROUTES each method to a provider (who processes it),
// following the configured route table, with failover to the next provider
// when one is down. That is what lets the gateway start on one connection and
// move a method to a direct integration later without merchants noticing.
package providers

import (
	"time"

	"paygate/internal/contracts/auth"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Payment methods (what the customer pays with).
const (
	MethodTelebirr     = "telebirr"
	MethodCBEBirr      = "cbe_birr"
	MethodMPesa        = "mpesa"
	MethodCard         = "card"          // Visa / Mastercard / domestic cards through the card switch
	MethodBankTransfer = "bank_transfer" // account-to-account / internet banking
)

// Methods lists every method the API accepts, in display order.
var Methods = []string{MethodTelebirr, MethodCBEBirr, MethodMPesa, MethodCard, MethodBankTransfer}

// MethodLabels are customer-facing names (hosted checkout page).
var MethodLabels = map[string]string{
	MethodTelebirr: "telebirr", MethodCBEBirr: "CBE Birr", MethodMPesa: "M-Pesa",
	MethodCard: "Card (Visa, Mastercard)", MethodBankTransfer: "Bank transfer",
}

func ValidMethod(m string) bool {
	for _, x := range Methods {
		if x == m {
			return true
		}
	}
	return false
}

var (
	ErrUnknownProvider     = apperror.Invalid("unknown_provider", "unknown payment provider")
	ErrTransactionNotFound = apperror.NotFound("provider_transaction_not_found", "transaction not found at provider")
	ErrProviderUnavailable = apperror.Unavailable("provider_unavailable", "payment provider is unavailable")
	ErrMethodUnavailable   = apperror.Unprocessable("payment_method_unavailable",
		"this payment method is not available right now")
)

type CheckoutResult struct {
	CheckoutURL string `json:"checkout_url"`
	ProviderRef string `json:"provider_ref"`
	Provider    string `json:"provider"` // the provider the method was routed to
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

// InitiateCheckout starts a checkout. Idempotent per TxRef. Send Method
// (routed to a provider) or, rarely, an explicit Provider.
type InitiateCheckout struct {
	bus.Returns[CheckoutResult]
	Method        string `json:"method"`
	Provider      string `json:"provider,omitempty"`
	Environment   string `json:"environment"` // test always routes to the sandbox
	TxRef         string `json:"tx_ref"`      // the payment id
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	CustomerEmail string `json:"customer_email"`
	CustomerName  string `json:"customer_name"`
	ReturnURL     string `json:"return_url"`
}

func (InitiateCheckout) Route() string { return "providers.initiate_checkout" }

// ListMethods returns the payment methods that can be used right now.
type ListMethods struct {
	bus.Returns[[]MethodInfo]
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
}

type MethodInfo struct {
	Method    string   `json:"method"`
	Label     string   `json:"label"`
	Available bool     `json:"available"`
	Providers []string `json:"providers,omitempty"` // in routing order (staff views only)
}

func (ListMethods) Route() string { return "providers.list_methods" }

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

// Permissions checked by the provider service (registered with auth at start-up).
const PermRead = "providers:read" // platform: provider health and routing

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View payment providers, routing and health", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleFinance, auth.RoleSupport}},
}
