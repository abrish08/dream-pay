package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/providers"
	"paygate/internal/modules/reconciliation/internal/domain"
	"paygate/internal/modules/reconciliation/internal/store"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/bus"
)

type RunView struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	PeriodStart time.Time     `json:"period_start"`
	PeriodEnd   time.Time     `json:"period_end"`
	Status      string        `json:"status"`
	Checked     int           `json:"checked"`
	Mismatched  int           `json:"mismatched"`
	Error       string        `json:"error,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	CompletedAt *time.Time    `json:"completed_at"`
	Items       []domain.Item `json:"items,omitempty"`
}

type Service struct {
	db    *gorm.DB
	store *store.Store
	bus   bus.Bus // the only way to reach other modules
	log   *slog.Logger
	now   func() time.Time
}

func New(db *gorm.DB, b bus.Bus, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), bus: b, log: log,
		now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) CreateRun(ctx context.Context, from, to time.Time) (RunView, error) {
	r, err := domain.NewRun(domain.KindManual, from.UTC(), to.UTC(), s.now())
	if err != nil {
		return RunView{}, err
	}
	if _, err := s.store.InsertRun(s.db.WithContext(ctx), r); err != nil {
		return RunView{}, err
	}
	return view(r, nil), nil
}

func (s *Service) GetRun(ctx context.Context, id string) (RunView, error) {
	r, err := s.store.GetRun(s.db.WithContext(ctx), id)
	if err != nil {
		return RunView{}, err
	}
	its, err := s.store.Items(s.db.WithContext(ctx), id)
	return view(r, its), err
}

func (s *Service) ListRuns(ctx context.Context, limit int) ([]RunView, error) {
	rs, err := s.store.ListRuns(s.db.WithContext(ctx), limit)
	out := make([]RunView, len(rs))
	for i := range rs {
		out[i] = view(&rs[i], nil)
	}
	return out, err
}

// Worker processes queued runs and schedules one run per day for yesterday.
func (s *Service) Worker(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var lastSchedule time.Time
	for {
		if s.now().Sub(lastSchedule) > time.Hour {
			s.scheduleDaily(ctx)
			lastSchedule = s.now()
		}
		for {
			r, err := s.store.ClaimQueued(s.db.WithContext(ctx), s.now())
			if err != nil {
				s.log.Error("claim reconciliation run", "err", err)
				break
			}
			if r == nil {
				break
			}
			s.process(ctx, r)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) scheduleDaily(ctx context.Context) {
	today := s.now().Truncate(24 * time.Hour)
	r, _ := domain.NewRun(domain.KindDaily, today.Add(-24*time.Hour), today, s.now())
	if _, err := s.store.InsertRun(s.db.WithContext(ctx), r); err != nil {
		s.log.Error("schedule daily reconciliation", "err", err)
	}
}

func (s *Service) process(ctx context.Context, r *domain.Run) {
	err := s.check(ctx, r)
	now := s.now()
	r.CompletedAt = &now
	r.Status = domain.RunCompleted
	if err != nil {
		r.Status, r.Error = domain.RunFailed, err.Error()
	}
	if err := s.store.SaveRun(s.db.WithContext(ctx), r); err != nil {
		s.log.Error("save reconciliation run", "run_id", r.ID, "err", err)
	}
	s.log.Info("reconciliation finished", "run_id", r.ID, "status", r.Status, "checked", r.Checked, "mismatched", r.Mismatched)
}

// check pages through our payments (payment contract) and verifies each with
// the provider (provider contract).
func (s *Service) check(ctx context.Context, r *domain.Run) error {
	after := ""
	for {
		page, err := bus.Ask(ctx, s.bus, payments.ListPayments{
			Statuses: []string{payments.StatusSucceeded, payments.StatusPartiallyRefunded,
				payments.StatusRefunded, payments.StatusFailed},
			CreatedFrom: r.PeriodStart, CreatedTo: r.PeriodEnd, AfterID: after, Limit: 200,
		})
		if err != nil {
			return err
		}
		for _, p := range page {
			if p.ProviderRef == "" {
				continue // never reached the provider (e.g. initiation failed)
			}
			r.Checked++
			if err := s.checkOne(ctx, r, p); err != nil {
				return err
			}
		}
		if len(page) < 200 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

func (s *Service) checkOne(ctx context.Context, r *domain.Run, p payments.Payment) error {
	item := &domain.Item{RunID: r.ID, PaymentID: p.ID, Provider: p.Provider, CreatedAt: s.now()}
	t, err := bus.Ask(ctx, s.bus, providers.VerifyTransaction{Provider: p.Provider, TxRef: p.ID})
	switch {
	case errors.Is(err, providers.ErrTransactionNotFound):
		item.Issue, item.Expected, item.Actual = domain.IssueMissingAtProvider, p.Status, "not_found"
	case err != nil:
		item.Issue, item.Expected, item.Actual = domain.IssueProviderError, p.Status, apperror.From(err).Message
	default:
		item.Issue, item.Expected, item.Actual = domain.Compare(p, t)
	}
	if item.Issue == "" {
		return nil
	}
	r.Mismatched++
	return s.store.InsertItem(s.db.WithContext(ctx), item)
}

func view(r *domain.Run, items []domain.Item) RunView {
	return RunView{ID: r.ID, Kind: r.Kind, PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Status: r.Status,
		Checked: r.Checked, Mismatched: r.Mismatched, Error: r.Error, CreatedAt: r.CreatedAt,
		CompletedAt: r.CompletedAt, Items: items}
}
