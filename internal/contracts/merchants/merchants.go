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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	FeeBPS    int64     `json:"fee_bps"` // platform fee in basis points: 150 = 1.5%
	CreatedAt time.Time `json:"created_at"`
}

// ---- Requests ----

// GetMerchant returns a merchant by id.
type GetMerchant struct {
	bus.Returns[Merchant]
	ID string `json:"id"`
}

func (GetMerchant) Route() string { return "merchants.get" }

// ---- Events ----

const TopicMerchantUpdated = "merchant.updated.v1"

// MerchantUpdated is published when a merchant is created or its status
// changes. Other services keep the fields they need (e.g. auth keeps the
// status) instead of asking the merchant service on every request.
type MerchantUpdated struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	FeeBPS    int64     `json:"fee_bps"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (MerchantUpdated) EventTopic() string { return TopicMerchantUpdated }
