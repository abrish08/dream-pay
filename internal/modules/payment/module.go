// Package payment owns payments and their state machine.
package payment

import (
	"embed"
	"log/slog"
	"time"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	"paygate/internal/contracts/payments"
	"paygate/internal/modules/payment/internal/service"
	"paygate/internal/modules/payment/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "payment"

type Deps struct {
	DB              *gorm.DB
	Bus             bus.Bus
	Log             *slog.Logger
	AllowMockInLive bool
	PaymentTTL      time.Duration // default time a customer has to pay
	ExpiryEvery     time.Duration // how often overdue payments are checked
	PublicBaseURL   string        // hosted checkout links
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, payments.Permissions))
	svc := service.New(d.DB, base.Events, d.Bus, d.AllowMockInLive, d.PaymentTTL, d.PublicBaseURL, base.Log)
	svc.ExpiryEvery = d.ExpiryEvery
	base.AddWorker(svc.ExpiryWorker)
	return &Module{Base: base, svc: svc}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterOpen(r.Open)
}

// Handles:    payments.GetPayment, payments.ListPayments
// Asks:       merchants.GetMerchant, providers.InitiateCheckout, providers.VerifyTransaction
// Publishes:  payments.PaymentSucceeded, payments.PaymentFailed, payments.PaymentExpired
// Subscribes: providers.TransactionUpdated, refunds.RefundSucceeded
func (m *Module) RegisterHandlers(b bus.Bus) {
	transport.RegisterBus(b, m.svc)
	bus.On(b, "payment.on_provider_transaction_updated", m.svc.OnProviderTransactionUpdated)
	bus.On(b, "payment.on_refund_succeeded", m.svc.OnRefundSucceeded)
}
