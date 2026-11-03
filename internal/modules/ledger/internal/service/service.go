package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/disputes"
	"paygate/internal/contracts/ledger"
	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/payouts"
	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/ledger/internal/domain"
	"paygate/internal/modules/ledger/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/money"
)

type Service struct {
	db    *gorm.DB
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
	// ReleaseEvery: how often due reserves are released (default 1 minute).
	ReleaseEvery time.Duration
}

func New(db *gorm.DB, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), log: log, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) OnPaymentSucceeded(ctx context.Context, e bus.Envelope, evt payments.PaymentSucceeded) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_payment_succeeded", e, func(tx *gorm.DB) error {
		posted, err := s.store.Post(tx, domain.PaymentCaptured(evt), s.now())
		if err != nil || !posted || evt.Reserve <= 0 || evt.ReserveDays <= 0 {
			return err
		}
		reserve := min(evt.Reserve, max(evt.Amount-evt.Fee, 0))
		if reserve <= 0 {
			return nil
		}
		return s.store.AddHold(tx, &store.ReserveHold{PaymentID: evt.PaymentID, MerchantID: evt.MerchantID,
			Environment: evt.Environment, Currency: evt.Currency, Amount: reserve,
			ReleaseAt: evt.OccurredAt.AddDate(0, 0, evt.ReserveDays)})
	})
}

// ReleaseWorker makes reserves available once their days have passed.
func (s *Service) ReleaseWorker(ctx context.Context) {
	every := s.ReleaseEvery
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.ReleaseDue(ctx, 100); err != nil && ctx.Err() == nil {
			s.log.Error("release reserves", "err", err)
		}
	}
}

// ReleaseDue releases up to limit due reserves; returns how many.
func (s *Service) ReleaseDue(ctx context.Context, limit int) (int, error) {
	n := 0
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		holds, err := s.store.ClaimDueHolds(tx, s.now(), limit)
		if err != nil {
			return err
		}
		for _, h := range holds {
			if _, err := s.store.Post(tx, domain.ReserveReleased(h.PaymentID, h.MerchantID, h.Environment, h.Currency, h.Amount), s.now()); err != nil {
				return err
			}
			if err := s.store.MarkReleased(tx, h.PaymentID, s.now()); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func (s *Service) OnDisputeOpened(ctx context.Context, e bus.Envelope, evt disputes.DisputeOpened) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_dispute_opened", e, func(tx *gorm.DB) error {
		_, err := s.store.Post(tx, domain.DisputeOpened(evt), s.now())
		return err
	})
}

func (s *Service) OnDisputeClosed(ctx context.Context, e bus.Envelope, evt disputes.DisputeClosed) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_dispute_closed", e, func(tx *gorm.DB) error {
		_, err := s.store.Post(tx, domain.DisputeClosed(evt), s.now())
		return err
	})
}

// GetBalance answers ledger.GetBalance.
func (s *Service) GetBalance(ctx context.Context, q ledger.GetBalance) (ledger.Balance, error) {
	b, err := s.MerchantBalance(ctx, q.MerchantID, q.Environment, q.Currency)
	return ledger.Balance{Available: b.Available, Reserved: b.Reserved, DisputeHold: b.DisputeHold, PendingPayouts: b.PendingPayouts}, err
}

func (s *Service) OnRefundSucceeded(ctx context.Context, e bus.Envelope, evt refunds.RefundSucceeded) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_refund_succeeded", e, func(tx *gorm.DB) error {
		_, err := s.store.Post(tx, domain.RefundSucceeded(evt), s.now())
		return err
	})
}

type BalanceView struct {
	MerchantID     string `json:"merchant_id"`
	Environment    string `json:"environment"`
	Currency       string `json:"currency"`
	Available      int64  `json:"available"`       // can be paid out
	Reserved       int64  `json:"reserved"`        // rolling reserve, released on schedule
	DisputeHold    int64  `json:"dispute_hold"`    // held for open disputes
	PendingPayouts int64  `json:"pending_payouts"` // held for payouts not sent yet
}

