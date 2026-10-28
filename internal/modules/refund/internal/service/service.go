package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/refund/internal/domain"
	"paygate/internal/modules/refund/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
)

type View struct {
	ID                string    `json:"id"`
	PaymentID         string    `json:"payment_id"`
	Amount            int64     `json:"amount"`
	Currency          string    `json:"currency"`
	Status            string    `json:"status"`
	Reason            string    `json:"reason,omitempty"`
	ProviderRefundRef string    `json:"provider_refund_ref,omitempty"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Service struct {
	db     *gorm.DB
	store  *store.Store
	events events.Publisher
	bus    bus.Bus // the only way to reach other modules
	log    *slog.Logger
	now    func() time.Time
}

func New(db *gorm.DB, pub events.Publisher, b bus.Bus, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), events: pub, bus: b,
		log: log, now: func() time.Time { return time.Now().UTC() }}
}

type CreateInput struct {
	MerchantID     string
	IdempotencyKey string
	PaymentID      string
	Amount         int64 // 0 = refund everything still refundable
	Reason         string
}

func (s *Service) Create(ctx context.Context, in CreateInput) (View, bool, error) {
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 100 {
		return View{}, false, apperror.Invalid("idempotency_key_required", "send a unique Idempotency-Key header")
	}
	hash := hashOf(in)
	if prev, err := s.replay(s.db.WithContext(ctx), in, hash); prev != nil || err != nil {
		return view(prev), false, err
	}

	p, err := bus.Ask(ctx, s.bus, payments.GetPayment{MerchantID: in.MerchantID, PaymentID: in.PaymentID})
	if err != nil {
		return View{}, false, err
	}
	if p.Status != payments.StatusSucceeded && p.Status != payments.StatusPartiallyRefunded {
		return View{}, false, refunds.ErrNotRefundable
	}

	var r *domain.Refund
	replayed := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.store.LockPayment(tx, p.ID); err != nil {
			return err
		}
		if prev, err := s.replay(tx, in, hash); prev != nil || err != nil {
			r, replayed = prev, true
			return err
		}
		committed, err := s.store.CommittedAmount(tx, p.ID)
		if err != nil {
			return err
		}
		available := domain.AvailableToRefund(p.Amount, committed)
		snap := domain.PaymentSnapshot{ID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
			Provider: p.Provider, Currency: p.Currency, Status: p.Status, Amount: p.Amount}
		r, err = domain.NewRefund(snap, in.Amount, in.Reason, in.IdempotencyKey, hash, available, s.now())
		if err != nil {
			if errors.Is(err, refunds.ErrExceedsRefundable) {
				return refunds.ErrExceedsRefundable.WithMessage(
					fmt.Sprintf("refund amount exceeds the refundable amount (%d available)", available))
			}
			return err
		}
		return s.store.Insert(tx, r)
	})
	if err != nil || replayed {
		return view(r), false, err
	}

	if err := s.execute(ctx, r.ID); err != nil {
		// Provider unreachable: the refund stays pending and the retry worker
		// finishes it. Never mark it failed on a timeout, the provider may
		// have processed it.
		s.log.Warn("refund left pending, will retry", "refund_id", r.ID, "err", err)
	}
	cur, err := s.store.Get(s.db.WithContext(ctx), r.ID, false)
	return view(cur), true, err
}

// execute sends a pending refund to the provider and records the outcome.
func (s *Service) execute(ctx context.Context, id string) error {
	r, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil || r.Final() {
		return err
	}
	res, err := bus.Ask(ctx, s.bus, providers.Refund{
		Provider: r.Provider, TxRef: r.PaymentID, RefundRef: r.ID,
		Amount: r.Amount, Currency: r.Currency, Reason: r.Reason,
	})
	if err != nil {
		var ae *apperror.Error
		if errors.As(err, &ae) && ae.Kind == apperror.KindUnavailable {
			return err // transient: stay pending
		}
		res = providers.RefundResult{Status: providers.StatusFailed, FailureReason: apperror.From(err).Message}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, err := s.store.Get(tx, id, true)
		if err != nil || r.Final() {
			return err
		}
		now := s.now()
		if res.Status == providers.StatusSucceeded {
			r.Succeed(res.ProviderRefundRef, now)
			if err := s.store.Save(tx, r); err != nil {
				return err
			}
			return s.events.Publish(ctx, tx, refunds.RefundSucceeded{
				RefundID: r.ID, PaymentID: r.PaymentID, MerchantID: r.MerchantID, Environment: r.Environment,
				Amount: r.Amount, Currency: r.Currency, Provider: r.Provider, OccurredAt: now,
			})
		}
		r.Fail(res.FailureReason, now)
		if err := s.store.Save(tx, r); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, refunds.RefundFailed{
			RefundID: r.ID, PaymentID: r.PaymentID, MerchantID: r.MerchantID, Environment: r.Environment,
			Amount: r.Amount, Currency: r.Currency, Reason: r.FailureReason, OccurredAt: now,
		})
	})
}

// RetryPending is a background worker that finishes refunds left pending.
func (s *Service) RetryPending(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		stale, err := s.store.StalePending(s.db.WithContext(ctx), s.now().Add(-30*time.Second), 20)
		if err != nil {
			s.log.Error("load pending refunds", "err", err)
			continue
		}
		for _, r := range stale {
			if err := s.execute(ctx, r.ID); err != nil {
				s.log.Warn("refund retry failed", "refund_id", r.ID, "err", err)
			}
		}
	}
}

func (s *Service) Get(ctx context.Context, merchantID, id string) (View, error) {
	r, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil {
		return View{}, err
	}
	if r.MerchantID != merchantID {
		return View{}, refunds.ErrNotFound
	}
	return view(r), nil
}

func (s *Service) replay(q *gorm.DB, in CreateInput, hash string) (*domain.Refund, error) {
	prev, err := s.store.FindByIdempotencyKey(q, in.MerchantID, in.IdempotencyKey)
	if err != nil || prev == nil {
		return nil, err
	}
	if prev.RequestHash != hash {
		return nil, apperror.Conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request body")
	}
	return prev, nil
}

func hashOf(in CreateInput) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%d|%s", in.PaymentID, in.Amount, in.Reason))
	return hex.EncodeToString(h[:])
}

func view(r *domain.Refund) View {
	if r == nil {
		return View{}
	}
	return View{ID: r.ID, PaymentID: r.PaymentID, Amount: r.Amount, Currency: r.Currency, Status: r.Status,
		Reason: r.Reason, ProviderRefundRef: r.ProviderRefundRef, FailureReason: r.FailureReason,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
