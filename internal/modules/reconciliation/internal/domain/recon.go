package domain

import (
	"strconv"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
)

const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"

	KindManual = "manual"
	KindDaily  = "daily"

	IssueStatusMismatch    = "status_mismatch"
	IssueAmountMismatch    = "amount_mismatch"
	IssueMissingAtProvider = "missing_at_provider"
	IssueProviderError     = "provider_error"
)

type Run struct {
	ID          string
	Kind        string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Status      string
	Checked     int
	Mismatched  int
	Error       string
	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

type Item struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	PaymentID string    `json:"payment_id"`
	Provider  string    `json:"provider"`
	Issue     string    `json:"issue"`
	Expected  string    `json:"expected"`
	Actual    string    `json:"actual"`
	CreatedAt time.Time `json:"created_at"`
}

func NewRun(kind string, from, to, now time.Time) (*Run, error) {
	if !from.Before(to) {
		return nil, apperror.Invalid("invalid_period", "from must be before to")
	}
	if to.Sub(from) > 31*24*time.Hour {
		return nil, apperror.Invalid("invalid_period", "period cannot exceed 31 days")
	}
	return &Run{ID: uuid.NewString(), Kind: kind, PeriodStart: from, PeriodEnd: to, Status: RunQueued, CreatedAt: now}, nil
}

// Compare checks our view of a payment against the provider's.
func Compare(p payments.Payment, t providers.Transaction) (issue, expected, actual string) {
	weCaptured := p.Status == payments.StatusSucceeded || p.Status == payments.StatusPartiallyRefunded ||
		p.Status == payments.StatusRefunded
	switch {
	case weCaptured && t.Status != providers.StatusSucceeded:
		return IssueStatusMismatch, "succeeded", t.Status
	case p.Status == payments.StatusFailed && t.Status == providers.StatusSucceeded:
		// The customer was charged but we told the merchant it failed: urgent.
		return IssueStatusMismatch, "failed", t.Status
	case weCaptured && (t.Amount != p.Amount || t.Currency != p.Currency):
		return IssueAmountMismatch, formatAmt(p.Amount, p.Currency), formatAmt(t.Amount, t.Currency)
	}
	return "", "", ""
}

func formatAmt(a int64, c string) string { return c + " " + strconv.FormatInt(a, 10) }
