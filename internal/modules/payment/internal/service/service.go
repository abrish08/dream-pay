package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/contracts/disputes"
	"paygate/internal/contracts/merchants"
	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/contracts/refunds"
	"paygate/internal/contracts/risk"
	"paygate/internal/modules/payment/internal/domain"
	"paygate/internal/modules/payment/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/metrics"
	"paygate/internal/platform/money"
)

type Service struct {
	db         *gorm.DB
	store      *store.Store
	events     events.Publisher
	bus        bus.Bus // the only way to reach other modules
	allowMock  bool
	publicBase string        // for hosted checkout URLs
	ttl        time.Duration // default time a customer has to pay
	// ExpiryEvery: how often the expiry worker runs (default 15s).
	ExpiryEvery time.Duration
	// CheckEvery: how often due status checks are run (default 5s).
	CheckEvery time.Duration
	// CheckSchedule: when open payments are checked with the provider, as
	// delays from the previous check (the first from when the checkout opened).
	CheckSchedule []time.Duration
	log           *slog.Logger
	now           func() time.Time
}

var (
	paymentsCreated = metrics.Counter("paygate_payments_created_total", "Payments created", "environment", "method", "currency")
	paymentsFinal   = metrics.Counter("paygate_payments_final_total", "Payments reaching a final status", "environment", "status", "provider", "reason")
	riskOutcomes    = metrics.Counter("paygate_payment_risk_total", "Risk and limit outcomes at creation", "outcome")
)

func New(db *gorm.DB, pub events.Publisher, b bus.Bus, allowMockInLive bool, ttl time.Duration, publicBase string, log *slog.Logger) *Service {
	metrics.Gauge("paygate_payments_open", "Open payments by status; stuck = processing for over 15 minutes", nil,
		func(ctx context.Context) ([]metrics.Sample, error) {
			var rows []struct {
				Environment string
				Status      string
				N           float64
				Stuck       float64
			}
			err := db.WithContext(ctx).Raw(`SELECT environment, status, COUNT(*) AS n,
				COUNT(*) FILTER (WHERE status = 'processing' AND created_at < now() - interval '15 minutes') AS stuck
				FROM payments WHERE status IN ('pending', 'processing') GROUP BY environment, status`).Scan(&rows).Error
			var out []metrics.Sample
			for _, r := range rows {
				out = append(out, metrics.Sample{Labels: map[string]string{"environment": r.Environment, "status": r.Status}, Value: r.N})
				if r.Status == "processing" {
					out = append(out, metrics.Sample{Labels: map[string]string{"environment": r.Environment, "status": "stuck"}, Value: r.Stuck})
				}
			}
			return out, err
		})
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	return &Service{db: db, store: store.New(db), events: pub, bus: b, allowMock: allowMockInLive, ttl: ttl,
		publicBase:    strings.TrimRight(publicBase, "/"),
		CheckSchedule: DefaultCheckSchedule,
		log:           log, now: func() time.Time { return time.Now().UTC() }}
}

// DefaultCheckSchedule: a payment still open is checked 1, 3, 8, 18, 38 and
// 68 minutes after its checkout opened (then the expiry check takes over), so
// a lost provider callback costs the customer minutes, not a day.
var DefaultCheckSchedule = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
	20 * time.Minute, 30 * time.Minute}

func (s *Service) firstCheck() time.Duration {
	if len(s.CheckSchedule) == 0 {
		return 0
	}
	return s.CheckSchedule[0]
}

