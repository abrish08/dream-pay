// Package ledger: messages owned by the ledger service.
package ledger

import "paygate/internal/contracts/auth"

// Permissions checked by the ledger service (registered with auth at start-up).
const (
	PermBalanceRead = "balance:read" // a merchant's own balance
	PermLedgerRead  = "ledger:read"  // platform-wide books (trial balance)
)

var Permissions = []auth.PermissionDef{
	{Code: PermBalanceRead, Description: "View balance", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleAnalyst, auth.RoleViewer}},
	{Code: PermLedgerRead, Description: "View the platform ledger", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
}
