package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/merchants"
	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/contracts/refunds"
	"paygate/internal/modules/payment/internal/domain"
	"paygate/internal/modules/payment/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
)

type Service struct {
	db        *gorm.DB
	store     *store.Store
	events    events.Publisher
	bus       bus.Bus // the only way to reach other modules
	allowMock bool
	ttl       time.Duration // default time a customer has to pay
	log       *slog.Logger
	now       func() time.Time
}

func New(db *gorm.DB, pub events.Publisher, b bus.Bus, allowMockInLive bool, ttl time.Duration, log *slog.Logger) *Service {
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	return &Service{db: db, store: store.New(db), events: pub, bus: b, allowMock: allowMockInLive, ttl: ttl,
		log: log, now: func() time.Time { return time.Now().UTC() }}
}

type CreateInput struct {
	MerchantID     string `json:"-"`
	Environment    string `json:"-"`
	IdempotencyKey string `json:"-"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	Provider       string `json:"provider"`
	Reference      string            `json:"reference"`
	Description    string            `json:"description"`
	Metadata       map[string]string `json:"metadata"`
	CustomerEmail  string            `json:"customer_email"`
	CustomerName   string            `json:"customer_name"`
	ReturnURL      string            `json:"return_url"`
	ExpiresInMin   int               `json:"expires_in_minutes"`
}

// Create registers a payment and starts checkout with the provider.
// The bool is false when an earlier result is replayed for the same Idempotency-Key.
func (s *Service) Create(ctx context.Context, in CreateInput) (payments.Payment, bool, error) {
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 100 {
		return payments.Payment{}, false, apperror.Invalid("idempotency_key_required",
			"send a unique Idempotency-Key header (max 100 characters)")
	}
	hash := requestHash(in)
	if prev, err := s.replay(ctx, in.MerchantID, in.IdempotencyKey, hash); prev != nil || err != nil {
		return toAPI(prev), false, err
	}

	if err := s.checkEnvironment(in); err != nil {
		return payments.Payment{}, false, err
	}
	m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: in.MerchantID})
	if err != nil {
		return payments.Payment{}, false, err
	}
	switch {
	case m.Status == merchants.StatusActive:
	case m.Status == merchants.StatusPending && in.Environment == "test":
		// Not approved yet: test mode works, live waits for approval.
	case m.Status == merchants.StatusPending:
		return payments.Payment{}, false, apperror.Forbidden("merchant_not_approved",
			"your account is waiting for approval; use a test key until then")
	default:
		return payments.Payment{}, false, apperror.Forbidden("merchant_inactive", "merchant account is not active")
	}
	ttl := s.ttl
	if in.ExpiresInMin != 0 {
		ttl = time.Duration(in.ExpiresInMin) * time.Minute
	}

	p, err := domain.NewPayment(domain.NewPaymentInput{
		MerchantID: in.MerchantID, Environment: in.Environment, IdempotencyKey: in.IdempotencyKey,
		RequestHash: hash, Amount: in.Amount, Currency: in.Currency, Provider: in.Provider,
		Reference: in.Reference, Description: in.Description, Metadata: in.Metadata,
		CustomerEmail: in.CustomerEmail, CustomerName: in.CustomerName, ReturnURL: in.ReturnURL, TTL: ttl,
	}, m.FeeBPS, s.now())
	if err != nil {
		return payments.Payment{}, false, err
	}
	if err := s.store.Insert(s.db.WithContext(ctx), p); errors.Is(err, store.ErrDuplicate) {
		// A concurrent request with the same key won the race.
		prev, err := s.replay(ctx, in.MerchantID, in.IdempotencyKey, hash)
		return toAPI(prev), false, err
	} else if err != nil {
		return payments.Payment{}, false, err
	}

	// External call happens outside any DB transaction.
	res, err := bus.Ask(ctx, s.bus, providers.InitiateCheckout{
		Provider: p.Provider, TxRef: p.ID, Amount: p.Amount, Currency: p.Currency,
		CustomerEmail: p.CustomerEmail, CustomerName: p.CustomerName, ReturnURL: p.ReturnURL,
	})
	if err != nil {
		reason := "provider_initiation_failed: " + apperror.From(err).Message
		if ferr := s.fail(ctx, p.ID, reason); ferr != nil {
			s.log.Error("mark payment failed", "payment_id", p.ID, "err", ferr)
		}
		return payments.Payment{}, false, err
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		cur, err := s.store.Get(tx, p.ID, true)
		if err != nil {
			return err
		}
		if cur.Status == domain.Pending {
			if err := cur.StartProcessing(res.ProviderRef, res.CheckoutURL, s.now()); err != nil {
				return err
			}
			if err := s.store.Save(tx, cur); err != nil {
				return err
			}
		}
		p = cur
		return nil
	})
	return toAPI(p), true, err
}

func (s *Service) replay(ctx context.Context, merchantID, key, hash string) (*domain.Payment, error) {
	prev, err := s.store.FindByIdempotencyKey(s.db.WithContext(ctx), merchantID, key)
	if err != nil || prev == nil {
		return nil, err
	}
	if prev.RequestHash != hash {
		return nil, payments.ErrIdempotencyConflict
	}
	return prev, nil
}

// Test keys may only use the mock provider; live keys may not (unless allowed).
func (s *Service) checkEnvironment(in CreateInput) error {
	if in.Environment != "live" && in.Environment != "test" {
		// Dashboard users have no environment; payments come from merchant servers.
		return apperror.Forbidden("api_key_required", "create payments with a live or test API key")
	}
	if in.Environment == "test" && in.Provider != "mock" {
		return apperror.Invalid("provider_not_allowed", "test API keys can only use the mock provider")
	}
	if in.Environment == "live" && in.Provider == "mock" && !s.allowMock {
		return apperror.Invalid("provider_not_allowed", "the mock provider is only available with test API keys")
	}
	return nil
}

func (s *Service) fail(ctx context.Context, id, reason string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p, err := s.store.Get(tx, id, true)
		if err != nil {
			return err
		}
		if err := p.Fail(reason, s.now()); err != nil {
			return err
		}
		if err := s.store.Save(tx, p); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, failedEvent(p))
	})
}

// OnProviderTransactionUpdated consumes provider.transaction.updated.
func (s *Service) OnProviderTransactionUpdated(ctx context.Context, e bus.Envelope, evt providers.TransactionUpdated) error {
	return events.HandleOnce(ctx, s.db, "payment.on_provider_transaction_updated", e, func(tx *gorm.DB) error {
		p, err := s.store.Get(tx, evt.TxRef, true)
		if errors.Is(err, payments.ErrNotFound) {
			s.log.Warn("provider update for unknown payment", "tx_ref", evt.TxRef)
			return nil
		}
		if err != nil {
			return err
		}
		return s.applyProviderResult(ctx, tx, p, evt.Status, evt.Amount, evt.Currency)
	})
}

// applyProviderResult records the provider's verified answer on a locked
// payment. Used by provider events and by the expiry worker.
func (s *Service) applyProviderResult(ctx context.Context, tx *gorm.DB, p *domain.Payment, status string, amount int64, currency string) error {
	if p.Captured() {
		return nil
	}
	now := s.now()
	switch status {
	case providers.StatusSucceeded:
		if amount != p.Amount || currency != p.Currency {
			s.log.Error("provider amount mismatch", "payment_id", p.ID,
				"expected", p.Amount, "got", amount, "currency", currency)
			return s.failInTx(ctx, tx, p, "amount_mismatch", now)
		}
		if p.Status == domain.Expired {
			s.log.Warn("payment completed after expiry; honouring it", "payment_id", p.ID)
		}
		// If the payment is still pending (checkout response not yet saved)
		// this returns an error and the event is retried shortly.
		if err := p.TransitionTo(domain.Succeeded, now); err != nil {
			return err
		}
		if err := s.store.Save(tx, p); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, payments.PaymentSucceeded{
			PaymentID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment, Amount: p.Amount,
			Fee: p.Fee, Currency: p.Currency, Provider: p.Provider, ProviderRef: p.ProviderRef,
			Reference: p.Reference, Metadata: p.Metadata, OccurredAt: now,
		})
	case providers.StatusFailed:
		return s.failInTx(ctx, tx, p, "declined_by_provider", now)
	}
	return nil
}

// ExpiryWorker closes payments the customer did not complete in time. Before
// expiring one it asks the provider, so a payment that did go through is
// recorded as succeeded instead. Safe with many replicas: each payment is
// re-checked under a row lock and only a real status change publishes.
func (s *Service) ExpiryWorker(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.ExpireDue(ctx, 100); err != nil {
			s.log.Error("expire payments", "err", err)
		}
	}
}

// ExpireDue handles up to limit overdue payments and returns how many changed.
func (s *Service) ExpireDue(ctx context.Context, limit int) (int, error) {
	due, err := s.store.DueForExpiry(s.db.WithContext(ctx), s.now(), limit)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, p := range due {
		remote, verr := bus.Ask(ctx, s.bus, providers.VerifyTransaction{Provider: p.Provider, TxRef: p.ID})
		if verr != nil && p.Status == domain.Processing && s.now().Sub(p.ExpiresAt) < 24*time.Hour {
			// Provider unreachable: try again next round rather than guess.
			s.log.Warn("verify before expiry failed, will retry", "payment_id", p.ID, "err", verr)
			continue
		}
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			cur, err := s.store.Get(tx, p.ID, true)
			if err != nil || cur.Captured() || cur.Status == domain.Expired {
				return err
			}
			if verr == nil && (remote.Status == providers.StatusSucceeded || remote.Status == providers.StatusFailed) {
				changed++
				return s.applyProviderResult(ctx, tx, cur, remote.Status, remote.Amount, remote.Currency)
			}
			if err := cur.Expire(s.now()); err != nil {
				return err
			}
			if err := s.store.Save(tx, cur); err != nil {
				return err
			}
			changed++
			return s.events.Publish(ctx, tx, payments.PaymentExpired{PaymentID: cur.ID, MerchantID: cur.MerchantID,
				Environment: cur.Environment, Amount: cur.Amount, Currency: cur.Currency, Reference: cur.Reference,
				Metadata: cur.Metadata, OccurredAt: cur.UpdatedAt})
		})
		if err != nil {
			s.log.Error("expire payment", "payment_id", p.ID, "err", err)
		}
	}
	return changed, nil
}

func (s *Service) failInTx(ctx context.Context, tx *gorm.DB, p *domain.Payment, reason string, now time.Time) error {
	if err := p.Fail(reason, now); err != nil {
		return err
	}
	if err := s.store.Save(tx, p); err != nil {
		return err
	}
	return s.events.Publish(ctx, tx, failedEvent(p))
}

// OnRefundSucceeded consumes refund.succeeded and updates the refunded amount.
func (s *Service) OnRefundSucceeded(ctx context.Context, e bus.Envelope, evt refunds.RefundSucceeded) error {
	return events.HandleOnce(ctx, s.db, "payment.on_refund_succeeded", e, func(tx *gorm.DB) error {
		p, err := s.store.Get(tx, evt.PaymentID, true)
		if err != nil {
			return err
		}
		if err := p.ApplyRefund(evt.Amount, s.now()); err != nil {
			return err
		}
		return s.store.Save(tx, p)
	})
}

func (s *Service) Get(ctx context.Context, merchantID, id string) (payments.Payment, error) {
	p, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil {
		return payments.Payment{}, err
	}
	if p.MerchantID != merchantID {
		return payments.Payment{}, payments.ErrNotFound // never reveal other merchants' data
	}
	return toAPI(p), nil
}

func (s *Service) List(ctx context.Context, f payments.ListPayments) ([]payments.Payment, error) {
	ps, err := s.store.List(s.db.WithContext(ctx), f)
	if err != nil {
		return nil, err
	}
	return toAPIList(ps), nil
}

// ListFilter selects a merchant's payments.
type ListFilter = store.MerchantFilter

// Page is one page of a list, newest first. Pass NextCursor as
// starting_after to get the next page.
type Page struct {
	Data       []payments.Payment `json:"data"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func (s *Service) ListForMerchant(ctx context.Context, f store.MerchantFilter) (Page, error) {
	ps, more, err := s.store.ListForMerchant(s.db.WithContext(ctx), f)
	if err != nil {
		return Page{}, err
	}
	page := Page{Data: toAPIList(ps), HasMore: more}
	if more && len(ps) > 0 {
		page.NextCursor = ps[len(ps)-1].ID
	}
	return page, nil
}

func requestHash(in CreateInput) string {
	b, _ := json.Marshal(in) // only the json-tagged body fields are included
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func failedEvent(p *domain.Payment) payments.PaymentFailed {
	return payments.PaymentFailed{PaymentID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
		Amount: p.Amount, Currency: p.Currency, Provider: p.Provider, Reference: p.Reference,
		Reason: p.FailureReason, Metadata: p.Metadata, OccurredAt: p.UpdatedAt}
}

func toAPI(p *domain.Payment) payments.Payment {
	if p == nil {
		return payments.Payment{}
	}
	meta := map[string]string(p.Metadata)
	if meta == nil {
		meta = map[string]string{}
	}
	return payments.Payment{
		ID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment, Amount: p.Amount, Fee: p.Fee,
		RefundedAmount: p.RefundedAmount, Currency: p.Currency, Status: string(p.Status), Provider: p.Provider,
		ProviderRef: p.ProviderRef, Reference: p.Reference, Description: p.Description, Metadata: meta,
		CustomerEmail: p.CustomerEmail, CustomerName: p.CustomerName, ReturnURL: p.ReturnURL,
		CheckoutURL: p.CheckoutURL, FailureReason: p.FailureReason, ExpiresAt: p.ExpiresAt,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

func toAPIList(ps []domain.Payment) []payments.Payment {
	out := make([]payments.Payment, len(ps))
	for i := range ps {
		out[i] = toAPI(&ps[i])
	}
	return out
}
