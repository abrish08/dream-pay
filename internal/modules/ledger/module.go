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
	// Merchants who owe the platform (a refund or a lost dispute larger than
	// their balance): finance follows up.
	metrics.SQLGauge("paygate_ledger_negative_merchant_balances", "Merchants whose available balance is below zero", nil, d.DB,
		`SELECT currency, COUNT(*) AS n FROM (SELECT account, currency FROM ledger_entries
		   WHERE account LIKE 'merchant:%:available:live' GROUP BY account, currency HAVING SUM(credit - debit) < 0) x
		 GROUP BY currency`, "currency")
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

// Handles: ledger.ReserveFunds, ledger.HoldRefund, ledger.GetPostings, ledger.GetBalance.
// Subscribes: payments.PaymentSucceeded, refunds.RefundSucceeded/Failed, payouts.PayoutPaid/Failed,
// disputes.DisputeOpened/Closed, reconciliation.SettlementReconciled.
func (m *Module) RegisterHandlers(b bus.Bus) {
	bus.Handle(b, m.svc.ReserveFunds)
	bus.Handle(b, m.svc.HoldRefund)
	bus.Handle(b, m.svc.ReleaseRefund)
	bus.On(b, "ledger.on_refund_failed", m.svc.OnRefundFailed)
	bus.On(b, "ledger.on_settlement_reconciled", m.svc.OnSettlementReconciled)
	bus.Handle(b, m.svc.GetPostings)
	bus.Handle(b, m.svc.GetBalance)
	bus.On(b, "ledger.on_dispute_opened", m.svc.OnDisputeOpened)
	bus.On(b, "ledger.on_dispute_closed", m.svc.OnDisputeClosed)
	bus.On(b, "ledger.on_payout_paid", m.svc.OnPayoutPaid)
	bus.On(b, "ledger.on_payout_failed", m.svc.OnPayoutFailed)
	bus.On(b, "ledger.on_payment_succeeded", m.svc.OnPaymentSucceeded)
	bus.On(b, "ledger.on_refund_succeeded", m.svc.OnRefundSucceeded)
}
