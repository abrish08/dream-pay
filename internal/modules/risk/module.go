// Package risk scores payments before they reach a provider: blocklists,
// velocity per customer and IP, repeated declines (card testing) and unusual
// amounts. Hard merchant limits are enforced by the payment module itself.
package risk

import (
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	riskc "paygate/internal/contracts/risk"
	"paygate/internal/modules/risk/internal/domain"
	"paygate/internal/modules/risk/internal/service"
	"paygate/internal/modules/risk/internal/store"
	"paygate/internal/modules/risk/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

const Name = "risk"

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
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, nil)
	base.Models(store.Models()...) // GORM models, auto-migrated (additive only)
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, riskc.Permissions))
	return &Module{Base: base, svc: service.New(d.DB, domain.DefaultThresholds, base.Log)}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterAdmin(r.Admin)
}

// Handles: risk.Assess. Subscribes: payments.PaymentFailed (declines).
func (m *Module) RegisterHandlers(b bus.Bus) {
	bus.Handle(b, m.svc.Assess)
	bus.On(b, "risk.on_payment_failed", m.svc.OnPaymentFailed)
}