type CreateInput struct {
	MerchantID     string            `json:"-"`
	Environment    string            `json:"-"`
	IdempotencyKey string            `json:"-"`
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Method         string            `json:"payment_method"`
	Provider       string            `json:"provider"`
	Reference      string            `json:"reference"`
	Description    string            `json:"description"`
	Metadata       map[string]string `json:"metadata"`
	CustomerEmail  string            `json:"customer_email"`
	CustomerName   string            `json:"customer_name"`
	CustomerPhone  string            `json:"customer_phone"`
	CustomerIP     string            `json:"customer_ip"`
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
	case in.Environment == "live" && !merchants.KYCPassed(m.KYCStatus):
		return payments.Payment{}, false, merchants.ErrKYCRequired
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
		RequestHash: hash, Amount: in.Amount, Currency: in.Currency, Method: in.Method, Provider: in.Provider,
		Reference: in.Reference, Description: in.Description, Metadata: in.Metadata,
		CustomerEmail: in.CustomerEmail, CustomerName: in.CustomerName, CustomerPhone: in.CustomerPhone,
		CustomerIP: in.CustomerIP, ReturnURL: in.ReturnURL, TTL: ttl,
		ReserveBPS: m.Settings.ReserveBPS, ReserveDays: m.Settings.ReserveDays,
	}, m.FeeBPS, s.now())
	if err != nil {
		return payments.Payment{}, false, err
	}
	hosted := p.PaymentMethod == "" && p.Provider == ""
	if hosted {
		// No method chosen: the customer picks one on our hosted page.
		p.CheckoutURL = s.publicBase + "/pay/" + p.ID
	}
	if err := s.insertWithinLimits(ctx, p, m.Settings.Limits[p.Currency]); errors.Is(err, store.ErrDuplicate) {
		// A concurrent request with the same key won the race.
		prev, err := s.replay(ctx, in.MerchantID, in.IdempotencyKey, hash)
		return toAPI(prev), false, err
	} else if err != nil {
		return payments.Payment{}, false, err
	}
	paymentsCreated.Inc(p.Environment, p.PaymentMethod, p.Currency)
	if err := s.assess(ctx, p, m.Settings.RiskLevel); err != nil {
		cur, _ := s.store.Get(s.db.WithContext(ctx), p.ID, false)
		return toAPI(cur), true, err
	}
	if hosted {
		return toAPI(p), true, nil
	}
	p, err = s.start(ctx, p, domain.SourceMerchantAPI)
	return toAPI(p), true, err
}

// insertWithinLimits stores the payment if the merchant's hard limits allow
// it. Checked and inserted under a per-merchant lock, so concurrent payments
// cannot together exceed a daily limit. Fails closed: if the check cannot
// run, the payment is not created.
func (s *Service) insertWithinLimits(ctx context.Context, p *domain.Payment, l merchants.Limit) error {
	by := domain.Actor{Source: domain.SourceMerchantAPI}
	if l.MaxAmount > 0 && p.Amount > l.MaxAmount {
		riskOutcomes.Inc("limit_exceeded")
		return limitError("max_amount", l.MaxAmount, p.Currency)
	}
	if l.DailyAmount == 0 && l.DailyCount == 0 {
		return s.store.Insert(s.db.WithContext(ctx), p, by)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "payment-limits:"+p.MerchantID+":"+p.Environment+":"+p.Currency).Error; err != nil {
			return err
		}
		total, count, err := s.store.Volume(tx, p.MerchantID, p.Environment, p.Currency, s.now().Add(-24*time.Hour))
		if err != nil {
			return err
		}
		if l.DailyAmount > 0 && total+p.Amount > l.DailyAmount {
			riskOutcomes.Inc("limit_exceeded")
			return limitError("daily_amount", l.DailyAmount, p.Currency)
		}
		if l.DailyCount > 0 && count+1 > int64(l.DailyCount) {
			riskOutcomes.Inc("limit_exceeded")
			return apperror.Unprocessable("limit_exceeded", fmt.Sprintf("daily_count limit of %d payments reached", l.DailyCount))
		}
		return s.store.Insert(tx, p, by)
	})
}

func limitError(which string, limit int64, currency string) error {
	return apperror.Unprocessable("limit_exceeded",
		fmt.Sprintf("%s limit of %s %s exceeded; ask us to raise it", which, currency, money.FormatMajor(limit)))
}

// assess asks the risk module. A block fails the payment (recorded in its
// history). If risk cannot answer, the payment continues and the outage is
// logged: the hard limits above still apply (fail open for scoring only).
func (s *Service) assess(ctx context.Context, p *domain.Payment, level string) error {
	actx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	d, err := bus.Ask(actx, s.bus, risk.Assess{PaymentID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
		Amount: p.Amount, Currency: p.Currency, CustomerEmail: p.CustomerEmail, CustomerPhone: p.CustomerPhone,
		CustomerIP: p.CustomerIP, RiskLevel: level})
	if err != nil {
		riskOutcomes.Inc("risk_unavailable")
		s.log.Error("ALERT: risk checks unavailable; payment allowed without scoring", "payment_id", p.ID, "err", err)
		return nil
	}
	riskOutcomes.Inc(d.Action)
	if d.Action != risk.ActionBlock {
		return nil
	}
	reason := "risk_declined: " + strings.Join(d.Reasons, ",")
	if err := s.fail(ctx, p.ID, reason, domain.Actor{Source: domain.SourceRisk,
		Details: map[string]any{"score": d.Score, "reasons": d.Reasons}}); err != nil {
		return err
	}
	return apperror.Unprocessable("risk_declined", "this payment was declined by our risk checks")
}

