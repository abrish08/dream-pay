// Package payout pays merchants their earned balance (settlement).
package payout

import (
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/payouts"
	"paygate/internal/modules/payout/internal/service"
	"paygate/internal/modules/payout/internal/store"
	"paygate/internal/modules/payout/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

const Name = "payout"

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
	base.Models(store.Models()...) // schema from GORM models, auto-migrated (additive only)
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, payouts.Permissions))
	svc := service.New(d.DB, base.Events, d.Bus, base.Log)
	base.AddWorker(svc.Worker)
	return &Module{Base: base, svc: svc}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Asks:      merchants.GetMerchant, ledger.ReserveFunds
// Publishes: payouts.PayoutPaid, payouts.PayoutFailed
func (m *Module) RegisterHandlers(bus.Bus) {}
