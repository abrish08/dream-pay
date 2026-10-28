// Package merchant owns merchant accounts (the tenants of the gateway).
package merchant

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/merchant/internal/service"
	"paygate/internal/modules/merchant/internal/store"
	"paygate/internal/modules/merchant/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "merchant"

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
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, merchants.Permissions))
	return &Module{Base: base, svc: service.New(d.DB, store.New(d.DB), base.Events)}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	transport.NewHTTP(m.svc, m.Log).RegisterAdmin(r.Admin)
}

// Handles: merchants.GetMerchant. Publishes: merchants.MerchantUpdated.
func (m *Module) RegisterHandlers(b bus.Bus) {
	transport.RegisterBus(b, m.svc)
}
