// Package payouts: messages owned by the payout service (merchant settlement).
package payouts

import (
	"time"

	"paygate/internal/contracts/auth"
)

const (
	StatusReserving = "reserving" // asking the ledger to hold the funds
	StatusRequested = "requested" // funds held, waiting for the transfer
	StatusPaid      = "paid"      // money sent to the merchant's bank
	StatusFailed    = "failed"    // transfer failed: funds released back
	StatusRejected  = "rejected"  // not enough balance: nothing held
)

// Permissions checked by the payout service (registered with auth at start-up).
const (
	PermRead   = "payouts:read"
	PermWrite  = "payouts:write"  // merchant: request payouts
	PermManage = "payouts:manage" // platform: mark payouts paid / failed
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View payouts", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleAnalyst}},
	{Code: PermWrite, Description: "Request payouts to the bank account", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin}},
	{Code: PermManage, Description: "Process merchant payouts", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
}

// ---- Events ----

const (
	TopicPayoutPaid   = "payout.paid.v1"
	TopicPayoutFailed = "payout.failed.v1"
)

// PayoutPaid: the held funds left to the merchant's bank.
type PayoutPaid struct {
	PayoutID      string    `json:"payout_id"`
	MerchantID    string    `json:"merchant_id"`
	Environment   string    `json:"environment"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	BankReference string    `json:"bank_reference"`
	OccurredAt    time.Time `json:"occurred_at"`
}

func (PayoutPaid) EventTopic() string { return TopicPayoutPaid }

// PayoutFailed: the transfer did not happen; held funds go back to the balance.
type PayoutFailed struct {
	PayoutID    string    `json:"payout_id"`
	MerchantID  string    `json:"merchant_id"`
	Environment string    `json:"environment"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Reason      string    `json:"reason"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func (PayoutFailed) EventTopic() string { return TopicPayoutFailed }
