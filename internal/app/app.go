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

	"paygate/internal/app/apidocs"
	authc "paygate/internal/contracts/auth"
	"paygate/internal/modules/auth"
	"paygate/internal/modules/ledger"
	"paygate/internal/modules/merchant"
	"paygate/internal/modules/payment"
	"paygate/internal/modules/payout"
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
	"paygate/internal/platform/mail"
	"paygate/internal/platform/module"
	"paygate/internal/platform/secrets"
	"paygate/internal/platform/sms"
)

// Modules is the list of every module in the system.
var Modules = []string{
	merchant.Name, auth.Name, provider.Name, payment.Name,
	refund.Name, ledger.Name, webhook.Name, reconciliation.Name, payout.Name,
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
			BootstrapEmail: cfg.BootstrapAdminEmail, BootstrapPassword: cfg.BootstrapAdminPassword,
			BootstrapName: cfg.BootstrapAdminName,
			Identity:      a.identityProviders(), Mailer: a.mailer(), SMS: a.sms(), Box: a.box(), DashboardURL: cfg.DashboardURL,
			ResetTTL: cfg.PasswordResetTTL}), nil
	case provider.Name:
		return provider.New(provider.Deps{DB: db, Bus: b, Log: log, PublicBaseURL: cfg.PublicBaseURL,
			Routes: cfg.ProviderRoutes, AllowMockInLive: cfg.AllowMockInLive}), nil
	case payment.Name:
		return payment.New(payment.Deps{DB: db, Bus: b, Log: log, AllowMockInLive: cfg.AllowMockInLive,
			PaymentTTL: cfg.PaymentTTL, ExpiryEvery: cfg.PaymentExpiryEvery, PublicBaseURL: cfg.PublicBaseURL}), nil
	case refund.Name:
		return refund.New(refund.Deps{DB: db, Bus: b, Log: log}), nil
	case ledger.Name:
		return ledger.New(ledger.Deps{DB: db, Bus: b, Log: log}), nil
	case webhook.Name:
		return webhook.New(webhook.Deps{DB: db, Bus: b, Log: log, EncryptionKey: cfg.EncryptionKey, AllowInsecure: cfg.AllowInsecureWebhooks})
	case payout.Name:
		return payout.New(payout.Deps{DB: db, Bus: b, Log: log}), nil
	case reconciliation.Name:
		return reconciliation.New(reconciliation.Deps{DB: db, Bus: b, Log: log}), nil
	}
	return nil, fmt.Errorf("unknown module")
}

func (a *App) sms() sms.Sender {
	switch {
	case a.cfg.SMS != nil:
		return a.cfg.SMS
	case a.cfg.SendEtAPIKey != "":
		return sms.SendEt{BaseURL: a.cfg.SendEtBaseURL, APIKey: a.cfg.SendEtAPIKey, Sender: a.cfg.SendEtSender}
	}
	return nil // SMS codes disabled: email only
}

func (a *App) box() *secrets.Box {
	b, err := secrets.NewBox(a.cfg.EncryptionKey)
	if err != nil {
		a.log.Warn("no DATA_ENCRYPTION_KEY: authenticator (MFA) disabled", "err", err)
		return nil
	}
	return b
}

// identityProviders: Sign in with Google / Apple, enabled per configured client ids.
func (a *App) identityProviders() map[string]*authn.OIDC {
	out := map[string]*authn.OIDC{}
	if len(a.cfg.GoogleClientIDs) > 0 {
		out["google"] = authn.NewGoogle(a.cfg.GoogleClientIDs, a.cfg.GoogleJWKSURL)
	}
	if len(a.cfg.AppleClientIDs) > 0 {
		out["apple"] = authn.NewApple(a.cfg.AppleClientIDs, a.cfg.AppleJWKSURL)
	}
	return out
}

func (a *App) mailer() mail.Sender {
	switch {
	case a.cfg.Mailer != nil:
		return a.cfg.Mailer
	case a.cfg.SMTPHost != "":
		return mail.SMTP{Host: a.cfg.SMTPHost, Port: a.cfg.SMTPPort, Username: a.cfg.SMTPUsername,
			Password: a.cfg.SMTPPassword, From: a.cfg.MailFrom}
	}
	a.log.Warn("SMTP_HOST not set: emails (password reset) are only written to the log; development only")
	return mail.Log{Log: a.log}
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
	r.Use(httpx.Correlation, httpx.Recoverer(a.log), httpx.AccessLog(a.log), httpx.CORS(a.cfg.CORSOrigins),
		httpx.SecurityHeaders(a.cfg.HSTS))
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

	// Rate limits (per process): each API key / user gets RATE_LIMIT_RPS with
	// bursts of RATE_LIMIT_BURST; login, signup and refresh are limited per
	// client IP per minute (brute force); other open routes per IP.
	perCaller := httpx.RateLimit(httpx.NewLimiter(a.cfg.RateLimitRPS, a.cfg.RateLimitBurst), httpx.ByCaller(a.cfg.TrustProxy), a.log)
	perIP := httpx.RateLimit(httpx.NewLimiter(a.cfg.RateLimitRPS, a.cfg.RateLimitBurst), httpx.ByIP(a.cfg.TrustProxy), a.log)
	credentials := httpx.RateLimit(httpx.NewLimiter(a.cfg.AuthRateLimitMin/60, a.cfg.AuthRateLimitMin),
		httpx.ByIP(a.cfg.TrustProxy), a.log)
	openLimits := func(next http.Handler) http.Handler {
		strict, normal := credentials(next), perIP(next)
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch req.URL.Path {
			case "/auth/login", "/auth/signup", "/auth/refresh", "/auth/password/forgot", "/auth/password/reset",
				"/auth/verify", "/auth/verify/resend", "/auth/login/mfa",
				"/auth/oauth/google", "/auth/oauth/apple":
				strict.ServeHTTP(w, req)
			default:
				normal.ServeHTTP(w, req)
			}
		})
	}

	var routers module.Routers
	r.Route("/v1", func(v chi.Router) {
		v.Use(authMW.Middleware, perCaller, httpx.RequireMerchant(a.log))
		routers.Public = v
	})
	r.Route("/admin", func(ad chi.Router) {
		ad.Use(authMW.Middleware, perCaller, httpx.RequirePlatform(a.log))
		routers.Admin = ad
	})
	r.Group(func(g chi.Router) {
		g.Use(authMW.Middleware, perCaller)
		routers.Authenticated = g
	})
	r.Group(func(g chi.Router) {
		g.Use(openLimits)
		routers.Open = g
	})
	apidocs.Register(r)

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

// Routes exposes the route tree (used to check the API docs cover every route).
func (a *App) Routes() chi.Routes { return a.router }

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