// start routes the payment's method to a provider and opens the checkout
// there. The external call happens outside any DB transaction.
//
// Real-money rule: a payment is failed here ONLY when the provider side
// certainly did not start anything (a refusal, or every provider down before
// accepting). If the answer is unknown (timeout, lost connection), the
// customer may already be paying: the payment stays open and the status
// checks ask the provider until the outcome is known.
func (s *Service) start(ctx context.Context, p *domain.Payment, source string) (*domain.Payment, error) {
	res, err := bus.Ask(ctx, s.bus, providers.InitiateCheckout{
		Method: p.PaymentMethod, Provider: p.Provider, Environment: p.Environment, TxRef: p.ID,
		Amount: p.Amount, Currency: p.Currency, CustomerEmail: p.CustomerEmail, CustomerName: p.CustomerName,
		ReturnURL: p.ReturnURL,
	})
	if err != nil {
		if notStarted(err) {
			reason := "provider_initiation_failed: " + apperror.From(err).Message
			if ferr := s.fail(ctx, p.ID, reason, domain.Actor{Source: domain.SourceProvider}); ferr != nil {
				s.log.Error("mark payment failed", "payment_id", p.ID, "err", ferr)
			}
			return nil, err
		}
		s.log.Warn("checkout outcome unknown; payment kept open and will be checked", "payment_id", p.ID, "err", err)
		res = providers.CheckoutResult{Provider: p.Provider, OutcomeUnknown: true}
	}
	by := domain.Actor{Source: source, Provider: res.Provider, Reason: "checkout opened"}
	if res.OutcomeUnknown {
		by.Reason = "provider_outcome_unknown: checking with the provider"
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		cur, err := s.store.Get(tx, p.ID, true)
		if err != nil {
			return err
		}
		if cur.Status == domain.Pending {
			first := s.firstCheck()
			if res.OutcomeUnknown && first > 0 {
				first = min(first, 30*time.Second) // unknown: look sooner
			}
			if err := cur.StartProcessing(res.Provider, res.ProviderRef, res.CheckoutURL, first, s.now()); err != nil {
				return err
			}
			if err := s.store.Save(tx, cur, by); err != nil {
				return err
			}
		}
		p = cur
		return nil
	})
	return p, err
}

// notStarted: the error proves nothing was started at any provider, so the
// payment can safely be failed.
func notStarted(err error) bool {
	ae := apperror.From(err)
	switch ae.Kind {
	case apperror.KindInvalid, apperror.KindUnprocessable, apperror.KindForbidden, apperror.KindConflict, apperror.KindNotFound:
		return true
	}
	return ae.Code == providers.ErrProviderUnavailable.Code // every provider refused before accepting it
}

// ChooseMethod: the customer picked a method on the hosted page. Returns the
// provider's checkout URL to redirect to. A second submit (double click,
// another tab) gets the same checkout instead of a second charge.
func (s *Service) ChooseMethod(ctx context.Context, id, method string) (string, error) {
	var p *domain.Payment
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if p, err = s.store.Get(tx, id, true); err != nil {
			return err
		}
		if p.Status == domain.Processing && p.CheckoutURL != "" && p.Provider != "" {
			return nil // already started
		}
		if err := p.ChooseMethod(method, s.now()); err != nil {
			return err
		}
		p.Note("method chosen: "+method, s.now())
		return s.store.Save(tx, p, domain.Actor{Source: domain.SourceCustomer})
	})
	if err != nil {
		return "", err
	}
	if p.Status == domain.Processing {
		return p.CheckoutURL, nil
	}
	if p, err = s.start(ctx, p, domain.SourceCustomer); err != nil {
		return "", err
	}
	return p.CheckoutURL, nil // empty when the outcome is being checked: the page shows the status
}

