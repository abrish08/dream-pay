// Package auth is the identity service: dashboard login for platform staff
// and merchant teams, RBAC, merchant API keys, and the API key -> access
// token exchange that every other service uses to authenticate requests.
package auth

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"paygate/internal/modules/auth/internal/domain"
	"paygate/internal/modules/auth/internal/service"
	"paygate/internal/modules/auth/internal/store"
	"paygate/internal/modules/auth/internal/transport"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/mail"
	"paygate/internal/platform/module"
	"paygate/internal/platform/secrets"
	"paygate/internal/platform/sms"
)

const Name = "auth"

type Deps struct {
	DB  *gorm.DB
	Bus bus.Bus
	Log *slog.Logger

	Signer   *authn.Signer   // private signing key: only this service has it
	Verifier *authn.Verifier // published as JWKS

	Pepper         []byte // API key pepper (current)
	PepperVersion  int
	PreviousPepper []byte // optional, while rotating the pepper

	BootstrapEmail    string
	BootstrapPassword string
	BootstrapName     string

	Identity     map[string]*authn.OIDC // Google / Apple sign-in (only the configured ones)
	Box          *secrets.Box           // encrypts authenticator secrets (DATA_ENCRYPTION_KEY)
	SMS          sms.Sender             // verification codes by SMS (send.et); nil = email only
	Mailer       mail.Sender
	DashboardURL string
	ResetTTL     time.Duration
}

type Module struct {
	*module.Base
	svc      *service.Service
	verifier *authn.Verifier
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	// No SQL files: the schema comes from the GORM models (auto-migrated,
	// additive only). Add a migrations/ folder only for renames or data fixes.
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, nil)
	base.Models(store.Models()...)
	cfg := service.Config{
		Signer:            d.Signer,
		Pepper:            domain.Pepper{Version: d.PepperVersion, Secret: d.Pepper},
		BootstrapEmail:    d.BootstrapEmail,
		BootstrapPassword: d.BootstrapPassword,
		BootstrapName:     d.BootstrapName,
		Identity:          d.Identity, Box: d.Box, SMS: d.SMS, Mailer: d.Mailer, DashboardURL: d.DashboardURL, ResetTTL: d.ResetTTL,
	}
	if len(d.PreviousPepper) > 0 {
		cfg.PreviousPepper = &domain.Pepper{Version: d.PepperVersion - 1, Secret: d.PreviousPepper}
	}
	return &Module{Base: base, svc: service.New(store.New(d.DB), d.Bus, base.Events, cfg, base.Log), verifier: d.Verifier}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.verifier, m.Log)
	h.RegisterOpen(r.Open)
	h.RegisterAuthenticated(r.Authenticated)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Handles: auth.ExchangeAPIKey, auth.RegisterPermissions.
// Asks: merchants.GetMerchant (only for a merchant it has not seen yet).
// Subscribes: merchants.MerchantUpdated. Publishes: auth.AccessChanged.
func (m *Module) RegisterHandlers(b bus.Bus) {
	transport.RegisterBus(b, m.svc)
	bus.On(b, "auth.on_merchant_updated", m.svc.OnMerchantUpdated)
}

// Migrate auto-migrates the tables, then seeds the built-in roles and auth's
// own permissions (idempotent, never overwrites edits).
func (m *Module) Migrate(ctx context.Context) error {
	if err := m.Base.Migrate(ctx); err != nil {
		return err
	}
	return m.svc.Seed(ctx)
}

// Start creates the first super_admin if needed, then starts the outbox relay.
func (m *Module) Start(ctx context.Context) error {
	if err := m.svc.Bootstrap(ctx); err != nil {
		return err
	}
	return m.Base.Start(ctx)
}
