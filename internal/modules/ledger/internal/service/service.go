package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/payments"
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
	MerchantID  string `json:"merchant_id"`
	Environment string `json:"environment"`
	Currency    string `json:"currency"`
	Available   int64  `json:"available"`
}

func (s *Service) MerchantBalance(ctx context.Context, merchantID, env, currency string) (BalanceView, error) {
	currency = strings.ToUpper(currency)
	if !money.Supported(currency) {
		return BalanceView{}, apperror.Invalid("unsupported_currency", "currency must be ETB or USD")
	}
	v, err := s.store.Balance(s.db.WithContext(ctx), domain.MerchantAvailable(merchantID, env), currency)
	return BalanceView{MerchantID: merchantID, Environment: env, Currency: currency, Available: v}, err
}

func (s *Service) TrialBalance(ctx context.Context) ([]store.TrialBalanceRow, error) {
	return s.store.TrialBalance(s.db.WithContext(ctx))
}
