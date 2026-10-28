// Package provider talks to payment service providers (PSPs) and turns their
// callbacks into provider.transaction.updated events.
package provider

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	"paygate/internal/modules/provider/internal/gateway"
	"paygate/internal/modules/provider/internal/service"
	"paygate/internal/modules/provider/internal/store"
	"paygate/internal/modules/provider/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/config"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "provider"

type Deps struct {
	DB            *gorm.DB
	Bus           bus.Bus
	Log           *slog.Logger
	PublicBaseURL string
	Chapa         config.ChapaConfig
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	st := store.New(d.DB)

	var others []gateway.Gateway
	if d.Chapa.SecretKey != "" {
		others = append(others, gateway.NewChapa(d.Chapa.BaseURL, d.Chapa.SecretKey, d.Chapa.WebhookSecret))
	}
	// telebirr, CBE Birr, Stripe...: implement gateway.Gateway and append here.

	svc := service.New(st, base.Events, gateway.NewMock(st, d.PublicBaseURL), others, d.PublicBaseURL, base.Log)
	return &Module{Base: base, svc: svc}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterOpen(r.Open)
}

// Handles: providers.InitiateCheckout, providers.VerifyTransaction, providers.Refund.
// Publishes: providers.TransactionUpdated.
func (m *Module) RegisterHandlers(b bus.Bus) {
	transport.RegisterBus(b, m.svc)
}