// CheckoutView is what the hosted page shows.
type CheckoutView struct {
	ID          string
	Merchant    string
	Amount      int64
	Currency    string
	Description string
	Reference   string
	Status      string
	Open        bool
	ExpiresAt   time.Time
	ReturnURL   string
	Methods     []providers.MethodInfo
}

func (s *Service) Checkout(ctx context.Context, id string) (CheckoutView, error) {
	p, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil {
		return CheckoutView{}, err
	}
	v := CheckoutView{ID: p.ID, Amount: p.Amount, Currency: p.Currency, Description: p.Description,
		Reference: p.Reference, Status: string(p.Status), ExpiresAt: p.ExpiresAt, ReturnURL: p.ReturnURL,
		Open: p.Status == domain.Pending && s.now().Before(p.ExpiresAt)}
	if m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: p.MerchantID}); err == nil {
		v.Merchant = m.Name
	}
	if v.Open {
		ms, err := bus.Ask(ctx, s.bus, providers.ListMethods{Environment: p.Environment, Currency: p.Currency})
		if err != nil {
			return CheckoutView{}, err
		}
		for _, m := range ms {
			if m.Available {
				v.Methods = append(v.Methods, m)
			}
		}
	}
	return v, nil
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
	if in.Environment == "test" && in.Provider != "" && in.Provider != "mock" {
		return apperror.Invalid("provider_not_allowed", "test API keys can only use the mock provider")
	}
	if in.Environment == "live" && in.Provider == "mock" && !s.allowMock {
		return apperror.Invalid("provider_not_allowed", "the mock provider is only available with test API keys")
	}
	return nil
}

func (s *Service) fail(ctx context.Context, id, reason string, by domain.Actor) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p, err := s.store.Get(tx, id, true)
		if err != nil {
			return err
		}
		return s.failInTx(ctx, tx, p, reason, s.now(), by)
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
		return s.applyProviderResult(ctx, tx, p, providers.Transaction{Provider: evt.Provider, Status: evt.Status,
			Amount: evt.Amount, Currency: evt.Currency, FailureReason: evt.FailureReason}, domain.SourceProvider)
	})
}

// applyProviderResult records the provider's verified answer on a locked
// payment, with who learned it (source) in the history.
func (s *Service) applyProviderResult(ctx context.Context, tx *gorm.DB, p *domain.Payment, r providers.Transaction, source string) error {
	if p.Settled() || (p.Status == domain.Failed && r.Status != providers.StatusSucceeded) {
		return nil
	}
	now := s.now()
	by := domain.Actor{Source: source, Provider: r.Provider, Details: map[string]any{
		"provider_status": r.Status, "amount": r.Amount, "currency": r.Currency}}
	if r.FailureReason != "" {
		by.Details["failure_reason"] = r.FailureReason
	}
	switch r.Status {
	case providers.StatusSucceeded:
		if r.Amount != p.Amount || r.Currency != p.Currency {
			s.log.Error("provider amount mismatch", "payment_id", p.ID,
				"expected", p.Amount, "got", r.Amount, "currency", r.Currency)
			if p.Status == domain.Failed {
				return nil
			}
			return s.failInTx(ctx, tx, p, "amount_mismatch", now, by)
		}
		if p.Status == domain.Expired || p.Status == domain.Failed {
			s.log.Warn("payment completed after "+string(p.Status)+"; honouring the provider's capture", "payment_id", p.ID)
			by.Reason = "paid after " + string(p.Status) + ": the provider confirmed the capture"
			p.FailureReason = ""
		}
		// If the payment is still pending (checkout response not yet saved)
		// this returns an error and the event is retried shortly.
		if err := p.TransitionTo(domain.Succeeded, now); err != nil {
			return err
		}
		if err := s.store.Save(tx, p, by); err != nil {
			return err
		}
		if p.LinkID != "" {
			if err := s.store.LinkPaid(tx, p.LinkID, now); err != nil {
				return err
			}
		}
		paymentsFinal.Inc(p.Environment, string(domain.Succeeded), p.Provider, "")
		return s.events.Publish(ctx, tx, payments.PaymentSucceeded{
			PaymentID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment, Amount: p.Amount,
			Fee: p.Fee, Reserve: p.Reserve, ReserveDays: p.ReserveDays, Currency: p.Currency, Provider: p.Provider, ProviderRef: p.ProviderRef,
			Reference: p.Reference, Metadata: p.Metadata, OccurredAt: now,
		})
	case providers.StatusFailed:
		reason := r.FailureReason
		if reason == "" {
			reason = "declined_by_provider"
		}
		return s.failInTx(ctx, tx, p, reason, now, by)
	}
	return nil
}

