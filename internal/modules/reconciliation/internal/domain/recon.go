// Package domain: reconciliation rules. Three-way: our payments, the
// provider (live API and settlement files) and our ledger must agree.
package domain

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
)

const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"

	KindManual     = "manual"
	KindDaily      = "daily"
	KindSettlement = "settlement"

	// Provider API vs our payments.
	IssueStatusMismatch    = "status_mismatch"     // e.g. customer charged, we say failed/expired
	IssueAmountMismatch    = "amount_mismatch"     // provider charged a different amount
	IssueMissingAtProvider = "missing_at_provider" // we say captured, provider has no record
	IssueProviderError     = "provider_error"      // provider could not be asked (retried next run)
	IssueStuckProcessing   = "stuck_processing"    // still open long after it should have ended
	// Ledger vs our payments.
	IssueLedgerMissing        = "ledger_missing"         // captured but never booked: merchant not credited
	IssueLedgerAmountMismatch = "ledger_amount_mismatch" // booked with a different amount
	// Settlement file vs our payments.
	IssueSettlementMissing        = "settlement_missing"         // captured, provider has not paid it yet
	IssueSettlementUnknown        = "settlement_unknown"         // provider settled something we did not capture
	IssueSettlementAmountMismatch = "settlement_amount_mismatch" // settled with a different amount
	IssueSettlementDuplicate      = "settlement_duplicate"       // the same payment settled twice
	IssueDepositMismatch          = "deposit_mismatch"           // bank deposit differs from the file's net

	SeverityCritical = "critical" // money is wrong somewhere: act today
	SeverityHigh     = "high"
	SeverityMedium   = "medium" // usually timing; re-checked automatically
	SeverityLow      = "low"

	ItemOpen         = "open"
	ItemAutoResolved = "auto_resolved"
	ItemResolved     = "resolved"
)

// Severity of each issue.
var Severity = map[string]string{
	IssueStatusMismatch: SeverityCritical, IssueAmountMismatch: SeverityCritical,
	IssueLedgerMissing: SeverityCritical, IssueLedgerAmountMismatch: SeverityCritical,
	IssueSettlementUnknown: SeverityCritical, IssueSettlementAmountMismatch: SeverityCritical,
	IssueDepositMismatch: SeverityCritical, IssueMissingAtProvider: SeverityHigh,
	IssueSettlementDuplicate: SeverityHigh, IssueStuckProcessing: SeverityHigh,
	IssueSettlementMissing: SeverityMedium, IssueProviderError: SeverityLow,
}

type Run struct {
	ID           string
	Kind         string
	Provider     string
	SettlementID *string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	Status       string
	Checked      int
	Mismatched   int
	Summary      string `gorm:"type:jsonb"`
	Error        string
	CreatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
}

