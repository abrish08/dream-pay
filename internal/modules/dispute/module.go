// Package dispute handles chargebacks: a cardholder disputes a payment, the
// merchant answers with evidence, the issuer decides. The ledger holds the
// disputed money from the start and settles it with the outcome.
package dispute

import (
	"log/slog"
	"time"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/disputes"
	"paygate/internal/modules/dispute/internal/service"
	"paygate/internal/modules/dispute/internal/store"
	"paygate/internal/modules/dispute/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/metrics"
	"paygate/internal/platform/module"
)

const Name = "dispute"

type Deps struct {
	DB            *gorm.DB
	Bus           bus.Bus
	Log           *slog.Logger
	DeadlineEvery time.Duration // tests only
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, nil)
	base.Models(store.Models()...) // GORM models, auto-migrated (additive only)
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, disputes.Permissions))
	metrics.SQLGauge("paygate_disputes_open", "Open disputes by status", nil, d.DB,
		`SELECT status, COUNT(*) AS n FROM disputes WHERE status IN ('needs_response', 'under_review') GROUP BY status`, "status")
	svc := service.New(d.DB, base.Events, d.Bus, base.Log)
	svc.DeadlineEvery = d.DeadlineEvery
	base.AddWorker(svc.DeadlineWorker)
	return &Module{Base: base, svc: svc}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Asks: payments.GetPaymentsByID. Publishes: disputes.DisputeOpened, DisputeClosed.
func (m *Module) RegisterHandlers(bus.Bus) {}
