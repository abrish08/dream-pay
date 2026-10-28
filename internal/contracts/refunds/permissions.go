package refunds

import "paygate/internal/contracts/auth"

// Permissions checked by the refund service (registered with auth at start-up).
const PermWrite = "refunds:write"

var Permissions = []auth.PermissionDef{
	{Code: PermWrite, Description: "Refund payments", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleAnalyst}},
}
