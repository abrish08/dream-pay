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
	// ErrProviderUnavailable: the request certainly did NOT reach the provider
	// (connection refused, circuit open, 5xx before acceptance). Safe to fail
	// over to another provider or to fail the payment.
	ErrProviderUnavailable = apperror.Unavailable("provider_unavailable", "payment provider is unavailable")
	// ErrOutcomeUnknown: the request may have reached the provider (timeout,
	// connection lost after sending). Never retry elsewhere: ask the provider.
	ErrOutcomeUnknown    = apperror.Unavailable("provider_outcome_unknown", "the provider did not confirm the request in time")
	ErrMethodUnavailable = apperror.Unprocessable("payment_method_unavailable",
		"this payment method is not available right now")
)

type CheckoutResult struct {
	CheckoutURL string `json:"checkout_url"`
	ProviderRef string `json:"provider_ref"`
	Provider    string `json:"provider"` // the provider the method was routed to
	// OutcomeUnknown: the request may have reached the provider (e.g. a
	// timeout after sending a USSD push), so it was NOT retried or sent
	// elsewhere. The payment stays open and its status is checked with the
	// provider until it is known. Never treat this as a failure.
	OutcomeUnknown bool `json:"outcome_unknown,omitempty"`
}

type Transaction struct {
	Provider    string `json:"provider"`
	TxRef       string `json:"tx_ref"`
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	// FailureReason when failed, e.g. insufficient_funds, expired_card,
	// declined, authentication_failed (3-D Secure).
	FailureReason string `json:"failure_reason,omitempty"`
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
	CustomerPhone string `json:"customer_phone,omitempty"` // +2519...: wallets push the PIN prompt to it
	ReturnURL     string `json:"return_url"`
}

func (InitiateCheckout) Route() string { return "providers.initiate_checkout" }

// Instructions: what the customer must do themselves (bank transfer:
// accounts and the reference to write on the transfer).
type GetInstructions struct {
	bus.Returns[Instructions]
	TxRef string `json:"tx_ref"`
}

type Instructions struct {
	Kind      string        `json:"kind"`      // bank_transfer, or "" when the provider needs nothing
	Reference string        `json:"reference"` // write this on the transfer
	Accounts  []BankAccount `json:"accounts,omitempty"`
}

type BankAccount struct {
	Bank          string `json:"bank"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
}

func (GetInstructions) Route() string { return "providers.get_instructions" }

// Wallet methods: the customer approves a push on their phone.
var WalletMethods = []string{MethodTelebirr, MethodCBEBirr, MethodMPesa}

func IsWallet(method string) bool {
	for _, m := range WalletMethods {
		if m == method {
			return true
		}
	}
	return false
}

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
// Provider may be empty: the provider module then uses the provider it
// recorded for TxRef.
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
	Provider    string `json:"provider"`
	TxRef       string `json:"tx_ref"`
	ProviderRef string `json:"provider_ref"`
	Status      string `json:"status"`
	Amount      int64  `json:"amount"`
	Currency    string `json:"currency"`
	// FailureReason: why the provider declined (see Transaction).
	FailureReason string    `json:"failure_reason,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`
}

func (TransactionUpdated) EventTopic() string { return TopicTransactionUpdated }

// Permissions checked by the provider service (registered with auth at start-up).
const (
	PermRead = "providers:read" // platform: provider health and routing
	// PermConfirmTransfers: finance confirms bank transfers seen on the bank statement.
	PermConfirmTransfers = "providers:confirm_transfers"
)

var Permissions = []auth.PermissionDef{
	{Code: PermConfirmTransfers, Description: "Confirm customers' bank transfers from the bank statement", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
	{Code: PermRead, Description: "View payment providers, routing and health", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleFinance, auth.RoleSupport}},
}