// StatusCheckWorker verifies open payments with the provider on the check
// schedule. It finds payments whose callback was lost or delayed (common
// with mobile money and USSD push) within minutes. Safe with many replicas.
func (s *Service) StatusCheckWorker(ctx context.Context) {
	every := s.CheckEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.CheckDue(ctx, 50); err != nil && ctx.Err() == nil {
			s.log.Error("payment status checks", "err", err)
		}
	}
}

// CheckDue runs up to limit due status checks; returns how many resolved.
func (s *Service) CheckDue(ctx context.Context, limit int) (int, error) {
	due, err := s.store.ClaimDueChecks(s.db.WithContext(ctx), s.now(), 2*time.Minute, limit)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, d := range due {
		remote, verr := bus.Ask(ctx, s.bus, providers.VerifyTransaction{Provider: d.Provider, TxRef: d.ID})
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			cur, err := s.store.Get(tx, d.ID, true)
			if err != nil || cur.Status != domain.Processing {
				return err
			}
			if verr == nil && (remote.Status == providers.StatusSucceeded || remote.Status == providers.StatusFailed) {
				resolved++
				return s.applyProviderResult(ctx, tx, cur, remote, domain.SourceStatusCheck)
			}
			if verr != nil {
				s.log.Warn("status check failed, will retry", "payment_id", d.ID, "err", verr)
			}
			cur.ScheduleCheck(s.CheckSchedule, s.now())
			return s.store.Save(tx, cur, domain.Actor{})
		})
		if err != nil {
			s.log.Error("status check", "payment_id", d.ID, "err", err)
		}
	}
	return resolved, nil
}

// Sync asks the provider for the payment's status now and applies it
// (payments.SyncPayment; used by reconciliation to repair a difference).
func (s *Service) Sync(ctx context.Context, q payments.SyncPayment) (payments.Payment, error) {
	p, err := s.store.Get(s.db.WithContext(ctx), q.PaymentID, false)
	if err != nil {
		return payments.Payment{}, err
	}
	if p.Status == domain.Pending {
		return toAPI(p), nil // never sent to a provider
	}
	remote, err := bus.Ask(ctx, s.bus, providers.VerifyTransaction{Provider: p.Provider, TxRef: p.ID})
	if err != nil {
		return payments.Payment{}, err
	}
	source := q.Source
	if source == "" {
		source = domain.SourceSystem
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		cur, err := s.store.Get(tx, p.ID, true)
		if err != nil {
			return err
		}
		if q.Note != "" {
			cur.Note(q.Note, s.now())
		}
		if err := s.applyProviderResult(ctx, tx, cur, remote, source); err != nil {
			return err
		}
		if err := s.store.Save(tx, cur, domain.Actor{Source: source}); err != nil { // the note, if nothing changed
			return err
		}
		p = cur
		return nil
	})
	return toAPI(p), err
}

// History is a payment's history, for its merchant (or any, when
// merchantID is empty: platform staff).
func (s *Service) History(ctx context.Context, merchantID, id string) ([]store.Event, error) {
	p, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil {
		return nil, err
	}
	if merchantID != "" && p.MerchantID != merchantID {
		return nil, payments.ErrNotFound
	}
	return s.store.History(s.db.WithContext(ctx), id)
}

// GetAny returns any merchant's payment (platform staff).
func (s *Service) GetAny(ctx context.Context, id string) (payments.Payment, error) {
	p, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil {
		return payments.Payment{}, err
	}
	return toAPI(p), nil
}

// GetByIDs answers payments.GetPaymentsByID.
func (s *Service) GetByIDs(ctx context.Context, q payments.GetPaymentsByID) ([]payments.Payment, error) {
	if len(q.IDs) > 500 {
		return nil, apperror.Invalid("too_many_ids", "at most 500 ids per request")
	}
	ps, err := s.store.GetByIDs(s.db.WithContext(ctx), q.IDs)
	if err != nil {
		return nil, err
	}
	return toAPIList(ps), nil
}

