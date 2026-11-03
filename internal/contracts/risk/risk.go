// Package risk: messages owned by the risk module (fraud rules).
package risk

import (
	"paygate/internal/contracts/auth"
	"paygate/internal/platform/bus"
)

const (
	ActionAllow = "allow"
	ActionBlock = "block"
)

// Assess scores a payment before it is sent to a provider: blocklists,
// velocity (same customer / IP), repeated declines (card testing) and
// unusual amounts. Every assessment is recorded.
type Assess struct {
	bus.Returns[Decision]
	PaymentID     string `json:"payment_id"`
	MerchantID    string `json:"merchant_id"`
	Environment   string `json:"environment"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	CustomerEmail string `json:"customer_email,omitempty"`
	CustomerPhone string `json:"customer_phone,omitempty"`
	CustomerIP    string `json:"customer_ip,omitempty"`
	RiskLevel     string `json:"risk_level,omitempty"` // the merchant's: low | normal | high
}

type Decision struct {
	Action  string   `json:"action"` // allow | block
	Score   int      `json:"score"`  // 0-100+; block at 100
	Reasons []string `json:"reasons,omitempty"`
}

func (Assess) Route() string { return "risk.assess" }

const (
	PermRead   = "risk:read"
	PermManage = "risk:manage"
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View risk decisions and the blocklist", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleSupport, auth.RoleFinance}},
	{Code: PermManage, Description: "Manage the blocklist", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations}},
}
