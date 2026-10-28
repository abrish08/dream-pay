package merchants

import "paygate/internal/contracts/auth"

// Permissions checked by the merchant service (registered with auth at start-up).
const (
	PermRead   = "merchants:read"
	PermManage = "merchants:manage"
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View merchant accounts", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleFinance, auth.RoleSupport}},
	{Code: PermManage, Description: "Create merchants and change their status", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations}},
}
