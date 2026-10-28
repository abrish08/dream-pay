// Package service: merchant payouts (settlement).
//
// Money safety: the ledger holds the funds (ledger.ReserveFunds) before a
// payout is "requested", atomically per merchant, so concurrent requests can
// never pay out more than the balance. Marking a payout paid or failed
// publishes an event; the ledger then books the transfer or releases the hold.
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

	"paygate/internal/contracts/ledger"
	"paygate/internal/contracts/merchants"
	"paygate/internal/contracts/payouts"
	"paygate/internal/modules/payout/internal/domain"
	"paygate/internal/modules/payout/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/reqctx"
)

type Service struct {
	db     *gorm.DB
	store  *store.Store
	events events.Publisher
	bus    bus.Bus
	log    *slog.Logger
	now    func() time.Time
}

func New(db *gorm.DB, pub events.Publisher, b bus.Bus, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), events: pub, bus: b, log: log,
		now: func() time.Time { return time.Now().UTC() }}
}

type View struct {
	ID            string     `json:"id"`
	MerchantID    string     `json:"merchant_id"`
	Environment   string     `json:"environment"`
	Amount        int64      `json:"amount"`
	Currency      string     `json:"currency"`
	Status        string     `json:"status"`
	BankName      string     `json:"bank_name"`
	AccountNumber string     `json:"account_number"` // masked
	AccountName   string     `json:"account_name"`
	Description   string     `json:"description,omitempty"`
	BankReference string     `json:"bank_reference,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	PaidAt        *time.Time `json:"paid_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func view(p *domain.Payout) View {
	if p == nil {
		return View{}
	}
	return View{ID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment, Amount: p.Amount, Currency: p.Currency,
		Status: p.Status, BankName: p.BankName, AccountNumber: p.MaskedAccount(), AccountName: p.AccountName,
		Description: p.Description, BankReference: p.BankReference, FailureReason: p.FailureReason, PaidAt: p.PaidAt,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

type RequestInput struct {
	Amount      int64              `json:"amount"`
	Currency    string             `json:"currency"`
	Environment string             `json:"environment"` // users: live (default) or test; API keys: their own
	BankAccount domain.BankAccount `json:"bank_account"`
	Description string             `json:"description"`
}

// Request creates a payout. The bool is false when an earlier result is
// replayed for the same Idempotency-Key.
func (s *Service) Request(ctx context.Context, actor reqctx.Principal, idemKey string, in RequestInput) (View, bool, error) {
	if idemKey == "" || len(idemKey) > 100 {
		return View{}, false, apperror.Invalid("idempotency_key_required", "send a unique Idempotency-Key header")
	}
	if actor.Environment != "" {
		in.Environment = actor.Environment
	} else if in.Environment == "" {
		in.Environment = "live"
	}
	hash := hashOf(in)
	if prev, err := s.replay(s.db.WithContext(ctx), actor.MerchantID, idemKey, hash); prev != nil || err != nil {
		return view(prev), false, err
	}
	m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: actor.MerchantID})
	if err != nil {
		return View{}, false, err
	}
	if m.Status != merchants.StatusActive && !(m.Status == merchants.StatusPending && in.Environment == "test") {
		return View{}, false, apperror.Forbidden("merchant_not_approved", "live payouts need an approved, active merchant")
	}
	p, err := domain.NewPayout(domain.NewPayoutInput{MerchantID: actor.MerchantID, Environment: in.Environment,
		Currency: in.Currency, Amount: in.Amount, Bank: in.BankAccount, Description: in.Description,
		IdempotencyKey: idemKey, RequestHash: hash, RequestedBy: actor.Subject}, s.now())
	if err != nil {
		return View{}, false, err
	}
	if err := s.store.Insert(s.db.WithContext(ctx), p); errors.Is(err, store.ErrDuplicate) {
		prev, err := s.replay(s.db.WithContext(ctx), actor.MerchantID, idemKey, hash)
		return view(prev), false, err
	} else if err != nil {
		return View{}, false, err
	}
	if err := s.reserve(ctx, p.ID); err != nil {
		var ae *apperror.Error
		if errors.As(err, &ae) && ae.Code == ledger.ErrInsufficientFunds.Code {
			return View{}, false, err
		}
		// Ledger unreachable: the payout stays "reserving" and the worker finishes it.
		s.log.Warn("payout reservation pending", "payout_id", p.ID, "err", err)
	}
	cur, err := s.store.Get(s.db.WithContext(ctx), p.ID, false)
	return view(cur), true, err
}

