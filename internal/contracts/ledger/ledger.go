// Package ledger: messages owned by the ledger service.
package ledger

import (
	"paygate/internal/contracts/auth"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

// Permissions checked by the ledger service (registered with auth at start-up).
const (
	PermBalanceRead = "balance:read" // a merchant's own balance
	PermLedgerRead  = "ledger:read"  // platform-wide books (trial balance, summary)
	// Manual corrections need two people: one requests, another approves.
	PermAdjustRequest = "ledger:adjust"
	PermAdjustApprove = "ledger:approve"
)

var Permissions = []auth.PermissionDef{
	{Code: PermBalanceRead, Description: "View balance", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleAnalyst, auth.RoleViewer}},
	{Code: PermLedgerRead, Description: "View the platform ledger", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
	{Code: PermAdjustRequest, Description: "Request a manual ledger correction", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
	{Code: PermAdjustApprove, Description: "Approve or reject manual ledger corrections (not your own)", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
}

var ErrInsufficientFunds = apperror.Unprocessable("insufficient_funds", "the available balance is lower than the payout amount")

// ReserveFunds holds part of a merchant's available balance for a payout.
// Atomic (no two payouts can spend the same money) and idempotent per PayoutID.
type ReserveFunds struct {
	bus.Returns[Reservation]
	PayoutID    string `json:"payout_id"`
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
	Amount      int64  `json:"amount"`
}

type Reservation struct {
	AvailableAfter int64 `json:"available_after"`
}

func (ReserveFunds) Route() string { return "ledger.reserve_funds" }

// GetPostings returns the postings with these references (e.g.
// "payment:<id>:captured"); missing references are left out. At most 500.
// Used by reconciliation to prove every captured payment was booked.
type GetPostings struct {
	bus.Returns[[]PostingInfo]
	References []string `json:"references"`
}

type PostingInfo struct {
	Reference string `json:"reference"`
	Currency  string `json:"currency"`
	Amount    int64  `json:"amount"` // total of the posting's debit lines
}

func (GetPostings) Route() string { return "ledger.get_postings" }

// PaymentReference is the ledger reference of a captured payment.
func PaymentReference(paymentID string) string { return "payment:" + paymentID + ":captured" }

// GetBalance returns a merchant's balances (scheduled payouts).
type GetBalance struct {
	bus.Returns[Balance]
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
}

type Balance struct {
	Available      int64 `json:"available"`       // can be paid out
	Reserved       int64 `json:"reserved"`        // rolling reserve, released on schedule
	DisputeHold    int64 `json:"dispute_hold"`    // held for open disputes
	PendingPayouts int64 `json:"pending_payouts"` // held for payouts not sent yet
	PendingRefunds int64 `json:"pending_refunds"` // held for refunds being processed
}

func (GetBalance) Route() string { return "ledger.get_balance" }

// HoldRefund holds the refund amount before the refund is sent to the
// provider (available -> refunds pending), so a merchant cannot refund money
// it no longer has. Atomic and idempotent per RefundID. The balance may go
// below zero only by Allowance (the non-refundable fee of that payment) plus
// the merchant's rolling reserve, which exists to cover refunds.
type HoldRefund struct {
	bus.Returns[Reservation]
	RefundID    string `json:"refund_id"`
	PaymentID   string `json:"payment_id"`
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
	Amount      int64  `json:"amount"`
	Allowance   int64  `json:"allowance"`
}

func (HoldRefund) Route() string { return "ledger.hold_refund" }

var ErrInsufficientForRefund = apperror.Unprocessable("insufficient_balance",
	"the balance is too low for this refund; wait for more payments or contact support")

// ReleaseRefund gives a failed refund's hold back at once (the
// refunds.RefundFailed event does the same later; it is booked only once).
type ReleaseRefund struct {
	bus.Returns[Reservation]
	RefundID    string `json:"refund_id"`
	PaymentID   string `json:"payment_id"`
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
	Amount      int64  `json:"amount"`
	Reason      string `json:"reason"`
}

func (ReleaseRefund) Route() string { return "ledger.release_refund" }
