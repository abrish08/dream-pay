// Package kyc: messages owned by the KYC service (merchant verification).
package kyc

import (
	"time"

	"paygate/internal/contracts/auth"
)

// Application status.
const (
	StatusNotStarted = "not_started" // merchant has not filled anything yet
	StatusDraft      = "draft"       // being filled in
	StatusSubmitted  = "submitted"   // waiting for review
	StatusApproved   = "approved"    // verified: the merchant can go live
	StatusRejected   = "rejected"    // reason given; merchant fixes and resubmits
	StatusWaived     = "waived"      // merchant created by staff (verified offline)
)

// Permissions checked by the KYC service (registered with auth at start-up).
const (
	PermRead   = "kyc:read"   // merchant: see own application
	PermWrite  = "kyc:write"  // merchant: fill in, upload, submit
	PermReview = "kyc:review" // platform: review, download, approve, reject
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View the business verification (KYC)", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin}},
	{Code: PermWrite, Description: "Fill in and submit the business verification (KYC)", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin}},
	{Code: PermReview, Description: "Review merchant verifications (KYC)", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations}},
}

// ---- Events ----

const TopicStatusChanged = "kyc.status_changed.v1"

// StatusChanged: a merchant's verification moved (submitted, approved,
// rejected). The merchant service activates a pending merchant on approval.
type StatusChanged struct {
	MerchantID string    `json:"merchant_id"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (StatusChanged) EventTopic() string { return TopicStatusChanged }
