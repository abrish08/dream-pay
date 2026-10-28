// Package refund owns refunds. It reads payments and calls providers only by
// sending requests on the bus; it never touches their code or tables.
package refund

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/refund/internal/service"
	"paygate/internal/modules/refund/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "refund"

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
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, refunds.Permissions))
	m := &Module{Base: base, svc: service.New(d.DB, base.Events, d.Bus, base.Log)}
	m.AddWorker(m.svc.RetryPending)
	return m
}

// Asks:      payments.GetPayment, providers.Refund
// Publishes: refunds.RefundSucceeded, refunds.RefundFailed
func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterPublic(r.Public)
}
