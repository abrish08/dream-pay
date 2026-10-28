// Package module defines the common shape every module implements, plus a
// Base that handles the parts all modules share (migrations, outbox relay,
// background workers, graceful stop).
//
// A module receives only platform services (its database, the bus, a
// logger, config values). It never receives another module.
package module

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"paygate/internal/platform/bus"
	"paygate/internal/platform/database"
	"paygate/internal/platform/events"
)

// Routers are the HTTP entry points a module can attach to.
type Routers struct {
	Public        chi.Router // /v1: merchant callers (API key or merchant user token)
	Admin         chi.Router // /admin: platform staff only
	Authenticated chi.Router // root: any valid credentials (e.g. /auth/me)
	Open          chi.Router // no auth: login, provider callbacks, hosted pages
}

type Module interface {
	Name() string
	Migrate(ctx context.Context) error
	RegisterRoutes(r Routers)
	// RegisterHandlers attaches the module's request handlers (bus.Handle)
	// and event subscriptions (bus.On) to the central bus.
	RegisterHandlers(b bus.Bus)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type Base struct {
	name       string
	DB         *gorm.DB
	Bus        bus.Bus
	Log        *slog.Logger
	Events     events.Publisher
	migrations fs.FS
	models     []any

	startTasks []startTask
	workers    []func(ctx context.Context)
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

type startTask struct {
	name string
	fn   func(ctx context.Context) error
}

// NewBase: migrations may be nil when the module only uses Models.
func NewBase(name string, db *gorm.DB, b bus.Bus, log *slog.Logger, migrations fs.FS) *Base {
	return &Base{
		name:       name,
		DB:         db,
		Bus:        b,
		Log:        log.With("module", name),
		Events:     events.NewOutbox(),
		migrations: migrations,
	}
}

func (b *Base) Name() string { return b.name }

// Models registers GORM models that are auto-migrated on every start
// (additive only: new tables, columns and indexes; nothing is dropped).
func (b *Base) Models(models ...any) { b.models = append(b.models, models...) }

// Migrate applies the shared outbox tables, the module's SQL migrations and
// its models into the module's schema.
func (b *Base) Migrate(ctx context.Context) error {
	return database.Migrate(ctx, b.DB, b.name, b.models, events.Migrations(), b.migrations)
}

// AddStartTask registers work that must happen once at start but may depend on
// another service (e.g. registering permissions with auth). It is tried once
// during Start; if that fails it is retried in the background until it
// succeeds, so a peer that starts later does not block this service.
func (b *Base) AddStartTask(name string, fn func(ctx context.Context) error) {
	b.startTasks = append(b.startTasks, startTask{name, fn})
}

// Defaults: modules override what they need.
func (b *Base) RegisterRoutes(Routers)   {}
func (b *Base) RegisterHandlers(bus.Bus) {}

// AddWorker registers a background loop started with the module.
func (b *Base) AddWorker(fn func(ctx context.Context)) { b.workers = append(b.workers, fn) }

// Start runs the module's outbox relay and workers until Stop.
func (b *Base) Start(ctx context.Context) error {
	ctx, b.cancel = context.WithCancel(context.WithoutCancel(ctx))
	workers := b.workers
	for _, t := range b.startTasks {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := t.fn(tctx)
		cancel()
		if err == nil {
			continue
		}
		b.Log.Warn("start task failed, retrying in background", "task", t.name, "err", err)
		workers = append(workers, b.retry(t))
	}
	relay := events.NewRelay(b.DB, b.Bus, b.name, b.Log)
	for _, fn := range append([]func(context.Context){relay.Run}, workers...) {
		b.wg.Add(1)
		go func(fn func(context.Context)) {
			defer b.wg.Done()
			fn(ctx)
		}(fn)
	}
	return nil
}

func (b *Base) retry(t startTask) func(ctx context.Context) {
	return func(ctx context.Context) {
		for attempt := 1; ; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(bus.Backoff(attempt)):
			}
			tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := t.fn(tctx)
			cancel()
			if err == nil {
				b.Log.Info("start task succeeded", "task", t.name, "attempt", attempt+1)
				return
			}
			b.Log.Warn("start task failed", "task", t.name, "attempt", attempt+1, "err", err)
		}
	}
}

func (b *Base) Stop(ctx context.Context) error {
	if b.cancel == nil {
		return nil
	}
	b.cancel()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MigrationsFS returns the "migrations" folder of a module's embed.FS.
func MigrationsFS(f embed.FS) fs.FS {
	sub, err := fs.Sub(f, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}
