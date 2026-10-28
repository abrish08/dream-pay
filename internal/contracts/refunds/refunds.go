// Package refunds: messages owned by the refund module. Other modules only
// react to its events, so it defines no requests yet (add one only when some
// module needs an answer from refund).
package refunds

import (
	"time"

	"paygate/internal/platform/apperror"
)

const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	ErrNotFound          = apperror.NotFound("refund_not_found", "refund not found")
	ErrNotRefundable     = apperror.Unprocessable("payment_not_refundable", "only succeeded payments can be refunded")
	ErrExceedsRefundable = apperror.Unprocessable("refund_exceeds_available", "refund amount exceeds the refundable amount")
)

// ---- Events ----

const (
	TopicRefundSucceeded = "refund.succeeded.v1"
	TopicRefundFailed    = "refund.failed.v1"
)

type RefundSucceeded struct {
	RefundID    string    `json:"refund_id"`
	PaymentID   string    `json:"payment_id"`
	MerchantID  string    `json:"merchant_id"`
	Environment string    `json:"environment"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Provider    string    `json:"provider"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func (RefundSucceeded) EventTopic() string { return TopicRefundSucceeded }

type RefundFailed struct {
	RefundID    string    `json:"refund_id"`
	PaymentID   string    `json:"payment_id"`
	MerchantID  string    `json:"merchant_id"`
	Environment string    `json:"environment"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Reason      string    `json:"reason"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func (RefundFailed) EventTopic() string { return TopicRefundFailed }
