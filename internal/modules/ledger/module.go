// Package ledger keeps the double-entry books. It only reacts to events.
package ledger

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	ledgerc "paygate/internal/contracts/ledger"
	"paygate/internal/modules/ledger/internal/service"
	"paygate/internal/modules/ledger/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "ledger"

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
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, ledgerc.Permissions))
	return &Module{Base: base, svc: service.New(d.DB, base.Log)}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Subscribes: payments.PaymentSucceeded, refunds.RefundSucceeded
func (m *Module) RegisterHandlers(b bus.Bus) {
	bus.On(b, "ledger.on_payment_succeeded", m.svc.OnPaymentSucceeded)
	bus.On(b, "ledger.on_refund_succeeded", m.svc.OnRefundSucceeded)
}
