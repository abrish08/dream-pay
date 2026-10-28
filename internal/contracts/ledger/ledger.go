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
