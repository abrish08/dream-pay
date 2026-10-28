package domain

import (
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/refunds"
	"paygate/internal/platform/apperror"
)

type Refund struct {
	ID                string
	PaymentID         string
	MerchantID        string
	Environment       string
	Provider          string
	Amount            int64
	Currency          string
	Status            string
	Reason            string
	ProviderRefundRef string
	FailureReason     string
	IdempotencyKey    string
	RequestHash       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Snapshot of the payment taken from the payment module's contract.
type PaymentSnapshot struct {
	ID, MerchantID, Environment, Provider, Currency, Status string
	Amount                                                  int64
}

// AvailableToRefund uses the refund module's OWN data (pending + succeeded
// refunds), not the payment's refunded_amount, which is updated by an event
// and may lag behind.
func AvailableToRefund(paymentAmount, alreadyCommitted int64) int64 {
	return max(paymentAmount-alreadyCommitted, 0)
}

func NewRefund(p PaymentSnapshot, amount int64, reason, idemKey, hash string, available int64, now time.Time) (*Refund, error) {
	if amount == 0 {
		amount = available // full (remaining) refund
	}
	if amount < 0 {
		return nil, apperror.Invalid("invalid_amount", "amount must be positive")
	}
	if amount == 0 || amount > available {
		return nil, refunds.ErrExceedsRefundable
	}
	if len(reason) > 500 {
		return nil, apperror.Invalid("invalid_reason", "reason is too long")
	}
	return &Refund{
		ID: uuid.NewString(), PaymentID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
		Provider: p.Provider, Amount: amount, Currency: p.Currency, Status: refunds.StatusPending,
		Reason: reason, IdempotencyKey: idemKey, RequestHash: hash, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (r *Refund) Succeed(providerRef string, now time.Time) {
	r.Status, r.ProviderRefundRef, r.UpdatedAt = refunds.StatusSucceeded, providerRef, now
}

func (r *Refund) Fail(reason string, now time.Time) {
	r.Status, r.FailureReason, r.UpdatedAt = refunds.StatusFailed, reason, now
}

func (r *Refund) Final() bool { return r.Status != refunds.StatusPending }
