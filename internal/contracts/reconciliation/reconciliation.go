// Package reconciliation: messages owned by the reconciliation service.
package reconciliation

import (
	"time"

	"paygate/internal/contracts/auth"
)

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

// TopicSettlementReconciled: a provider's settlement report was checked;
// the ledger books the cash that reached the bank.
const TopicSettlementReconciled = "reconciliation.settlement_reconciled.v1"

// SettlementReconciled: one per currency of a settlement report. The ledger
// moves Gross-Refunds out of the provider's clearing account, books the
// provider's fees as an expense and Deposit as cash in the bank; any
// difference goes to a settlement-differences account for finance.
type SettlementReconciled struct {
	SettlementID string    `json:"settlement_id"`
	Provider     string    `json:"provider"`
	Environment  string    `json:"environment"`
	Currency     string    `json:"currency"`
	Gross        int64     `json:"gross"`         // payments in the report
	Refunds      int64     `json:"refunds"`       // refunds in the report
	ProviderFees int64     `json:"provider_fees"` // the provider's fees
	Deposit      int64     `json:"deposit"`       // what reached the bank (or the report's net when not given)
	OccurredAt   time.Time `json:"occurred_at"`
}

func (SettlementReconciled) EventTopic() string { return TopicSettlementReconciled }
