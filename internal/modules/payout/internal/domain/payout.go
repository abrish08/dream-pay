// Package domain: a payout moves a merchant's earned balance to its bank account.
//
//	reserving --(ledger holds funds)--> requested --(transfer sent)--> paid
//	    |                                   |
//	    +--(not enough balance)--> rejected +--(transfer failed)--> failed (funds released)
package domain

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/payouts"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/money"
)

var transitions = map[string][]string{
	payouts.StatusReserving: {payouts.StatusRequested, payouts.StatusRejected},
	payouts.StatusRequested: {payouts.StatusPaid, payouts.StatusFailed},
}

var ErrInvalidTransition = apperror.Conflict("invalid_payout_transition", "payout status change not allowed")

type BankAccount struct {
	BankName      string `json:"bank_name"`
	AccountNumber string `json:"account_number"`
	AccountName   string `json:"account_name"`
}

type Payout struct {
	ID             string
	MerchantID     string
	Environment    string
	Amount         int64
	Currency       string
	Status         string
	BankName       string
	AccountNumber  string
	AccountName    string
	Description    string
	BankReference  string
	FailureReason  string
	IdempotencyKey string
	RequestHash    string
	RequestedBy    string
	ProcessedBy    string
	PaidAt         *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// MinPayout avoids transfers that cost more than they move (100.00).
const MinPayout = 10_000

var accountRe = regexp.MustCompile(`^[0-9]{6,20}$`)

type NewPayoutInput struct {
	MerchantID, Environment, Currency, Description string
	Amount                                         int64
	Bank                                           BankAccount
	IdempotencyKey, RequestHash, RequestedBy       string
}

func NewPayout(in NewPayoutInput, now time.Time) (*Payout, error) {
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	b := in.Bank
	b.BankName, b.AccountName = strings.TrimSpace(b.BankName), strings.TrimSpace(b.AccountName)
	b.AccountNumber = strings.ReplaceAll(strings.TrimSpace(b.AccountNumber), " ", "")
	switch {
	case in.Environment != "live" && in.Environment != "test":
		return nil, apperror.Invalid("invalid_environment", "environment must be live or test")
	case !money.Supported(cur):
		return nil, apperror.Invalid("unsupported_currency", "currency must be ETB or USD")
	case in.Amount < MinPayout || in.Amount > money.MaxAmount:
		return nil, apperror.Invalid("invalid_amount", fmt.Sprintf("payout amount must be at least %d (minor units)", MinPayout))
	case b.BankName == "" || len(b.BankName) > 100:
		return nil, apperror.Invalid("invalid_bank", "bank_account.bank_name is required")
	case !accountRe.MatchString(b.AccountNumber):
		return nil, apperror.Invalid("invalid_bank", "bank_account.account_number must be 6-20 digits")
	case b.AccountName == "" || len(b.AccountName) > 100:
		return nil, apperror.Invalid("invalid_bank", "bank_account.account_name is required")
	case len(in.Description) > 200:
		return nil, apperror.Invalid("invalid_description", "description is at most 200 characters")
	}
	return &Payout{ID: uuid.NewString(), MerchantID: in.MerchantID, Environment: in.Environment, Amount: in.Amount,
		Currency: cur, Status: payouts.StatusReserving, BankName: b.BankName, AccountNumber: b.AccountNumber,
		AccountName: b.AccountName, Description: strings.TrimSpace(in.Description), IdempotencyKey: in.IdempotencyKey,
		RequestHash: in.RequestHash, RequestedBy: in.RequestedBy, CreatedAt: now, UpdatedAt: now}, nil
}

func (p *Payout) transition(next string, now time.Time) error {
	if !slices.Contains(transitions[p.Status], next) {
		return ErrInvalidTransition.WithMessage(fmt.Sprintf("cannot move payout from %s to %s", p.Status, next))
	}
	p.Status, p.UpdatedAt = next, now
	return nil
}

func (p *Payout) Reserved(now time.Time) error { return p.transition(payouts.StatusRequested, now) }

func (p *Payout) Reject(reason string, now time.Time) error {
	if err := p.transition(payouts.StatusRejected, now); err != nil {
		return err
	}
	p.FailureReason = reason
	return nil
}

func (p *Payout) MarkPaid(bankRef, by string, now time.Time) error {
	if strings.TrimSpace(bankRef) == "" || len(bankRef) > 100 {
		return apperror.Invalid("invalid_bank_reference", "bank_reference (the transfer reference) is required")
	}
	if err := p.transition(payouts.StatusPaid, now); err != nil {
		return err
	}
	p.BankReference, p.ProcessedBy, p.PaidAt = strings.TrimSpace(bankRef), by, &now
	return nil
}

func (p *Payout) MarkFailed(reason, by string, now time.Time) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 200 {
		return apperror.Invalid("invalid_reason", "reason is required")
	}
	if err := p.transition(payouts.StatusFailed, now); err != nil {
		return err
	}
	p.FailureReason, p.ProcessedBy = strings.TrimSpace(reason), by
	return nil
}

// MaskedAccount shows only the last 4 digits.
func (p *Payout) MaskedAccount() string {
	if len(p.AccountNumber) <= 4 {
		return p.AccountNumber
	}
	return strings.Repeat("•", len(p.AccountNumber)-4) + p.AccountNumber[len(p.AccountNumber)-4:]
}
