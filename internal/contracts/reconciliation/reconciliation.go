// Package reconciliation: messages owned by the reconciliation service.
package reconciliation

import "paygate/internal/contracts/auth"

// Permissions checked by the reconciliation service (registered with auth at start-up).
const PermRun = "reconciliation:run"

var Permissions = []auth.PermissionDef{
	{Code: PermRun, Description: "Run and view reconciliations", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleFinance}},
}
