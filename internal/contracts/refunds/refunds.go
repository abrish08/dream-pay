// Package refunds: messages owned by the refund module.
package refunds

import (
	"time"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

// Refund as other modules see it.
type Refund struct {
	ID          string    `json:"id"`
	PaymentID   string    `json:"payment_id"`
	MerchantID  string    `json:"merchant_id"`
	Environment string    `json:"environment"`
	Provider    string    `json:"provider"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GetRefundsByID returns the refunds with these ids (unknown ids are left
// out). At most 500. Used by reconciliation to match settlement files.
type GetRefundsByID struct {
	bus.Returns[[]Refund]
	IDs []string `json:"ids"`
}

func (GetRefundsByID) Route() string { return "refunds.get_by_ids" }

// ListRefunds: refunds by status, provider and creation time, ordered by id.
type ListRefunds struct {
	bus.Returns[[]Refund]
	Statuses    []string  `json:"statuses"`
	Provider    string    `json:"provider,omitempty"`
	CreatedFrom time.Time `json:"created_from"`
	CreatedTo   time.Time `json:"created_to"`
	AfterID     string    `json:"after_id"`
	Limit       int       `json:"limit"`
}

func (ListRefunds) Route() string { return "refunds.list" }

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
