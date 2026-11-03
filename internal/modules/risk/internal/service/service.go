package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/risk"
	"paygate/internal/modules/risk/internal/domain"
	"paygate/internal/modules/risk/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/metrics"
)

var decisions = metrics.Counter("paygate_risk_decisions_total", "Risk decisions", "action")

type Service struct {
	db         *gorm.DB
	store      *store.Store
	thresholds domain.Thresholds
	log        *slog.Logger
	now        func() time.Time
}

func New(db *gorm.DB, t domain.Thresholds, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), thresholds: t, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// Assess answers risk.Assess: gather the signals, apply the rules, record
// the attempt (allowed or not) for later velocity checks and audit.
func (s *Service) Assess(ctx context.Context, q risk.Assess) (risk.Decision, error) {
	db, now := s.db.WithContext(ctx), s.now()
	email := domain.NormalizeOrEmpty(domain.KindEmail, q.CustomerEmail)
	phone := domain.NormalizeOrEmpty(domain.KindPhone, q.CustomerPhone)
	ip := domain.NormalizeOrEmpty(domain.KindIP, q.CustomerIP)

	sig := domain.Signals{Amount: q.Amount, RiskLevel: q.RiskLevel}
	var err error
	if sig.Blocklisted, err = s.store.Blocked(db, map[string]string{domain.KindEmail: email, domain.KindPhone: phone, domain.KindIP: ip}); err != nil {
		return risk.Decision{}, err
	}
	count := func(col, v string, since time.Time, failed bool) int {
		if err != nil {
			return 0
		}
		var n int
		n, err = s.store.Count(db, col, v, since, failed)
		return n
	}
	sig.CustomerLast10Min = max(count("email", email, now.Add(-10*time.Minute), false), count("phone", phone, now.Add(-10*time.Minute), false))
	sig.CustomerLastDay = max(count("email", email, now.Add(-24*time.Hour), false), count("phone", phone, now.Add(-24*time.Hour), false))
	sig.IPLast10Min = count("ip", ip, now.Add(-10*time.Minute), false)
	sig.CustomerDeclinesHour = max(count("email", email, now.Add(-time.Hour), true), count("phone", phone, now.Add(-time.Hour), true))
	if err != nil {
		return risk.Decision{}, err
	}
	if sig.MerchantAverage, sig.MerchantCount, err = s.store.MerchantAverage(db, q.MerchantID, q.Environment, q.Currency,
		now.AddDate(0, 0, -30)); err != nil {
		return risk.Decision{}, err
	}

	d := domain.Decide(sig, s.thresholds)
	reasons, _ := json.Marshal(d.Reasons)
	if d.Reasons == nil {
		reasons = []byte("[]")
	}
	if err := s.store.Record(db, &store.Attempt{PaymentID: q.PaymentID, MerchantID: q.MerchantID, Environment: q.Environment,
		Amount: q.Amount, Currency: q.Currency, Email: email, Phone: phone, IP: ip, Action: d.Action, Score: d.Score,
		Reasons: string(reasons), CreatedAt: now}); err != nil {
		return risk.Decision{}, err
	}
	decisions.Inc(d.Action)
	if d.Action == risk.ActionBlock {
		s.log.Warn("payment blocked by risk rules", "payment_id", q.PaymentID, "merchant_id", q.MerchantID,
			"score", d.Score, "reasons", d.Reasons)
	}
	return d, nil
}

// OnPaymentFailed counts declines per customer (card testing shows as many
// declines in a short time). Risk blocks and expiries are not declines.
func (s *Service) OnPaymentFailed(ctx context.Context, e bus.Envelope, evt payments.PaymentFailed) error {
	if strings.HasPrefix(evt.Reason, "risk_") || strings.HasPrefix(evt.Reason, "provider_initiation") || evt.Reason == "limit_exceeded" {
		return nil
	}
	return events.HandleOnce(ctx, s.db, "risk.on_payment_failed", e, func(tx *gorm.DB) error {
		return s.store.MarkFailed(tx, evt.PaymentID)
	})
}

// ---- Blocklist ----

func (s *Service) Block(ctx context.Context, kind, value, reason, by string) (*store.BlockEntry, error) {
	v, err := domain.Normalize(kind, value)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return nil, apperror.Invalid("reason_required", "say why (kept for audit)")
	}
	e := &store.BlockEntry{ID: uuid.NewString(), Kind: kind, Value: v, Reason: strings.TrimSpace(reason), CreatedBy: by, CreatedAt: s.now()}
	return e, s.store.AddBlock(s.db.WithContext(ctx), e)
}

func (s *Service) Unblock(ctx context.Context, id string) error {
	return s.store.DeleteBlock(s.db.WithContext(ctx), id)
}

func (s *Service) Blocks(ctx context.Context, kind string, limit int) ([]store.BlockEntry, error) {
	return s.store.ListBlocks(s.db.WithContext(ctx), kind, limit)
}

type DecisionView struct {
	store.Attempt
	Reasons []string `json:"reasons"`
}

func (s *Service) Decisions(ctx context.Context, merchantID, paymentID, action string, limit int) ([]DecisionView, error) {
	rows, err := s.store.Decisions(s.db.WithContext(ctx), merchantID, paymentID, action, limit)
	out := make([]DecisionView, len(rows))
	for i, r := range rows {
		out[i] = DecisionView{Attempt: r, Reasons: []string{}}
		_ = json.Unmarshal([]byte(r.Reasons), &out[i].Reasons)
	}
	return out, err
}
