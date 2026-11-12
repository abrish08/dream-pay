// Package audit keeps the audit log: who did what, for the whole backend.
// Every process records actions (HTTP middleware) and sends them here over
// the bus; this module stores them append-only in a hash chain.
package audit

import (
	"embed"
	"log/slog"

	"gorm.io/gorm"

	auditc "paygate/internal/contracts/audit"
	authc "paygate/internal/contracts/auth"
	"paygate/internal/modules/audit/internal/service"
	"paygate/internal/modules/audit/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "audit"

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
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, auditc.Permissions))
	return &Module{Base: base, svc: service.New(d.DB, base.Log)}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Handles: audit.Record.
func (m *Module) RegisterHandlers(b bus.Bus) {
	bus.Handle(b, m.svc.Record)
}