// ExpiryWorker closes payments the customer did not complete in time. Before
// expiring one it asks the provider, so a payment that did go through is
// recorded as succeeded instead. Safe with many replicas: each payment is
// re-checked under a row lock and only a real status change publishes.
func (s *Service) ExpiryWorker(ctx context.Context) {
	every := s.ExpiryEvery
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
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
		if errors.Is(verr, providers.ErrTransactionNotFound) {
			verr = nil // the provider never received it: nothing can complete later
			remote = providers.Transaction{Status: providers.StatusPending}
		}
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
				return s.applyProviderResult(ctx, tx, cur, remote, domain.SourceStatusCheck)
			}
			if err := cur.Expire(s.now()); err != nil {
				return err
			}
			paymentsFinal.Inc(cur.Environment, string(domain.Expired), cur.Provider, "")
			if err := s.store.Save(tx, cur, domain.Actor{Source: domain.SourceExpiry,
				Details: map[string]any{"provider_status": remote.Status}}); err != nil {
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

func (s *Service) failInTx(ctx context.Context, tx *gorm.DB, p *domain.Payment, reason string, now time.Time, by domain.Actor) error {
	if err := p.Fail(reason, now); err != nil {
		return err
	}
	short, _, _ := strings.Cut(reason, ":") // e.g. "risk_declined", "insufficient_funds"
	paymentsFinal.Inc(p.Environment, string(domain.Failed), p.Provider, short)
	if err := s.store.Save(tx, p, by); err != nil {
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
		return s.store.Save(tx, p, domain.Actor{Source: domain.SourceRefund, Reason: "refund " + evt.RefundID,
			Details: map[string]any{"refund_id": evt.RefundID, "amount": evt.Amount, "currency": evt.Currency}})
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
		RefundedAmount: p.RefundedAmount, Currency: p.Currency, Status: string(p.Status),
		PaymentMethod: p.PaymentMethod, Provider: p.Provider,
		ProviderRef: p.ProviderRef, Reference: p.Reference, Description: p.Description, Metadata: meta,
		CustomerEmail: p.CustomerEmail, CustomerName: p.CustomerName, CustomerPhone: p.CustomerPhone, ReturnURL: p.ReturnURL,
		Reserve: p.Reserve, ReserveDays: p.ReserveDays,
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

// OnDisputeOpened / OnDisputeClosed note the chargeback in the payment's
// history (the payment's status does not change: the money moves in the
// ledger).
func (s *Service) OnDisputeOpened(ctx context.Context, e bus.Envelope, evt disputes.DisputeOpened) error {
	return s.disputeNote(ctx, e, "payment.on_dispute_opened", evt.PaymentID,
		fmt.Sprintf("dispute %s opened: %s, %s held", evt.DisputeID, evt.Reason, money.FormatMajor(evt.Amount)+" "+evt.Currency))
}

func (s *Service) OnDisputeClosed(ctx context.Context, e bus.Envelope, evt disputes.DisputeClosed) error {
	return s.disputeNote(ctx, e, "payment.on_dispute_closed", evt.PaymentID, "dispute "+evt.DisputeID+" "+evt.Outcome)
}

func (s *Service) disputeNote(ctx context.Context, e bus.Envelope, consumer, paymentID, note string) error {
	return events.HandleOnce(ctx, s.db, consumer, e, func(tx *gorm.DB) error {
		p, err := s.store.Get(tx, paymentID, true)
		if errors.Is(err, payments.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		p.Note(note, s.now())
		return s.store.Save(tx, p, domain.Actor{Source: domain.SourceDispute})
	})
}

// ---- Payment links ----

type LinkInput struct {
	Amount      int64      `json:"amount"`
	Currency    string     `json:"currency"`
	Description string     `json:"description"`
	Reference   string     `json:"reference"`  // prefix of each payment's reference
	SingleUse   bool       `json:"single_use"` // an invoice: closes once paid
	ExpiresAt   *time.Time `json:"expires_at"`
	Environment string     `json:"environment"` // dashboard users: live (default) or test; API keys: their own
}

type LinkView struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"` // share this
	Environment string     `json:"environment"`
	Amount      int64      `json:"amount"`
	Currency    string     `json:"currency"`
	Description string     `json:"description,omitempty"`
	Reference   string     `json:"reference,omitempty"`
	SingleUse   bool       `json:"single_use"`
	Active      bool       `json:"active"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	PaidCount   int        `json:"paid_count"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (s *Service) linkView(l *domain.PaymentLink) LinkView {
	return LinkView{ID: l.ID, URL: s.publicBase + "/l/" + l.ID, Environment: l.Environment, Amount: l.Amount,
		Currency: l.Currency, Description: l.Description, Reference: l.Reference, SingleUse: l.SingleUse,
		Active: l.Active && l.Usable(s.now()) == nil, ExpiresAt: l.ExpiresAt, PaidCount: l.PaidCount, CreatedAt: l.CreatedAt}
}

func (s *Service) CreateLink(ctx context.Context, merchantID, env string, in LinkInput) (LinkView, error) {
	if env == "" {
		env = in.Environment
	}
	if env == "" {
		env = "live"
	}
	m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: merchantID})
	if err != nil {
		return LinkView{}, err
	}
	if env == "live" && !merchants.KYCPassed(m.KYCStatus) {
		return LinkView{}, merchants.ErrKYCRequired
	}
	l, err := domain.NewLink(domain.NewLinkInput{MerchantID: merchantID, Environment: env, Amount: in.Amount, Currency: in.Currency,
		Description: in.Description, Reference: in.Reference, SingleUse: in.SingleUse, ExpiresAt: in.ExpiresAt}, s.now())
	if err != nil {
		return LinkView{}, err
	}
	return s.linkView(l), s.store.InsertLink(s.db.WithContext(ctx), l)
}

func (s *Service) ListLinks(ctx context.Context, merchantID, env string, limit int) ([]LinkView, error) {
	ls, err := s.store.ListLinks(s.db.WithContext(ctx), merchantID, env, limit)
	out := make([]LinkView, len(ls))
	for i := range ls {
		out[i] = s.linkView(&ls[i])
	}
	return out, err
}

func (s *Service) GetLink(ctx context.Context, merchantID, id string) (LinkView, error) {
	l, err := s.store.GetLink(s.db.WithContext(ctx), id, false)
	if err != nil {
		return LinkView{}, err
	}
	if l.MerchantID != merchantID {
		return LinkView{}, store.ErrLinkNotFound
	}
	return s.linkView(l), nil
}

func (s *Service) DeactivateLink(ctx context.Context, merchantID, id string) (LinkView, error) {
	var l *domain.PaymentLink
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		if l, err = s.store.GetLink(tx, id, true); err != nil {
			return err
		}
		if l.MerchantID != merchantID {
			return store.ErrLinkNotFound
		}
		l.Active, l.UpdatedAt = false, s.now()
		return s.store.SaveLink(tx, l)
	})
	if err != nil {
		return LinkView{}, err
	}
	return s.linkView(l), nil
}