// reserve asks the ledger to hold the funds and records the answer.
func (s *Service) reserve(ctx context.Context, id string) error {
	p, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil || p.Status != payouts.StatusReserving {
		return err
	}
	_, askErr := bus.Ask(ctx, s.bus, ledger.ReserveFunds{PayoutID: p.ID, MerchantID: p.MerchantID,
		Environment: p.Environment, Currency: p.Currency, Amount: p.Amount})
	insufficient := errors.Is(askErr, ledger.ErrInsufficientFunds)
	if askErr != nil && !insufficient {
		return askErr
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		cur, err := s.store.Get(tx, id, true)
		if err != nil || cur.Status != payouts.StatusReserving {
			return err
		}
		if insufficient {
			err = cur.Reject(apperror.From(askErr).Message, s.now())
		} else {
			err = cur.Reserved(s.now())
		}
		if err != nil {
			return err
		}
		return s.store.Save(tx, cur)
	})
	if err != nil {
		return err
	}
	return askErr
}

// Worker finishes payouts whose reservation could not complete (ledger down).
func (s *Service) Worker(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		stuck, err := s.store.Stuck(s.db.WithContext(ctx), s.now().Add(-30*time.Second), 50)
		if err != nil {
			s.log.Error("list stuck payouts", "err", err)
			continue
		}
		for _, p := range stuck {
			if err := s.reserve(ctx, p.ID); err != nil && !errors.Is(err, ledger.ErrInsufficientFunds) {
				s.log.Warn("payout reservation retry failed", "payout_id", p.ID, "err", err)
			}
		}
	}
}

// MarkPaid: platform finance confirms the bank transfer went out.
func (s *Service) MarkPaid(ctx context.Context, actor reqctx.Principal, id, bankRef string) (View, error) {
	return s.process(ctx, id, func(p *domain.Payout, now time.Time) (bus.Event, error) {
		if err := p.MarkPaid(bankRef, actor.Subject, now); err != nil {
			return nil, err
		}
		return payouts.PayoutPaid{PayoutID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
			Amount: p.Amount, Currency: p.Currency, BankReference: p.BankReference, OccurredAt: now}, nil
	})
}

// MarkFailed: the transfer bounced; the held funds go back to the balance.
func (s *Service) MarkFailed(ctx context.Context, actor reqctx.Principal, id, reason string) (View, error) {
	return s.process(ctx, id, func(p *domain.Payout, now time.Time) (bus.Event, error) {
		if err := p.MarkFailed(reason, actor.Subject, now); err != nil {
			return nil, err
		}
		return payouts.PayoutFailed{PayoutID: p.ID, MerchantID: p.MerchantID, Environment: p.Environment,
			Amount: p.Amount, Currency: p.Currency, Reason: p.FailureReason, OccurredAt: now}, nil
	})
}

func (s *Service) process(ctx context.Context, id string, fn func(*domain.Payout, time.Time) (bus.Event, error)) (View, error) {
	var out *domain.Payout
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		p, err := s.store.Get(tx, id, true)
		if err != nil {
			return err
		}
		evt, err := fn(p, s.now())
		if err != nil {
			return err
		}
		if err := s.store.Save(tx, p); err != nil {
			return err
		}
		out = p
		return s.events.Publish(ctx, tx, evt)
	})
	return view(out), err
}

func (s *Service) Get(ctx context.Context, merchantID, id string) (View, error) {
	p, err := s.store.Get(s.db.WithContext(ctx), id, false)
	if err != nil {
		return View{}, err
	}
	if merchantID != "" && p.MerchantID != merchantID {
		return View{}, store.ErrNotFound
	}
	return view(p), nil
}

type Page struct {
	Data       []View `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// List: merchantID "" lists every merchant's payouts (platform staff).
func (s *Service) List(ctx context.Context, merchantID, status, startingAfter string, limit int) (Page, error) {
	ps, more, err := s.store.List(s.db.WithContext(ctx), merchantID, status, startingAfter, limit)
	if err != nil {
		return Page{}, err
	}
	page := Page{Data: make([]View, len(ps)), HasMore: more}
	for i, p := range ps {
		page.Data[i] = view(p)
	}
	if more {
		page.NextCursor = ps[len(ps)-1].ID
	}
	return page, nil
}

func (s *Service) replay(q *gorm.DB, merchantID, key, hash string) (*domain.Payout, error) {
	prev, err := s.store.ByIdempotencyKey(q, merchantID, key)
	if err != nil || prev == nil {
		return nil, err
	}
	if prev.RequestHash != hash {
		return nil, apperror.Conflict("idempotency_key_reused", "this Idempotency-Key was already used with a different request body")
	}
	return prev, nil
}

func hashOf(in RequestInput) string {
	b, _ := json.Marshal(in)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
