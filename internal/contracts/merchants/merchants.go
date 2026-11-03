// Package merchants: messages owned by the merchant module.
package merchants

import (
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

const (
	StatusPending   = "pending"   // signed up, waiting for approval: test mode only
	StatusActive    = "active"    // approved: live payments allowed
	StatusSuspended = "suspended" // blocked
)

var ErrNotFound = apperror.NotFound("merchant_not_found", "merchant not found")

type Merchant struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Email      string     `json:"email"`
	Phone      string     `json:"phone,omitempty"`
	Website    string     `json:"website,omitempty"`
	Status     string     `json:"status"`
	FeeBPS     int64      `json:"fee_bps"` // platform fee in basis points: 150 = 1.5%
	KYCStatus  string     `json:"kyc_status"`
	Settings   Settings   `json:"settings"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ---- Requests ----

// GetMerchant returns a merchant by id.
type GetMerchant struct {
	bus.Returns[Merchant]
	ID string `json:"id"`
}

func (GetMerchant) Route() string { return "merchants.get" }

// CreateMerchant registers a self-signed-up merchant (status pending). Sent by
// the auth service during signup.
type CreateMerchant struct {
	bus.Returns[Merchant]
	Name    string `json:"name"`
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Website string `json:"website"`
}

func (CreateMerchant) Route() string { return "merchants.create" }

// ---- Events ----

const TopicMerchantUpdated = "merchant.updated.v1"

// MerchantUpdated is published when a merchant is created or its status
// changes. Other services keep the fields they need (e.g. auth keeps the
// status) instead of asking the merchant service on every request.
type MerchantUpdated struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	KYCStatus string    `json:"kyc_status"`
	FeeBPS    int64     `json:"fee_bps"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (MerchantUpdated) EventTopic() string { return TopicMerchantUpdated }

// KYCPassed: live mode (live API keys, live payments, live payouts) needs an
// approved business verification. "waived" exists only for merchants that
// were live before KYC was introduced.
func KYCPassed(kycStatus string) bool { return kycStatus == "approved" || kycStatus == "waived" }

var ErrKYCRequired = apperror.Forbidden("kyc_required",
	"live mode needs an approved business verification (KYC): submit it at /v1/kyc; test mode works without it")

// ---- Settings (set by platform staff) ----

// Settings: risk limits, reserve and payout schedule of a merchant.
type Settings struct {
	// RiskLevel: low | normal | high. High tightens the fraud rules.
	RiskLevel string `json:"risk_level,omitempty"`
	// Limits per currency (minor units); 0 = no limit.
	Limits map[string]Limit `json:"limits,omitempty"`
	// Reserve: this share of each captured payment is held for ReserveDays
	// before it becomes available (cover for refunds and chargebacks).
	ReserveBPS  int64          `json:"reserve_bps"`  // 1000 = 10%
	ReserveDays int            `json:"reserve_days"` // 0..180
	Payout      PayoutSettings `json:"payout"`
}

type Limit struct {
	MaxAmount   int64 `json:"max_amount,omitempty"`   // largest single payment
	DailyAmount int64 `json:"daily_amount,omitempty"` // total of the last 24 hours
	DailyCount  int   `json:"daily_count,omitempty"`  // payments in the last 24 hours
}

// PayoutSettings: automatic payouts of the available balance.
type PayoutSettings struct {
	Schedule   string       `json:"schedule"`             // manual (default) | daily | weekly
	Weekday    int          `json:"weekday,omitempty"`    // weekly: 1 = Monday ... 7 = Sunday
	Currencies []string     `json:"currencies,omitempty"` // default ETB
	MinAmount  int64        `json:"min_amount,omitempty"` // do not pay out less (minor units)
	Bank       *BankAccount `json:"bank,omitempty"`       // verified by staff (KYC bank_account_proof)
}

type BankAccount struct {
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
}

const (
	RiskLow    = "low"
	RiskNormal = "normal"
	RiskHigh   = "high"

	PayoutManual = "manual"
	PayoutDaily  = "daily"
	PayoutWeekly = "weekly"
)

// ListPayoutSchedules returns merchants with automatic payouts (active and
// KYC-approved only).
type ListPayoutSchedules struct {
	bus.Returns[[]PayoutSchedule]
}

type PayoutSchedule struct {
	MerchantID string         `json:"merchant_id"`
	Payout     PayoutSettings `json:"payout"`
}

func (ListPayoutSchedules) Route() string { return "merchants.list_payout_schedules" }
