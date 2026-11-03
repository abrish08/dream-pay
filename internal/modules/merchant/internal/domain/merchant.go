package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/kyc"
	"paygate/internal/contracts/merchants"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
)

type Merchant struct {
	ID         string
	Name       string
	Email      string
	Phone      string
	Website    string
	Status     string
	FeeBPS     int64
	KYCStatus  string
	Settings   merchants.Settings `gorm:"serializer:json;type:jsonb"`
	ApprovedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// DefaultFeeBPS applies to self-signed-up merchants until staff change it.
const DefaultFeeBPS = 150

type NewMerchantInput struct {
	Name, Email, Phone, Website string
	FeeBPS                      int64
	Status                      string // active (created by staff) or pending (self-signup)
}

func NewMerchant(in NewMerchantInput, now time.Time) (*Merchant, error) {
	name, email := strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Email))
	feeBPS := in.FeeBPS
	if name == "" || len(name) > 200 {
		return nil, apperror.Invalid("invalid_name", "business name is required (max 200 characters)")
	}
	if len(in.Phone) > 30 || len(in.Website) > 200 {
		return nil, apperror.Invalid("invalid_contact", "phone or website too long")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, apperror.Invalid("invalid_email", "a valid email is required")
	}
	if feeBPS < 0 || feeBPS > 10_000 {
		return nil, apperror.Invalid("invalid_fee", "fee_bps must be between 0 and 10000")
	}
	m := &Merchant{
		ID: uuid.NewString(), Name: name, Email: email, Phone: strings.TrimSpace(in.Phone),
		Website: strings.TrimSpace(in.Website), Status: in.Status, FeeBPS: feeBPS,
		CreatedAt: now, UpdatedAt: now,
	}
	switch in.Status {
	case merchants.StatusActive:
		m.ApprovedAt, m.KYCStatus = &now, kyc.StatusNotStarted // usable at once in test mode; live mode needs KYC
	case merchants.StatusPending:
		m.KYCStatus = kyc.StatusNotStarted
	default:
		return nil, apperror.Invalid("invalid_status", "a new merchant is active or pending")
	}
	return m, nil
}

// SetStatus: approve (pending -> active), suspend, reactivate, or reject a
// pending merchant (-> suspended). Nothing goes back to pending.
func (m *Merchant) SetStatus(status string, now time.Time) error {
	if status != merchants.StatusActive && status != merchants.StatusSuspended {
		return apperror.Invalid("invalid_status", "status must be active or suspended")
	}
	// A merchant never approved before (self-signed-up) needs approved KYC to
	// become active, whatever its current status: suspend-then-activate must
	// not skip verification.
	if status == merchants.StatusActive && m.ApprovedAt == nil &&
		m.KYCStatus != kyc.StatusApproved && m.KYCStatus != kyc.StatusWaived {
		return ErrKYCNotApproved
	}
	if status == merchants.StatusActive && m.ApprovedAt == nil {
		m.ApprovedAt = &now
	}
	m.Status, m.UpdatedAt = status, now
	return nil
}

var ErrKYCNotApproved = apperror.Conflict("kyc_not_approved",
	"this merchant's business verification (KYC) is not approved yet; approve it in /admin/kyc")

// OnKYC records the verification status; approval activates a pending merchant.
// Returns true when the merchant changed.
func (m *Merchant) OnKYC(status string, now time.Time) bool {
	changed := m.KYCStatus != status
	m.KYCStatus = status
	if status == kyc.StatusApproved && m.Status == merchants.StatusPending {
		m.Status, m.ApprovedAt, changed = merchants.StatusActive, &now, true
	}
	if changed {
		m.UpdatedAt = now
	}
	return changed
}

func (m *Merchant) SetFee(feeBPS int64, now time.Time) error {
	if feeBPS < 0 || feeBPS > 10_000 {
		return apperror.Invalid("invalid_fee", "fee_bps must be between 0 and 10000")
	}
	m.FeeBPS, m.UpdatedAt = feeBPS, now
	return nil
}

var bankAccountRe = regexp.MustCompile(`^[0-9]{6,20}$`)

// SetSettings validates and stores risk limits, reserve and payout schedule.
func (m *Merchant) SetSettings(st merchants.Settings, now time.Time) error {
	switch st.RiskLevel {
	case "":
		st.RiskLevel = merchants.RiskNormal
	case merchants.RiskLow, merchants.RiskNormal, merchants.RiskHigh:
	default:
		return apperror.Invalid("invalid_risk_level", "risk_level must be low, normal or high")
	}
	for cur, l := range st.Limits {
		if !money.Supported(cur) {
			return apperror.Invalid("invalid_limits", "limits are per currency: ETB or USD")
		}
		if l.MaxAmount < 0 || l.DailyAmount < 0 || l.DailyCount < 0 {
			return apperror.Invalid("invalid_limits", "limits cannot be negative")
		}
		if l.MaxAmount > 0 && l.DailyAmount > 0 && l.MaxAmount > l.DailyAmount {
			return apperror.Invalid("invalid_limits", "max_amount cannot exceed daily_amount")
		}
	}
	if st.ReserveBPS < 0 || st.ReserveBPS > 5_000 {
		return apperror.Invalid("invalid_reserve", "reserve_bps must be between 0 and 5000 (50%)")
	}
	if st.ReserveDays < 0 || st.ReserveDays > 180 || (st.ReserveBPS > 0 && st.ReserveDays == 0) {
		return apperror.Invalid("invalid_reserve", "reserve_days must be 1-180 when a reserve is set")
	}
	p := &st.Payout
	switch p.Schedule {
	case "":
		p.Schedule = merchants.PayoutManual
	case merchants.PayoutManual:
	case merchants.PayoutDaily, merchants.PayoutWeekly:
		if p.Schedule == merchants.PayoutWeekly && (p.Weekday < 1 || p.Weekday > 7) {
			return apperror.Invalid("invalid_payout", "weekday must be 1 (Monday) to 7 (Sunday)")
		}
		if p.Bank == nil || strings.TrimSpace(p.Bank.BankName) == "" || strings.TrimSpace(p.Bank.AccountName) == "" ||
			!bankAccountRe.MatchString(strings.ReplaceAll(p.Bank.AccountNumber, " ", "")) {
			return apperror.Invalid("invalid_payout", "automatic payouts need a verified bank account (bank_name, account_number 6-20 digits, account_name)")
		}
	default:
		return apperror.Invalid("invalid_payout", "schedule must be manual, daily or weekly")
	}
	if len(p.Currencies) == 0 {
		p.Currencies = []string{"ETB"}
	}
	for i, c := range p.Currencies {
		p.Currencies[i] = strings.ToUpper(c)
		if !money.Supported(p.Currencies[i]) {
			return apperror.Invalid("invalid_payout", "payout currencies: ETB or USD")
		}
	}
	if p.MinAmount < 0 {
		return apperror.Invalid("invalid_payout", "min_amount cannot be negative")
	}
	if p.Bank != nil {
		p.Bank.AccountNumber = strings.ReplaceAll(strings.TrimSpace(p.Bank.AccountNumber), " ", "")
	}
	m.Settings, m.UpdatedAt = st, now
	return nil
}
