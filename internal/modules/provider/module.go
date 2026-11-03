// Package provider talks to payment service providers (PSPs) and turns their
// callbacks into provider.transaction.updated events.
package provider

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

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
	Routes          map[string][]string // payment method -> providers in order (live)
	TestRoutes      map[string][]string // test mode: providers' TEST accounts (then the sandbox)
	AllowMockInLive bool

	// Mastercard Gateway (MPGS) card acquiring: the live merchant and,
	// optionally, the bank's TEST merchant for test mode. nil = not installed.
	MPGS, MPGSTest *MPGSConfig
}

// MPGSConfig: credentials from the acquiring bank (see docs/providers.md).
type MPGSConfig struct {
	BaseURL, MerchantID, APIPassword, APIVersion, MerchantName, NotificationSecret string
	Currencies                                                                     []string
	HTTP                                                                           *http.Client // tests only
}

type Module struct {
	*module.Base
	svc    *service.Service
	checks []func(context.Context) error
}

var _ module.Module = (*Module)(nil)

func New(d Deps) (*Module, error) {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	st := store.New(d.DB)

	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, providers.Permissions))

	// Direct provider adapters are added here as each integration is signed
	// and certified (see docs/PROVIDER_ONBOARDING.md) and listed in
	// PROVIDER_ROUTES. Methods without one report "unavailable" in live mode;
	// test mode uses TEST accounts (PROVIDER_TEST_ROUTES) or the sandbox.
	var (
		others []gateway.Gateway
		checks []func(context.Context) error
	)
	for _, c := range []struct {
		cfg     *MPGSConfig
		name    string
		sandbox bool
	}{{d.MPGS, "mpgs", false}, {d.MPGSTest, "mpgs_test", true}} {
		if c.cfg == nil {
			continue
		}
		g, err := gateway.NewMPGS(gateway.MPGSConfig{Name: c.name, Sandbox: c.sandbox, BaseURL: c.cfg.BaseURL,
			MerchantID: c.cfg.MerchantID, APIPassword: c.cfg.APIPassword, APIVersion: c.cfg.APIVersion,
			MerchantName: c.cfg.MerchantName, NotificationSecret: c.cfg.NotificationSecret,
			Currencies: c.cfg.Currencies, PublicBaseURL: d.PublicBaseURL, HTTP: c.cfg.HTTP})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.name, err)
		}
		others = append(others, g)
		name := c.name
		checks = append(checks, func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			version, err := g.Check(ctx)
			switch {
			case errors.Is(err, gateway.ErrMPGSCredentials):
				return fmt.Errorf("%s: %w: check the merchant id and API password from your bank", name, err)
			case err != nil:
				// Mastercard down at start-up must not stop the whole gateway:
				// card payments fail over / report unavailable until it is back.
				base.Log.Error("ALERT: Mastercard Gateway check failed; card payments may fail", "provider", name, "err", err)
			default:
				base.Log.Info("Mastercard Gateway reachable, credentials accepted", "provider", name, "gateway_version", version)
			}
			return nil
		})
	}

	for method, names := range mergeRoutes(d.Routes, d.TestRoutes) {
		if !providers.ValidMethod(method) {
			base.Log.Warn("PROVIDER_ROUTES names an unknown payment method", "method", method)
		}
		for _, n := range names {
			if !slices.ContainsFunc(others, func(g gateway.Gateway) bool { return g.Name() == n }) {
				base.Log.Warn("PROVIDER_ROUTES names a provider that is not installed", "method", method, "provider", n)
			}
		}
	}
	router := service.NewRouter(d.Routes, d.TestRoutes, d.AllowMockInLive)
	svc := service.New(st, base.Events, gateway.NewMock(st, d.PublicBaseURL), others, router, d.PublicBaseURL, base.Log)
	return &Module{Base: base, svc: svc, checks: checks}, nil
}

// Start checks each Mastercard merchant first: credentials the bank's
// gateway rejects stop start-up (a configuration error to fix now, not a
// surprise for the first customer).
func (m *Module) Start(ctx context.Context) error {
	for _, check := range m.checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return m.Base.Start(ctx)
}

func mergeRoutes(a, b map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, m := range []map[string][]string{a, b} {
		for k, v := range m {
			out[k] = append(out[k], v...)
		}
	}
	return out
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
