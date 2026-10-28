// Package domain: double-entry bookkeeping. Every posting is balanced
// (total debits == total credits) and entries are never updated or deleted.
package domain

import (
	"fmt"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/payouts"
	"paygate/internal/contracts/refunds"
	"paygate/internal/platform/apperror"
)

type Line struct {
	Account string
	Debit   int64
	Credit  int64
}

type Posting struct {
	Reference   string // unique: makes posting idempotent (e.g. "payment:<id>:captured")
	Currency    string
	Description string
	Lines       []Line
}

var ErrUnbalanced = apperror.New(apperror.KindInternal, "ledger_unbalanced", "posting does not balance")

func (p Posting) Validate() error {
	if p.Reference == "" || p.Currency == "" || len(p.Lines) < 2 {
		return ErrUnbalanced.WithMessage("posting needs a reference, a currency and at least two lines")
	}
	var dr, cr int64
	for _, l := range p.Lines {
		if l.Account == "" || l.Debit < 0 || l.Credit < 0 || (l.Debit > 0) == (l.Credit > 0) {
			return ErrUnbalanced.WithMessage(fmt.Sprintf("invalid line %+v", l))
		}
		dr += l.Debit
		cr += l.Credit
	}
	if dr != cr {
		return ErrUnbalanced.WithMessage(fmt.Sprintf("debits %d != credits %d", dr, cr))
	}
	return nil
}

// Chart of accounts. Test and live money never mix.
func MerchantAvailable(merchantID, env string) string {
	return "merchant:" + merchantID + ":available:" + env
}
func ProviderClearing(provider, env string) string {
	return "provider:" + provider + ":clearing:" + env
}
func PlatformFees(env string) string { return "platform:fees:" + env }

// MerchantPayoutsPending: money still owed to the merchant but held for a payout.
func MerchantPayoutsPending(merchantID, env string) string {
	return "merchant:" + merchantID + ":payouts_pending:" + env
}

// SettlementBank: the platform's bank account that payouts are sent from.
func SettlementBank(env string) string { return "platform:settlement_bank:" + env }

func PayoutReserved(payoutID, merchantID, env, currency string, amount int64) Posting {
	return Posting{Reference: "payout:" + payoutID + ":reserved", Currency: currency, Description: "payout hold",
		Lines: []Line{
			{Account: MerchantAvailable(merchantID, env), Debit: amount},
			{Account: MerchantPayoutsPending(merchantID, env), Credit: amount},
		}}
}

func PayoutPaid(e payouts.PayoutPaid) Posting {
	return Posting{Reference: "payout:" + e.PayoutID + ":paid", Currency: e.Currency,
		Description: "payout sent, bank ref " + e.BankReference, Lines: []Line{
			{Account: MerchantPayoutsPending(e.MerchantID, e.Environment), Debit: e.Amount},
			{Account: SettlementBank(e.Environment), Credit: e.Amount},
		}}
}

// PayoutReleased: a failed payout's hold goes back to the available balance.
func PayoutReleased(e payouts.PayoutFailed) Posting {
	return Posting{Reference: "payout:" + e.PayoutID + ":released", Currency: e.Currency,
		Description: "payout failed: " + e.Reason, Lines: []Line{
			{Account: MerchantPayoutsPending(e.MerchantID, e.Environment), Debit: e.Amount},
			{Account: MerchantAvailable(e.MerchantID, e.Environment), Credit: e.Amount},
		}}
}

// PaymentCaptured: the provider owes us the gross amount; we owe the merchant
// the net amount; the platform earns the fee.
func PaymentCaptured(e payments.PaymentSucceeded) Posting {
	lines := []Line{{Account: ProviderClearing(e.Provider, e.Environment), Debit: e.Amount}}
	if net := e.Amount - e.Fee; net > 0 {
		lines = append(lines, Line{Account: MerchantAvailable(e.MerchantID, e.Environment), Credit: net})
	}
	if e.Fee > 0 {
		lines = append(lines, Line{Account: PlatformFees(e.Environment), Credit: e.Fee})
	}
	return Posting{Reference: "payment:" + e.PaymentID + ":captured", Currency: e.Currency,
		Description: "payment " + e.Reference, Lines: lines}
}

// RefundSucceeded: the refund comes out of the merchant's balance and goes
// back through the provider. The platform fee is not returned.
func RefundSucceeded(e refunds.RefundSucceeded) Posting {
	return Posting{Reference: "refund:" + e.RefundID + ":succeeded", Currency: e.Currency,
		Description: "refund of payment " + e.PaymentID, Lines: []Line{
			{Account: MerchantAvailable(e.MerchantID, e.Environment), Debit: e.Amount},
			{Account: ProviderClearing(e.Provider, e.Environment), Credit: e.Amount},
		}}
}
