// Package reconciliation compares our payments with what providers report.
package reconciliation

import (
	"embed"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	reconciliationc "paygate/internal/contracts/reconciliation"
	"paygate/internal/modules/reconciliation/internal/service"
	"paygate/internal/modules/reconciliation/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/metrics"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "reconciliation"

type Deps struct {
	DB  *gorm.DB
	Bus bus.Bus
	Log *slog.Logger
	// Timezone of the business day (default Africa/Addis_Ababa) and how long
	// after local midnight the daily run starts (default 1h).
	Timezone   string
	DailyAfter time.Duration
	SettleWait time.Duration // tests only
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) (*Module, error) {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, reconciliationc.Permissions))
	metrics.SQLGauge("paygate_reconciliation_open_items", "Open reconciliation differences by severity", nil, d.DB,
		`SELECT severity, COUNT(*) AS n FROM reconciliation_items WHERE status = 'open' GROUP BY severity`, "severity")
	opt := service.Options{DailyAfter: d.DailyAfter, SettleWait: d.SettleWait}
	if d.Timezone != "" {
		loc, err := time.LoadLocation(d.Timezone)
		if err != nil {
			return nil, fmt.Errorf("RECONCILIATION_TIMEZONE: %w", err)
		}
		opt.Location = loc
	}
	m := &Module{Base: base, svc: service.New(d.DB, d.Bus, base.Events, opt, base.Log)}
	m.AddWorker(m.svc.Worker)
	return m, nil
}

// Asks: payments.ListPayments, payments.GetPaymentsByID, payments.SyncPayment,
// providers.VerifyTransaction, ledger.GetPostings
func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterAdmin(r.Admin)
}
