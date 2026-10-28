// Package app is the composition root. It creates the central bus, builds the
// modules enabled for this process (MODULES), and connects every module to
// the bus. Modules receive no other module: whether a peer runs in this
// process or in another service is invisible to them.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/modules/auth"
	"paygate/internal/modules/ledger"
	"paygate/internal/modules/merchant"
	"paygate/internal/modules/payment"
	"paygate/internal/modules/provider"
	"paygate/internal/modules/reconciliation"
	"paygate/internal/modules/refund"
	"paygate/internal/modules/webhook"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/authn"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/config"
	"paygate/internal/platform/database"
	"paygate/internal/platform/httpx"
	"paygate/internal/platform/module"
)

// Modules is the list of every module in the system.
var Modules = []string{
	merchant.Name, auth.Name, provider.Name, payment.Name,
	refund.Name, ledger.Name, webhook.Name, reconciliation.Name,
}

type App struct {
	cfg      config.Config
	log      *slog.Logger
	bus      bus.Bus
	verifier *authn.Verifier // every process: verifies access tokens locally
	signer   *authn.Signer   // only where the auth module runs
	modules  []module.Module
	dbs      []*gorm.DB
	router   chi.Router
}

func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (_ *App, err error) {
	if err := cfg.Validate(Modules); err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, log: log}
	defer func() {
		if err != nil {
			a.closeResources()
		}
	}()

	switch cfg.Bus {
	case "memory":
		a.bus = bus.NewMemory(log)
	case "nats":
		if a.bus, err = bus.NewNATS(ctx, cfg.NATSURL, log); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown BUS %q (memory or nats)", cfg.Bus)
	}

	if err := a.buildAuth(); err != nil {
		return nil, err
	}
	a.bus.SetVerifier(a.verifier)

	// Every process (not just one replica) learns about role and permission
	// changes, so tokens issued before the change stop working everywhere.
	// A process that starts later replays the last 15 minutes (> token TTL).
	bus.OnEach(a.bus, 15*time.Minute, func(_ context.Context, e authc.AccessChanged) error {
		a.verifier.Revoke(e.UserIDs, e.RoleIDs, e.NotBefore)
		return nil
	})

	for _, name := range Modules {
		if !cfg.ModuleEnabled(name) {
			continue
		}
		m, err := a.newModule(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		a.modules = append(a.modules, m)
	}

	a.router = a.buildRouter()
	for _, m := range a.modules {
		m.RegisterHandlers(a.bus)
	}
	return a, nil
}

// newModule builds one module from platform services only.
func (a *App) newModule(name string) (module.Module, error) {
	db, err := a.openDB(name)
	if err != nil {
		return nil, err
	}
	b, log, cfg := a.bus, a.log, a.cfg
	switch name {
	case merchant.Name:
		return merchant.New(merchant.Deps{DB: db, Bus: b, Log: log}), nil
	case auth.Name:
		return auth.New(auth.Deps{DB: db, Bus: b, Log: log, Signer: a.signer, Verifier: a.verifier,
			Pepper: []byte(cfg.APIKeyPepper), PepperVersion: cfg.APIKeyPepperVersion,
			PreviousPepper: []byte(cfg.APIKeyPepperPrevious),
			BootstrapEmail: cfg.BootstrapAdminEmail, BootstrapPassword: cfg.BootstrapAdminPassword}), nil
	case provider.Name:
		return provider.New(provider.Deps{DB: db, Bus: b, Log: log, PublicBaseURL: cfg.PublicBaseURL, Chapa: cfg.Chapa}), nil
	case payment.Name:
		return payment.New(payment.Deps{DB: db, Bus: b, Log: log, AllowMockInLive: cfg.AllowMockInLive,
			PaymentTTL: cfg.PaymentTTL}), nil
	case refund.Name:
		return refund.New(refund.Deps{DB: db, Bus: b, Log: log}), nil
	case ledger.Name:
		return ledger.New(ledger.Deps{DB: db, Bus: b, Log: log}), nil
	case webhook.Name:
		return webhook.New(webhook.Deps{DB: db, Bus: b, Log: log, EncryptionKey: cfg.EncryptionKey, AllowInsecure: cfg.AllowInsecureWebhooks})
	case reconciliation.Name:
		return reconciliation.New(reconciliation.Deps{DB: db, Bus: b, Log: log}), nil
	}
	return nil, fmt.Errorf("unknown module")
}

