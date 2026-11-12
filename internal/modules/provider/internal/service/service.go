package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/internal/gateway"
	"paygate/internal/modules/provider/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/events"
	"paygate/internal/platform/metrics"
	"paygate/internal/platform/money"
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
// checkout there. Real-money rules:
//
//   - The attempt is recorded BEFORE the provider is called, so a crash or a
//     timeout leaves a record that can be verified later.
//   - It fails over to the next provider ONLY when the previous one certainly
//     did not receive the request (providers.ErrProviderUnavailable). After a
//     timeout the request may have reached the provider (a USSD push may be on
//     the customer's phone): sending it elsewhere could charge twice, so the
//     result is reported as OutcomeUnknown and the status is checked later.
//   - Idempotent per tx_ref: a repeated call never sends a second request.
func (s *Service) InitiateCheckout(ctx context.Context, req providers.InitiateCheckout) (providers.CheckoutResult, error) {
	candidates, err := s.router.Candidates(s.gateways, s.mock, req)
	if err != nil {
		if prev, perr := s.previous(ctx, req.TxRef); perr == nil {
			return prev, nil
		}
		return providers.CheckoutResult{}, err
	}
	now := s.now()
	claimed, err := s.store.ClaimAttempt(s.store.DB(ctx), &store.Transaction{
		TxRef: req.TxRef, Provider: candidates[0].Name(), Method: req.Method, Amount: req.Amount, Currency: req.Currency,
		Status: store.StatusInitiating, ReturnURL: req.ReturnURL, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return providers.CheckoutResult{}, err
	}
	if !claimed {
		return s.previous(ctx, req.TxRef) // already sent (or being sent): never send twice
	}
	var lastErr error
	for i, cand := range candidates {
		if i > 0 {
			if err := s.store.SwitchAttempt(s.store.DB(ctx), req.TxRef, cand.Name(), s.now()); err != nil {
				return providers.CheckoutResult{}, err
			}
		}
		started := time.Now()
		res, err := cand.Initiate(ctx, req, s.callbackBase+"/providers/"+cand.Name()+"/callback")
		observe(cand.Name(), "initiate", started, err)
		if err == nil {
			s.router.Success(cand.Name())
			res.Provider = cand.Name()
			// Background context: the provider has the request now; recording
			// its answer must not be lost because the caller gave up.
			if err := s.store.CompleteAttempt(s.store.DB(context.WithoutCancel(ctx)), req.TxRef, res.ProviderRef,
				res.CheckoutURL, s.now()); err != nil {
				s.log.Error("save checkout", "tx_ref", req.TxRef, "err", err)
			}
			return res, nil
		}
		lastErr = err
		if !Retryable(err) {
			if NotSent(err) {
				_ = s.store.DropAttempt(s.store.DB(ctx), req.TxRef) // a business refusal: nothing was started
				return providers.CheckoutResult{}, err
			}
			// Maybe received: keep the "initiating" record and let the status checks find out.
			s.router.Failure(cand.Name(), err)
			s.log.Warn("provider outcome unknown; not retrying elsewhere", "provider", cand.Name(),
				"tx_ref", req.TxRef, "err", err)
			return providers.CheckoutResult{Provider: cand.Name(), OutcomeUnknown: true}, nil
		}
		s.router.Failure(cand.Name(), err)
		s.log.Warn("provider unavailable, trying next", "provider", cand.Name(), "method", req.Method,
			"tx_ref", req.TxRef, "err", err)
	}
	_ = s.store.DropAttempt(s.store.DB(ctx), req.TxRef)
	return providers.CheckoutResult{}, providers.ErrProviderUnavailable.WithMessage(
		"every provider for " + req.Method + " is unavailable: " + lastErr.Error())
}

// previous returns the stored result of an earlier attempt for txRef.
func (s *Service) previous(ctx context.Context, txRef string) (providers.CheckoutResult, error) {
	t, err := s.store.GetTransaction(s.store.DB(ctx), txRef, false)
	if err != nil {
		return providers.CheckoutResult{}, err
	}
	return providers.CheckoutResult{CheckoutURL: t.CheckoutURL, ProviderRef: t.ProviderRef, Provider: t.Provider,
		OutcomeUnknown: t.Status == store.StatusInitiating}, nil
}

func (s *Service) VerifyTransaction(ctx context.Context, provider, txRef string) (providers.Transaction, error) {
	if provider == "" { // the caller does not know where it was routed: use our record
		t, err := s.store.GetTransaction(s.store.DB(ctx), txRef, false)
		if err != nil {
			return providers.Transaction{}, err
		}
		provider = t.Provider
	}
	g, err := s.gateway(provider)
	if err != nil {
		return providers.Transaction{}, err
	}
	started := time.Now()
	t, err := g.Verify(ctx, txRef)
	observe(g.Name(), "verify", started, err)
	return t, err
}

var (
	providerCalls    = metrics.Counter("paygate_provider_requests_total", "Calls to payment providers", "provider", "operation", "outcome")
	providerDuration = metrics.Histogram("paygate_provider_request_duration_seconds", "Provider call latency", metrics.DefBuckets, "provider", "operation")
)

// observe records a provider call: outcome ok, unavailable (down), unknown
// (no answer) or error (refused / business error).
func observe(provider, op string, started time.Time, err error) {
	outcome := "ok"
	switch {
	case err == nil:
	case Retryable(err):
		outcome = "unavailable"
	case NotSent(err):
		outcome = "error"
	default:
		outcome = "unknown"
	}
	providerCalls.Inc(provider, op, outcome)
	providerDuration.Since(started, provider, op)
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
	started := time.Now()
	res, err := g.Refund(ctx, req)
	observe(g.Name(), "refund", started, err)
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
			Amount: remote.Amount, Currency: remote.Currency, FailureReason: remote.FailureReason, OccurredAt: s.now(),
		})
	})
}