// OpenLink: a customer visits the link. A single-use link reuses its open
// payment (two visitors cannot pay it twice: visits are serialised by a
// lock on the link); a reusable link opens a new payment per visit.
// Returns the hosted checkout URL.
func (s *Service) OpenLink(ctx context.Context, id, customerIP string) (string, error) {
	var checkout string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		l, err := s.store.GetLink(tx, id, true)
		if err != nil {
			return err
		}
		if err := l.Usable(s.now()); err != nil {
			return err
		}
		if l.SingleUse {
			if open, err := s.store.OpenPaymentForLink(tx, l.ID, s.now()); err != nil || open != nil {
				if open != nil {
					checkout = s.publicBase + "/pay/" + open.ID
				}
				return err
			}
		}
		ref := l.Reference
		if ref == "" {
			ref = "LINK"
		}
		visit := uuid.NewString()
		p, _, err := s.Create(ctx, CreateInput{MerchantID: l.MerchantID, Environment: l.Environment,
			IdempotencyKey: "link:" + l.ID + ":" + visit, Amount: l.Amount, Currency: l.Currency,
			Reference: ref + "-" + visit[:8], Description: l.Description, CustomerIP: customerIP,
			Metadata: map[string]string{"payment_link": l.ID}})
		if err != nil {
			return err
		}
		checkout = p.CheckoutURL
		return s.store.SetLinkID(tx, p.ID, l.ID)
	})
	return checkout, err
}
