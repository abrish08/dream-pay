package payments

import "paygate/internal/contracts/auth"

// Permissions checked by the payment service (registered with auth at start-up).
const (
	PermRead  = "payments:read"
	PermWrite = "payments:write"
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View payments", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleAnalyst, auth.RoleViewer}},
	{Code: PermWrite, Description: "Create payments", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleDeveloper}},
}
