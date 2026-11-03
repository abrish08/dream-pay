// Package ledger keeps the double-entry books. It only reacts to events.
package ledger

import (
	"embed"
	"log/slog"
	"time"

	"gorm.io/gorm"

	authc "paygate/internal/contracts/auth"
	ledgerc "paygate/internal/contracts/ledger"
	"paygate/internal/modules/ledger/internal/service"
	"paygate/internal/modules/ledger/internal/transport"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/metrics"
	"paygate/internal/platform/module"
)

//go:embed migrations/*.sql
var migrations embed.FS

const Name = "ledger"

type Deps struct {
	DB  *gorm.DB
	Bus bus.Bus
	Log *slog.Logger
	// ReleaseEvery: how often due reserves are released (default 1 minute).
	ReleaseEvery time.Duration
}

type Module struct {
	*module.Base
	svc *service.Service
}

var _ module.Module = (*Module)(nil)

func New(d Deps) *Module {
	base := module.NewBase(Name, d.DB, d.Bus, d.Log, module.MigrationsFS(migrations))
	base.AddStartTask("register permissions", authc.RegisterTask(d.Bus, Name, ledgerc.Permissions))
	// 1 = the books do not balance for that currency: page someone.
	metrics.SQLGauge("paygate_ledger_unbalanced", "1 if total debits differ from total credits", nil, d.DB,
		`SELECT currency, CASE WHEN SUM(debit) = SUM(credit) THEN 0 ELSE 1 END AS n FROM ledger_entries GROUP BY currency`, "currency")
	svc := service.New(d.DB, base.Log)
	svc.ReleaseEvery = d.ReleaseEvery
	base.AddWorker(svc.ReleaseWorker)
	return &Module{Base: base, svc: svc}
}

func (m *Module) RegisterRoutes(r module.Routers) {
	h := transport.NewHTTP(m.svc, m.Log)
	h.RegisterPublic(r.Public)
	h.RegisterAdmin(r.Admin)
}

// Handles: ledger.ReserveFunds.
// Subscribes: payments.PaymentSucceeded, refunds.RefundSucceeded, payouts.PayoutPaid, payouts.PayoutFailed
func (m *Module) RegisterHandlers(b bus.Bus) {
	bus.Handle(b, m.svc.ReserveFunds)
	bus.Handle(b, m.svc.GetPostings)
	bus.Handle(b, m.svc.GetBalance)
	bus.On(b, "ledger.on_dispute_opened", m.svc.OnDisputeOpened)
	bus.On(b, "ledger.on_dispute_closed", m.svc.OnDisputeClosed)
	bus.On(b, "ledger.on_payout_paid", m.svc.OnPayoutPaid)
	bus.On(b, "ledger.on_payout_failed", m.svc.OnPayoutFailed)
	bus.On(b, "ledger.on_payment_succeeded", m.svc.OnPaymentSucceeded)
	bus.On(b, "ledger.on_refund_succeeded", m.svc.OnRefundSucceeded)
}
