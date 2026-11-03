// Package disputes: messages owned by the dispute module (chargebacks).
package disputes

import (
	"time"

	"paygate/internal/contracts/auth"
)

const (
	StatusNeedsResponse = "needs_response" // the merchant should send evidence before evidence_due_by
	StatusUnderReview   = "under_review"   // evidence sent to the provider / issuer
	StatusWon           = "won"            // the money comes back to the merchant
	StatusLost          = "lost"           // the cardholder keeps the money

	OutcomeWon  = "won"
	OutcomeLost = "lost"
)

const (
	PermRead   = "disputes:read"
	PermWrite  = "disputes:write"  // merchant: send evidence, accept
	PermManage = "disputes:manage" // platform: open, submit, decide
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View disputes (chargebacks)", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleAnalyst}},
	{Code: PermWrite, Description: "Respond to disputes with evidence", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin}},
	{Code: PermManage, Description: "Open and decide disputes", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleFinance}},
}

// ---- Events ----

const (
	TopicDisputeOpened = "dispute.opened.v1"
	TopicDisputeClosed = "dispute.closed.v1"
)

// DisputeOpened: a cardholder disputed a payment. The disputed amount is
// taken from the merchant's balance and held until the dispute is decided.
type DisputeOpened struct {
	DisputeID     string    `json:"dispute_id"`
	PaymentID     string    `json:"payment_id"`
	MerchantID    string    `json:"merchant_id"`
	Environment   string    `json:"environment"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	Provider      string    `json:"provider"`
	Reason        string    `json:"reason"`
	EvidenceDueBy time.Time `json:"evidence_due_by"`
	OccurredAt    time.Time `json:"occurred_at"`
}

func (DisputeOpened) EventTopic() string { return TopicDisputeOpened }

// DisputeClosed: won (held money back to the merchant) or lost (it goes back
// to the cardholder; Fee is charged to the merchant).
type DisputeClosed struct {
	DisputeID   string    `json:"dispute_id"`
	PaymentID   string    `json:"payment_id"`
	MerchantID  string    `json:"merchant_id"`
	Environment string    `json:"environment"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Provider    string    `json:"provider"`
	Outcome     string    `json:"outcome"` // won | lost
	Fee         int64     `json:"fee"`
	Note        string    `json:"note,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

func (DisputeClosed) EventTopic() string { return TopicDisputeClosed }
