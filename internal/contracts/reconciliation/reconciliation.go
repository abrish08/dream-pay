// Package reconciliation: messages owned by the reconciliation service.
package reconciliation

import "paygate/internal/contracts/auth"

// Permissions checked by the reconciliation service (registered with auth at start-up).
const (
	PermRun = "reconciliation:run"
	// PermResolve: close a difference after investigating it (finance only:
	// the people who run checks are not the ones who sign them off).
	PermResolve = "reconciliation:resolve"
)

var Permissions = []auth.PermissionDef{
	{Code: PermRun, Description: "Run and view reconciliations, upload settlement files", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations, auth.RoleFinance}},
	{Code: PermResolve, Description: "Resolve reconciliation differences", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleFinance}},
}
