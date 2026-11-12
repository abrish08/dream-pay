// Package audit: messages owned by the audit service (who did what).
package audit

import (
	"paygate/internal/contracts/auth"
	"paygate/internal/platform/auditlog"
	"paygate/internal/platform/bus"
)

// Record stores one audit entry. Sent by every process's auditlog.Recorder.
type Record struct {
	bus.Returns[Recorded]
	Entry auditlog.Entry `json:"entry"`
}

type Recorded struct {
	ID int64 `json:"id"`
}

func (Record) Route() string { return "audit.record" }

const (
	PermRead    = "audit:read"     // a merchant's own activity log
	PermReadAll = "audit:read_all" // every action on the platform, and the integrity check
)

var Permissions = []auth.PermissionDef{
	{Code: PermRead, Description: "View the business's activity log", Audience: auth.AudienceMerchant,
		DefaultRoles: []string{auth.RoleAdmin}},
	{Code: PermReadAll, Description: "View the platform audit log and verify its integrity", Audience: auth.AudiencePlatform,
		DefaultRoles: []string{auth.RoleOperations}},
}