// buildAuth sets up token verification for this process and, if the auth
// module runs here, token signing.
func (a *App) buildAuth() error {
	pubs := a.cfg.AuthPublicKeys
	if len(a.cfg.AuthSigningKey) > 0 {
		s, err := authn.NewSigner(a.cfg.AuthSigningKey)
		if err != nil {
			return err
		}
		if a.cfg.ModuleEnabled(auth.Name) {
			a.signer = s
		}
		pubs = append(pubs, s.PublicKey())
	}
	v, err := authn.NewVerifier(pubs...)
	if err != nil {
		return fmt.Errorf("auth keys: %w", err)
	}
	a.verifier = v
	return nil
}

func (a *App) buildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(httpx.Correlation, httpx.Recoverer(a.log), httpx.AccessLog(a.log))
	notFound := func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteError(w, req, a.log, apperror.NotFound("route_not_found", "no route for "+req.Method+" "+req.URL.Path))
	}
	r.NotFound(notFound)
	r.MethodNotAllowed(notFound)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "bus": a.cfg.Bus, "modules": a.ModuleNames(), "handles": a.bus.Routes(),
		})
	})

	// Every service authenticates for itself: user tokens are verified
	// locally; API keys are exchanged with the auth service over the bus
	// (wherever it runs) and cached for AuthKeyCacheTTL.
	authMW := &httpx.Auth{
		Verifier: a.verifier,
		Exchange: func(ctx context.Context, key string) (string, error) {
			t, err := bus.Ask(ctx, a.bus, authc.ExchangeAPIKey{Key: key})
			return t.Token, err
		},
		CacheTTL: a.cfg.AuthKeyCacheTTL,
		Log:      a.log,
	}

	var routers module.Routers
	r.Route("/v1", func(v chi.Router) {
		v.Use(authMW.Middleware, httpx.RequireMerchant(a.log))
		routers.Public = v
	})
	r.Route("/admin", func(ad chi.Router) {
		ad.Use(authMW.Middleware, httpx.RequirePlatform(a.log))
		routers.Admin = ad
	})
	r.Group(func(g chi.Router) {
		g.Use(authMW.Middleware)
		routers.Authenticated = g
	})
	r.Group(func(g chi.Router) { routers.Open = g })

	for _, m := range a.modules {
		m.RegisterRoutes(routers)
	}
	return r
}

func (a *App) openDB(name string) (*gorm.DB, error) {
	dsn, err := a.cfg.ModuleDSN(name)
	if err != nil {
		return nil, err
	}
	db, err := database.Open(dsn, a.cfg.DBMaxConns)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	a.dbs = append(a.dbs, db)
	return db, nil
}

func (a *App) Handler() http.Handler { return a.router }

func (a *App) ModuleNames() []string {
	out := make([]string, len(a.modules))
	for i, m := range a.modules {
		out[i] = m.Name()
	}
	return out
}

// Start migrates every local module, connects the bus, then starts each
// module's outbox relay and workers.
func (a *App) Start(ctx context.Context) error {
	for _, m := range a.modules {
		if err := m.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate %s: %w", m.Name(), err)
		}
	}
	if err := a.bus.Start(ctx); err != nil {
		return err
	}
	for _, m := range a.modules {
		if err := m.Start(ctx); err != nil {
			return fmt.Errorf("start %s: %w", m.Name(), err)
		}
	}
	return nil
}

func (a *App) Stop(ctx context.Context) {
	for i := len(a.modules) - 1; i >= 0; i-- {
		if err := a.modules[i].Stop(ctx); err != nil {
			a.log.Warn("module stop", "module", a.modules[i].Name(), "err", err)
		}
	}
	a.closeResources()
}

func (a *App) closeResources() {
	if a.bus != nil {
		_ = a.bus.Close()
	}
	for _, db := range a.dbs {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
}
