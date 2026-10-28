package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/internal/gateway"
	"paygate/internal/modules/provider/internal/store"
	"paygate/internal/platform/events"
)

type Service struct {
	gateways     map[string]gateway.Gateway
	router       *Router
	mock         *gateway.Mock
	store        *store.Store
	events       events.Publisher
	callbackBase string
	log          *slog.Logger
	now          func() time.Time
}

func New(st *store.Store, pub events.Publisher, mock *gateway.Mock, others []gateway.Gateway, router *Router,
	publicBaseURL string, log *slog.Logger) *Service {
	gws := map[string]gateway.Gateway{mock.Name(): mock}
	for _, g := range others {
		gws[g.Name()] = g
	}
	return &Service{gateways: gws, router: router, mock: mock, store: st, events: pub, callbackBase: publicBaseURL,
		log: log, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) gateway(name string) (gateway.Gateway, error) {
	g, ok := s.gateways[name]
	if !ok {
		return nil, providers.ErrUnknownProvider.WithMessage("unknown payment provider: " + name)
	}
	return g, nil
}

// InitiateCheckout routes the payment method to a provider and starts the
// checkout there, failing over to the next provider in the route when one is
// unavailable. Idempotent per tx_ref.
func (s *Service) InitiateCheckout(ctx context.Context, req providers.InitiateCheckout) (providers.CheckoutResult, error) {
	if t, err := s.store.GetTransaction(s.store.DB(ctx), req.TxRef, false); err == nil {
		return providers.CheckoutResult{CheckoutURL: t.CheckoutURL, ProviderRef: t.ProviderRef, Provider: t.Provider}, nil
	} else if !errors.Is(err, providers.ErrTransactionNotFound) {
		return providers.CheckoutResult{}, err
	}
	candidates, err := s.router.Candidates(s.gateways, s.mock, req)
	if err != nil {
		return providers.CheckoutResult{}, err
	}
	var (
		g       gateway.Gateway
		res     providers.CheckoutResult
		lastErr error
	)
	for _, cand := range candidates {
		res, lastErr = cand.Initiate(ctx, req, s.callbackBase+"/providers/"+cand.Name()+"/callback")
		if lastErr == nil {
			s.router.Success(cand.Name())
			g = cand
			break
		}
		if !Retryable(lastErr) {
			return providers.CheckoutResult{}, lastErr
		}
		s.router.Failure(cand.Name(), lastErr)
		s.log.Warn("provider unavailable, trying next", "provider", cand.Name(), "method", req.Method,
			"tx_ref", req.TxRef, "err", lastErr)
	}
	if g == nil {
		return providers.CheckoutResult{}, providers.ErrProviderUnavailable.WithMessage(
			"every provider for " + req.Method + " is unavailable: " + lastErr.Error())
	}
	res.Provider = g.Name()
	now := s.now()
	err = s.store.CreateTransaction(s.store.DB(ctx), &store.Transaction{
		TxRef: req.TxRef, Provider: g.Name(), Method: req.Method, ProviderRef: res.ProviderRef, Amount: req.Amount,
		Currency: req.Currency, Status: providers.StatusPending, CheckoutURL: res.CheckoutURL,
		ReturnURL: req.ReturnURL, CreatedAt: now, UpdatedAt: now,
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return res, nil
	}
	return res, err
}

func (s *Service) VerifyTransaction(ctx context.Context, provider, txRef string) (providers.Transaction, error) {
	g, err := s.gateway(provider)
	if err != nil {
		return providers.Transaction{}, err
	}
	return g.Verify(ctx, txRef)
}

// Refund is idempotent per refund_ref: a retried call returns the stored result.
func (s *Service) Refund(ctx context.Context, req providers.Refund) (providers.RefundResult, error) {
	g, err := s.gateway(req.Provider)
	if err != nil {
		return providers.RefundResult{}, err
	}
	if prev, err := s.store.GetRefund(s.store.DB(ctx), req.RefundRef); err != nil {
		return providers.RefundResult{}, err
	} else if prev != nil {
		return providers.RefundResult{Status: prev.Status, ProviderRefundRef: prev.ProviderRefundRef, FailureReason: prev.FailureReason}, nil
	}
	res, err := g.Refund(ctx, req)
	if err != nil {
		return providers.RefundResult{}, err
	}
	err = s.store.SaveRefund(s.store.DB(ctx), &store.Refund{
		RefundRef: req.RefundRef, Provider: g.Name(), TxRef: req.TxRef, Amount: req.Amount, Currency: req.Currency,
		Status: res.Status, ProviderRefundRef: res.ProviderRefundRef, FailureReason: res.FailureReason, CreatedAt: s.now(),
	})
	return res, err
}

// ListMethods: methods and whether they can be used right now.
func (s *Service) ListMethods(_ context.Context, q providers.ListMethods) ([]providers.MethodInfo, error) {
	if q.Currency == "" {
		q.Currency = "ETB"
	}
	out := make([]providers.MethodInfo, 0, len(providers.Methods))
	for _, m := range providers.Methods {
		names, ok := s.router.Available(s.gateways, s.mock, q.Environment, m, q.Currency)
		out = append(out, providers.MethodInfo{Method: m, Label: providers.MethodLabels[m], Available: ok, Providers: names})
	}
	return out, nil
}

// Health: every provider adapter, its methods and circuit-breaker state.
func (s *Service) Health() []ProviderHealth { return s.router.Health(s.gateways) }

// HandleCallback processes an inbound provider notification.
func (s *Service) HandleCallback(ctx context.Context, provider string, r *http.Request, body []byte) (string, error) {
	g, err := s.gateway(provider)
	if err != nil {
		return "", err
	}
	txRef, err := g.ParseCallback(r, body)
	if err != nil {
		return "", err
	}
	return txRef, s.sync(ctx, g, txRef)
}

// CompleteMockPayment is used by the mock provider's hosted checkout page.
func (s *Service) CompleteMockPayment(ctx context.Context, txRef string, succeeded bool) (*store.Transaction, error) {
	if err := s.mock.Complete(ctx, txRef, succeeded); err != nil {
		return nil, err
	}
	if err := s.sync(ctx, s.mock, txRef); err != nil {
		return nil, err
	}
	return s.store.GetTransaction(s.store.DB(ctx), txRef, false)
}

func (s *Service) GetTransaction(ctx context.Context, txRef string) (*store.Transaction, error) {
	return s.store.GetTransaction(s.store.DB(ctx), txRef, false)
}

// sync asks the provider for the authoritative status and, if it changed to a
// final state, records it and publishes TransactionUpdated in one transaction.
func (s *Service) sync(ctx context.Context, g gateway.Gateway, txRef string) error {
	remote, err := g.Verify(ctx, txRef)
	if err != nil {
		return err
	}
	if remote.Status == providers.StatusPending {
		return nil
	}
	return s.store.DB(ctx).Transaction(func(tx *gorm.DB) error {
		local, err := s.store.GetTransaction(tx, txRef, true)
		if err != nil {
			return err
		}
		if local.Status == remote.Status {
			return nil // duplicate notification
		}
		if err := s.store.UpdateTransactionStatus(tx, txRef, remote.Status, s.now()); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, providers.TransactionUpdated{
			Provider: g.Name(), TxRef: txRef, ProviderRef: remote.ProviderRef, Status: remote.Status,
			Amount: remote.Amount, Currency: remote.Currency, OccurredAt: s.now(),
		})
	})
}
