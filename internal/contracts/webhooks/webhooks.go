// Package webhooks: messages owned by the webhook service.
package webhooks

import "paygate/internal/contracts/auth"

// Permissions checked by the webhook service (registered with auth at start-up).
const PermManage = "webhooks:write"

var Permissions = []auth.PermissionDef{
	{Code: PermManage, Description: "Manage webhook endpoints", Audience: auth.AudienceMerchant, APIKey: true,
		DefaultRoles: []string{auth.RoleAdmin, auth.RoleDeveloper}},
}
