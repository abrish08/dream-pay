// Package reconciliation compares our payments with what providers report.
package reconciliation

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	reconciliationc "paygate/internal/contracts/reconciliation"
	"paygate/internal/modules/reconciliation/internal/service"
	"paygate/internal/modules/reconciliation/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "reconciliation"

type Deps struct {
	DB  *gorm.DB
	Bus bus.Bus
	Log *slog.Logger
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, reconciliationc.Permissions))
	m := &Module{Base: base, svc: service.New(d.DB, d.Bus, base.Log)}
	m.AddWorker(m.svc.Worker)
	return m
}

// Asks: payments.ListPayments, providers.VerifyTransaction
func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterAdmin(r.Admin)
}
