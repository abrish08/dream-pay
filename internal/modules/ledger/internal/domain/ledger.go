// Package domain: double-entry bookkeeping. Every posting is balanced
// (total debits == total credits) and entries are never updated or deleted.
package domain

import (
	"fmt"
	"strings"

	"paygate/internal/contracts/disputes"
	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/payouts"
	"paygate/internal/contracts/reconciliation"
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

// MerchantReserve: the merchant's money held as a rolling reserve.
func MerchantReserve(merchantID, env string) string {
	return "merchant:" + merchantID + ":reserve:" + env
}

// MerchantDisputeHold: the merchant's money held for open disputes.
func MerchantDisputeHold(merchantID, env string) string {
	return "merchant:" + merchantID + ":dispute_hold:" + env
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
// Any reserve is credited to the merchant's reserve account instead of the
// available balance, and released later (ReserveReleased).
func PaymentCaptured(e payments.PaymentSucceeded) Posting {
	lines := []Line{{Account: ProviderClearing(e.Provider, e.Environment), Debit: e.Amount}}
	net := e.Amount - e.Fee
	reserve := min(max(e.Reserve, 0), max(net, 0))
	if available := net - reserve; available > 0 {
		lines = append(lines, Line{Account: MerchantAvailable(e.MerchantID, e.Environment), Credit: available})
	}
	if reserve > 0 {
		lines = append(lines, Line{Account: MerchantReserve(e.MerchantID, e.Environment), Credit: reserve})
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

// ReserveReleased: a payment's reserve becomes available after its days.
func ReserveReleased(paymentID, merchantID, env, currency string, amount int64) Posting {
	return Posting{Reference: "reserve:" + paymentID + ":released", Currency: currency, Description: "reserve released",
		Lines: []Line{
			{Account: MerchantReserve(merchantID, env), Debit: amount},
			{Account: MerchantAvailable(merchantID, env), Credit: amount},
		}}
}

// DisputeOpened: the disputed amount leaves the merchant's available balance
// and is held until the dispute is decided.
func DisputeOpened(e disputes.DisputeOpened) Posting {
	return Posting{Reference: "dispute:" + e.DisputeID + ":opened", Currency: e.Currency,
		Description: "dispute on payment " + e.PaymentID, Lines: []Line{
			{Account: MerchantAvailable(e.MerchantID, e.Environment), Debit: e.Amount},
			{Account: MerchantDisputeHold(e.MerchantID, e.Environment), Credit: e.Amount},
		}}
}

// DisputeClosed: won, the hold goes back to the merchant; lost, the provider
// returns it to the cardholder (it owes us that much less) and the dispute
// fee is charged.
func DisputeClosed(e disputes.DisputeClosed) Posting {
	hold := MerchantDisputeHold(e.MerchantID, e.Environment)
	if e.Outcome == disputes.OutcomeWon {
		return Posting{Reference: "dispute:" + e.DisputeID + ":closed", Currency: e.Currency, Description: "dispute won",
			Lines: []Line{{Account: hold, Debit: e.Amount}, {Account: MerchantAvailable(e.MerchantID, e.Environment), Credit: e.Amount}}}
	}
	lines := []Line{{Account: hold, Debit: e.Amount}, {Account: ProviderClearing(e.Provider, e.Environment), Credit: e.Amount}}
	if e.Fee > 0 {
		lines = append(lines, Line{Account: MerchantAvailable(e.MerchantID, e.Environment), Debit: e.Fee},
			Line{Account: PlatformFees(e.Environment), Credit: e.Fee})
	}
	return Posting{Reference: "dispute:" + e.DisputeID + ":closed", Currency: e.Currency, Description: "dispute lost", Lines: lines}
}

// ---- Refund holds, settlements, corrections ----

// MerchantRefundsPending: the merchant's money held for refunds in progress.
func MerchantRefundsPending(merchantID, env string) string {
	return "merchant:" + merchantID + ":refunds_pending:" + env
}

// ProviderFees: what providers charge us (an expense of the platform).
func ProviderFees(provider, env string) string {
	return "platform:provider_fees:" + provider + ":" + env
}

// SettlementDifferences: deposits that did not match a provider's report,
// waiting for finance to explain them.
func SettlementDifferences(env string) string { return "platform:settlement_differences:" + env }

// Adjustments: the platform's side of approved manual corrections.
func Adjustments(env string) string { return "platform:adjustments:" + env }

// RefundHeld: before a refund goes to the provider its amount is held.
func RefundHeld(refundID, merchantID, env, currency string, amount int64) Posting {
	return Posting{Reference: RefundHoldRef(refundID), Currency: currency, Description: "refund hold",
		Lines: []Line{
			{Account: MerchantAvailable(merchantID, env), Debit: amount},
			{Account: MerchantRefundsPending(merchantID, env), Credit: amount},
		}}
}

func RefundHoldRef(refundID string) string { return "refund:" + refundID + ":held" }

// RefundPaidFromHold: the held amount goes back to the customer through the provider.
func RefundPaidFromHold(e refunds.RefundSucceeded) Posting {
	return Posting{Reference: "refund:" + e.RefundID + ":succeeded", Currency: e.Currency,
		Description: "refund of payment " + e.PaymentID, Lines: []Line{
			{Account: MerchantRefundsPending(e.MerchantID, e.Environment), Debit: e.Amount},
			{Account: ProviderClearing(e.Provider, e.Environment), Credit: e.Amount},
		}}
}

// RefundReleased: a failed refund's hold goes back to the available balance.
func RefundReleased(e refunds.RefundFailed) Posting {
	return Posting{Reference: "refund:" + e.RefundID + ":released", Currency: e.Currency,
		Description: "refund failed: " + e.Reason, Lines: []Line{
			{Account: MerchantRefundsPending(e.MerchantID, e.Environment), Debit: e.Amount},
			{Account: MerchantAvailable(e.MerchantID, e.Environment), Credit: e.Amount},
		}}
}

// PaymentBanked: money paid by bank transfer lands directly in our bank, so
// the provider's clearing account is settled at once.
func PaymentBanked(e payments.PaymentSucceeded) Posting {
	return Posting{Reference: "payment:" + e.PaymentID + ":banked", Currency: e.Currency,
		Description: "bank transfer received " + e.Reference, Lines: []Line{
			{Account: SettlementBank(e.Environment), Debit: e.Amount},
			{Account: ProviderClearing(e.Provider, e.Environment), Credit: e.Amount},
		}}
}

// IsBankTransfer: providers whose money arrives straight in our bank account.
func IsBankTransfer(provider string) bool { return strings.HasPrefix(provider, "bank_transfer") }

// SettlementBooked: a provider paid out a settlement. The clearing account
// is reduced by what the report settled (payments minus refunds), the
// provider's fees are an expense, the deposit is cash in the bank, and any
// difference waits in settlement differences.
func SettlementBooked(e reconciliation.SettlementReconciled) Posting {
	settled := e.Gross - e.Refunds
	diff := e.Deposit - (settled - e.ProviderFees)
	var lines []Line
	add := func(account string, debit int64) { // positive: debit, negative: credit
		switch {
		case debit > 0:
			lines = append(lines, Line{Account: account, Debit: debit})
		case debit < 0:
			lines = append(lines, Line{Account: account, Credit: -debit})
		}
	}
	add(SettlementBank(e.Environment), e.Deposit)
	add(ProviderFees(e.Provider, e.Environment), e.ProviderFees)
	add(ProviderClearing(e.Provider, e.Environment), -settled)
	add(SettlementDifferences(e.Environment), -diff)
	return Posting{Reference: "settlement:" + e.SettlementID + ":" + e.Currency, Currency: e.Currency,
		Description: "settlement from " + e.Provider, Lines: lines}
}

// Adjustment directions.
const (
	AdjustCredit = "credit" // give the merchant money (e.g. a fee charged by mistake)
	AdjustDebit  = "debit"  // take money from the merchant (e.g. a duplicate credit)
)

// AdjustmentPosting: an approved manual correction of a merchant's balance.
func AdjustmentPosting(id, merchantID, env, currency, direction, reason string, amount int64) Posting {
	merchant, platform := MerchantAvailable(merchantID, env), Adjustments(env)
	lines := []Line{{Account: platform, Debit: amount}, {Account: merchant, Credit: amount}}
	if direction == AdjustDebit {
		lines = []Line{{Account: merchant, Debit: amount}, {Account: platform, Credit: amount}}
	}
	return Posting{Reference: "adjustment:" + id, Currency: currency, Description: "correction: " + reason, Lines: lines}
}
