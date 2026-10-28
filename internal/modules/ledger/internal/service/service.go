package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

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
}

func New(db *gorm.DB, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), log: log, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) OnPaymentSucceeded(ctx context.Context, e bus.Envelope, evt payments.PaymentSucceeded) error {
	return events.HandleOnce(ctx, s.db, "ledger.on_payment_succeeded", e, func(tx *gorm.DB) error {
		_, err := s.store.Post(tx, domain.PaymentCaptured(evt), s.now())
		return err
	})
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
	return BalanceView{MerchantID: merchantID, Environment: env, Currency: currency, Available: v, PendingPayouts: held}, err
}

func (s *Service) TrialBalance(ctx context.Context) ([]store.TrialBalanceRow, error) {
	return s.store.TrialBalance(s.db.WithContext(ctx))
}