// HostedCard is implemented by card adapters whose payment page is opened by
// a script from the provider (e.g. Mastercard Hosted Checkout).
type HostedCard interface {
	ScriptURL() string
	Origin() string
}

type CardPage struct {
	ScriptURL, Origin, TxRef, Amount, Currency, CancelURL string
}

// HostedCardPage returns what our page needs to open the provider's card
// form, only for a pending checkout of that provider.
func (s *Service) HostedCardPage(ctx context.Context, provider, txRef string) (CardPage, error) {
	g, err := s.gateway(provider)
	if err != nil {
		return CardPage{}, err
	}
	hc, ok := g.(HostedCard)
	if !ok {
		return CardPage{}, providers.ErrTransactionNotFound
	}
	t, err := s.store.GetTransaction(s.store.DB(ctx), txRef, false)
	if err != nil {
		return CardPage{}, err
	}
	if t.Provider != provider || t.Status != providers.StatusPending {
		return CardPage{}, providers.ErrTransactionNotFound
	}
	return CardPage{ScriptURL: hc.ScriptURL(), Origin: hc.Origin(), TxRef: t.TxRef,
		Amount: money.FormatMajor(t.Amount), Currency: t.Currency,
		CancelURL: s.callbackBase + "/providers/" + provider + "/return/" + t.TxRef + "?cancelled=1"}, nil
}

// Return handles the customer coming back from the provider's page: the
// result is read from the provider (never from the URL) before redirecting.
func (s *Service) Return(ctx context.Context, provider, txRef string) (*store.Transaction, error) {
	t, err := s.store.GetTransaction(s.store.DB(ctx), txRef, false)
	if err != nil {
		return nil, err
	}
	if t.Provider != provider {
		return nil, providers.ErrTransactionNotFound
	}
	g, err := s.gateway(provider)
	if err != nil {
		return nil, err
	}
	if err := s.sync(ctx, g, txRef); err != nil {
		s.log.Warn("verify on return failed; the webhook or expiry check will settle it", "tx_ref", txRef, "err", err)
	}
	return s.store.GetTransaction(s.store.DB(ctx), txRef, false)
}

// ---- Bank transfers and instructions ----

// Instructor: adapters whose customers act on their own (bank transfer).
type Instructor interface {
	Instructions(txRef string) providers.Instructions
}

// Instructions answers providers.GetInstructions.
func (s *Service) Instructions(ctx context.Context, q providers.GetInstructions) (providers.Instructions, error) {
	t, err := s.store.GetTransaction(s.store.DB(ctx), q.TxRef, false)
	if err != nil {
		return providers.Instructions{}, err
	}
	if in, ok := s.gateways[t.Provider].(Instructor); ok {
		return in.Instructions(q.TxRef), nil
	}
	return providers.Instructions{}, nil
}

// ConfirmTransfer: finance saw the customer's transfer on the bank
// statement. The exact amount must match; the payment then succeeds through
// the normal verified path (sync → TransactionUpdated).
func (s *Service) ConfirmTransfer(ctx context.Context, txRef string, amount int64, bankRef, by string) (providers.Transaction, error) {
	if strings.TrimSpace(bankRef) == "" {
		return providers.Transaction{}, apperror.Invalid("bank_reference_required", "enter the bank's transaction reference from the statement")
	}
	t, err := s.store.GetTransaction(s.store.DB(ctx), txRef, false)
	if err != nil {
		return providers.Transaction{}, err
	}
	g, ok := s.gateways[t.Provider].(*gateway.BankTransfer)
	if !ok {
		return providers.Transaction{}, apperror.Unprocessable("not_a_bank_transfer", "this payment is not a bank transfer")
	}
	if _, err := s.store.ConfirmTransfer(s.store.DB(ctx), txRef, amount, t.Currency, strings.TrimSpace(bankRef), by, s.now()); err != nil {
		return providers.Transaction{}, err
	}
	s.log.Info("bank transfer confirmed", "tx_ref", txRef, "by", by, "bank_reference", bankRef)
	if err := s.sync(ctx, g, txRef); err != nil {
		return providers.Transaction{}, err
	}
	return g.Verify(ctx, txRef)
}

func (s *Service) PendingTransfers(ctx context.Context, reference string, limit int) ([]store.BankTransfer, error) {
	return s.store.PendingTransfers(s.store.DB(ctx), reference, limit)
}