type Item struct {
	ID         int64      `json:"id"`
	RunID      string     `json:"run_id"`
	PaymentID  *string    `json:"payment_id,omitempty"`
	Reference  string     `json:"reference,omitempty"`
	Provider   string     `json:"provider"`
	Issue      string     `json:"issue"`
	Severity   string     `json:"severity"`
	Expected   string     `json:"expected"`
	Actual     string     `json:"actual"`
	Status     string     `json:"status"`
	Resolution string     `json:"resolution,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// NewItem builds an open item with its severity.
func NewItem(runID, paymentID, reference, provider, issue, expected, actual string, now time.Time) *Item {
	it := &Item{RunID: runID, Reference: reference, Provider: provider, Issue: issue, Severity: Severity[issue],
		Expected: expected, Actual: actual, Status: ItemOpen, CreatedAt: now}
	if paymentID != "" {
		it.PaymentID = &paymentID
	}
	if it.Reference == "" {
		it.Reference = paymentID
	}
	return it
}

func NewRun(kind string, from, to, now time.Time) (*Run, error) {
	if !from.Before(to) {
		return nil, apperror.Invalid("invalid_period", "from must be before to")
	}
	if to.Sub(from) > 31*24*time.Hour {
		return nil, apperror.Invalid("invalid_period", "period cannot exceed 31 days")
	}
	return &Run{ID: uuid.NewString(), Kind: kind, PeriodStart: from, PeriodEnd: to, Status: RunQueued,
		Summary: "{}", CreatedAt: now}, nil
}

// BusinessDay returns [start, end) of the calendar day before now in loc
// (e.g. Africa/Addis_Ababa), the period a daily run checks.
func BusinessDay(now time.Time, loc *time.Location) (time.Time, time.Time) {
	l := now.In(loc)
	end := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	return end.AddDate(0, 0, -1).UTC(), end.UTC()
}

// Captured: the customer's money was taken.
func Captured(status string) bool {
	return status == payments.StatusSucceeded || status == payments.StatusPartiallyRefunded || status == payments.StatusRefunded
}

// Compare checks our view of a payment against the provider's.
func Compare(p payments.Payment, t providers.Transaction) (issue, expected, actual string) {
	weCaptured := Captured(p.Status)
	switch {
	case weCaptured && t.Status != providers.StatusSucceeded:
		return IssueStatusMismatch, "succeeded", t.Status
	case !weCaptured && t.Status == providers.StatusSucceeded:
		// The customer was charged but we told the merchant it did not succeed: urgent.
		return IssueStatusMismatch, p.Status, t.Status
	case weCaptured && (t.Amount != p.Amount || t.Currency != p.Currency):
		return IssueAmountMismatch, FormatAmount(p.Amount, p.Currency), FormatAmount(t.Amount, t.Currency)
	}
	return "", "", ""
}

func FormatAmount(a int64, c string) string { return c + " " + money.FormatMajor(a) }

// ---- Settlement files ----

// Line is one row of a provider settlement report.
type Line struct {
	LineNo    int
	Reference string // our payment id (the order id sent to the provider)
	Kind      string // payment | refund
	Amount    int64  // minor units
	Fee       int64
	Currency  string
	SettledAt *time.Time
}

const MaxSettlementBytes = 20 << 20

// ParseSettlementCSV reads a settlement report. Required columns (any
// order, header row first): reference, amount, currency. Optional: type
// (payment|refund, default payment), fee, settled_at (RFC 3339 or
// YYYY-MM-DD). Amounts are in major units ("50.00"). Every bad line is
// reported with its number; nothing is imported if any line is bad.
func ParseSettlementCSV(data []byte) ([]Line, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, apperror.Invalid("invalid_settlement_file", "the file is empty or not CSV")
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	for _, need := range []string{"reference", "amount", "currency"} {
		if _, ok := col[need]; !ok {
			return nil, apperror.Invalid("invalid_settlement_file", "missing column "+need+" (need reference, amount, currency)")
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []Line
	var bad []string
	for n := 2; ; n++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, apperror.Invalid("invalid_settlement_file", fmt.Sprintf("line %d: %v", n, err))
		}
		l := Line{LineNo: n, Reference: get(rec, "reference"), Kind: strings.ToLower(get(rec, "type")),
			Currency: strings.ToUpper(get(rec, "currency"))}
		if l.Kind == "" {
			l.Kind = "payment"
		}
		var problems []string
		if l.Reference == "" {
			problems = append(problems, "reference is empty")
		}
		if l.Kind != "payment" && l.Kind != "refund" {
			problems = append(problems, "type must be payment or refund")
		}
		if !money.Supported(l.Currency) {
			problems = append(problems, "unsupported currency "+l.Currency)
		}
		if l.Amount, err = money.ParseMajor(get(rec, "amount")); err != nil || l.Amount <= 0 {
			problems = append(problems, "amount must be positive, e.g. 50.00")
		}
		if f := get(rec, "fee"); f != "" {
			if l.Fee, err = money.ParseMajor(f); err != nil || l.Fee < 0 {
				problems = append(problems, "fee must be a non-negative amount")
			}
		}
		if v := get(rec, "settled_at"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				t, err = time.Parse("2006-01-02", v)
			}
			if err != nil {
				problems = append(problems, "settled_at must be RFC 3339 or YYYY-MM-DD")
			} else {
				l.SettledAt = &t
			}
		}
		if len(problems) > 0 {
			bad = append(bad, fmt.Sprintf("line %d: %s", n, strings.Join(problems, "; ")))
			if len(bad) >= 20 {
				break
			}
			continue
		}
		out = append(out, l)
	}
	if len(bad) > 0 {
		return nil, apperror.Invalid("invalid_settlement_file", strings.Join(bad, " | "))
	}
	if len(out) == 0 {
		return nil, apperror.Invalid("invalid_settlement_file", "the file has no lines")
	}
	return out, nil
}

// Totals per currency, for summaries.
type Totals struct {
	Count    int   `json:"count"`
	Amount   int64 `json:"amount"`
	Fees     int64 `json:"fees"`
	Refunded int64 `json:"refunded,omitempty"`
	Net      int64 `json:"net,omitempty"`
}

func FormatInt(v int64) string { return strconv.FormatInt(v, 10) }
