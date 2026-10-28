// Package provider talks to payment service providers (PSPs) and turns their
// callbacks into provider.transaction.updated events.
package provider

import (
	"embed"
	"log/slog"
	"slices"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/internal/gateway"
	"paygate/internal/modules/provider/internal/service"
	"paygate/internal/modules/provider/internal/store"
	"paygate/internal/modules/provider/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "provider"

type Deps struct {
	DB              *gorm.DB
	Bus             bus.Bus
	Log             *slog.Logger
	PublicBaseURL   string
	Routes          map[string][]string // payment method -> providers in order
	AllowMockInLive bool
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	st := store.New(d.DB)

	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, providers.Permissions))

	// Direct provider adapters are added here as each integration is signed
	// and certified (see docs/PROVIDER_ONBOARDING.md), e.g.
	//   others = append(others, gateway.NewTelebirr(cfg.Telebirr))
	// and listed in PROVIDER_ROUTES. Until then live payments report the
	// method as unavailable; test mode always uses the sandbox.
	var others []gateway.Gateway

	for method, names := range d.Routes {
		if !providers.ValidMethod(method) {
			base.Log.Warn("PROVIDER_ROUTES names an unknown payment method", "method", method)
		}
		for _, n := range names {
			if !slices.ContainsFunc(others, func(g gateway.Gateway) bool { return g.Name() == n }) {
				base.Log.Warn("PROVIDER_ROUTES names a provider that is not installed", "method", method, "provider", n)
			}
		}
	}
	router := service.NewRouter(d.Routes, d.AllowMockInLive)
	svc := service.New(st, base.Events, gateway.NewMock(st, d.PublicBaseURL), others, router, d.PublicBaseURL, base.Log)
	return &Module{Base: base, svc: svc}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterOpen(r.Open)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Handles: providers.InitiateCheckout, ListMethods, VerifyTransaction, Refund.
// Publishes: providers.TransactionUpdated.
func (m *Module) RegisterHandlers(b bus.Bus) {
	transport.RegisterBus(b, m.svc)
}