// ReserveFunds holds funds for a payout. The per-account lock makes the
// check-then-post atomic: concurrent payouts can never overdraw a balance.
func (s *Service) ReserveFunds(ctx context.Context, q ledger.ReserveFunds) (ledger.Reservation, error) {
	if q.Amount <= 0 || !money.Supported(q.Currency) {
		return ledger.Reservation{}, apperror.Invalid("invalid_reservation", "amount and currency are required")
	}
	account := domain.MerchantAvailable(q.MerchantID, q.Environment)
	var out ledger.Reservation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.store.LockAccount(tx, account, q.Currency); err != nil {
			return err
		}
		posting := domain.PayoutReserved(q.PayoutID, q.MerchantID, q.Environment, q.Currency, q.Amount)
		done, err := s.store.Posted(tx, posting.Reference)
		if err != nil {
			return err
		}
		available, err := s.store.Balance(tx, account, q.Currency)
		if err != nil {
			return err
		}
		if done { // replay of a reservation that already succeeded
			out.AvailableAfter = available
			return nil
		}
		if available < q.Amount {
			return ledger.ErrInsufficientFunds.WithMessage(
				fmt.Sprintf("available balance is %d, payout is %d", available, q.Amount))
		}
		if _, err := s.store.Post(tx, posting, s.now()); err != nil {
			return err
		}
		out.AvailableAfter = available - q.Amount
		return nil
	})
	return out, err
}

func (s *Service) OnPayoutPaid(ctx context.Context, e bus.Envelope, evt payouts.PayoutPaid) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_payout_paid", e, func(tx *gorm.DB) error {
		_, err := s.store.Post(tx, domain.PayoutPaid(evt), s.now())
		return err
	})
}

func (s *Service) OnPayoutFailed(ctx context.Context, e bus.Envelope, evt payouts.PayoutFailed) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_payout_failed", e, func(tx *gorm.DB) error {
		_, err := s.store.Post(tx, domain.PayoutReleased(evt), s.now())
		return err
	})
}

func (s *Service) MerchantBalance(ctx context.Context, merchantID, env, currency string) (BalanceView, error) {
	currency = strings.ToUpper(currency)
	if !money.Supported(currency) {
		return BalanceView{}, apperror.Invalid("unsupported_currency", "currency must be ETB or USD")
	}
	q := s.db.WithContext(ctx)
	v, err := s.store.Balance(q, domain.MerchantAvailable(merchantID, env), currency)
	if err != nil {
		return BalanceView{}, err
	}
	held, err := s.store.Balance(q, domain.MerchantPayoutsPending(merchantID, env), currency)
	if err != nil {
		return BalanceView{}, err
	}
	reserved, err := s.store.Balance(q, domain.MerchantReserve(merchantID, env), currency)
	if err != nil {
		return BalanceView{}, err
	}
	disputed, err := s.store.Balance(q, domain.MerchantDisputeHold(merchantID, env), currency)
	return BalanceView{MerchantID: merchantID, Environment: env, Currency: currency, Available: v, Reserved: reserved,
		DisputeHold: disputed, PendingPayouts: held}, err
}

func (s *Service) TrialBalance(ctx context.Context) ([]store.TrialBalanceRow, error) {
	return s.store.TrialBalance(s.db.WithContext(ctx))
}

// GetPostings answers ledger.GetPostings (reconciliation's ledger check).
func (s *Service) GetPostings(ctx context.Context, q ledger.GetPostings) ([]ledger.PostingInfo, error) {
	if len(q.References) > 500 {
		return nil, apperror.Invalid("too_many_references", "at most 500 references per request")
	}
	rows, err := s.store.PostingTotals(s.db.WithContext(ctx), q.References)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.PostingInfo, len(rows))
	for i, r := range rows {
		out[i] = ledger.PostingInfo{Reference: r.Reference, Currency: r.Currency, Amount: r.Amount}
	}
	return out, nil
}
