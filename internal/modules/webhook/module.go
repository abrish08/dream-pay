// Package webhook notifies merchants about events with signed HTTP callbacks.
package webhook

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/webhooks"
	"paygate/internal/modules/webhook/internal/domain"
	"paygate/internal/modules/webhook/internal/service"
	"paygate/internal/modules/webhook/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
	"paygate/internal/platform/secrets"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "webhook"

type Deps struct {
	DB            *gorm.DB
	Bus           bus.Bus
	Log           *slog.Logger
	EncryptionKey []byte
	AllowInsecure bool
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) (*Module, error) {
	box, err := secrets.NewBox(d.EncryptionKey)
	if err != nil {
		return nil, err
	}
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, webhooks.Permissions))
	m := &Module{Base: base, svc: service.New(d.DB, box, d.AllowInsecure, base.Log)}
	m.AddWorker(m.svc.DeliveryWorker)
	return m, nil
}

func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterPublic(r.Public)
}

// Subscribes: payments.PaymentSucceeded/Failed, refunds.RefundSucceeded/Failed
// (untyped: it forwards any of them to merchants as-is).
func (m *Module) RegisterHandlers(b bus.Bus) {
	for topic := range domain.PublicEventTypes {
		b.Subscribe(topic, "webhook.fanout."+topic, m.svc.OnEvent)
	}
}
