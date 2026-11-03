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
	PermLedgerRead  = "ledger:read"  // platform-wide books (trial balance)
)

var Permissions = []auth.PermissionDef{
	{Code: PermBalanceRead, Description: "View balance", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleAnalyst, auth.RoleViewer}},
	{Code: PermLedgerRead, Description: "View the platform ledger", Audience: auth.AudiencePlatform,
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
}

func (GetBalance) Route() string { return "ledger.get_balance" }
