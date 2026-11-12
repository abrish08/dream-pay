// Package service: storing and reading the audit log.
package service

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/audit"
	"paygate/internal/modules/audit/internal/store"
	"paygate/internal/platform/apperror"
)

type Service struct {
	db    *gorm.DB
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
}

func New(db *gorm.DB, log *slog.Logger) *Service {
	return &Service{db: db, store: store.New(), log: log, now: time.Now}
}

// Record answers audit.Record (from every process's recorder).
func (s *Service) Record(ctx context.Context, q audit.Record) (audit.Recorded, error) {
	e := q.Entry
	if e.Action == "" || e.ActorKind == "" || e.Outcome == "" || e.OccurredAt.IsZero() {
		return audit.Recorded{}, apperror.Invalid("invalid_audit_entry", "action, actor_kind, outcome and occurred_at are required")
	}
	id, err := s.store.Append(s.db.WithContext(ctx), store.FromEntry(e, s.now()))
	return audit.Recorded{ID: id}, err
}

type Page struct {
	Data       []store.Row `json:"data"`
	HasMore    bool        `json:"has_more"`
	NextCursor int64       `json:"next_cursor,omitempty"`
}

// List: merchants see only their own business (MerchantID is forced by the
// caller from the signed token); staff may filter any merchant or none.
func (s *Service) List(ctx context.Context, f store.Filter) (Page, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, more, err := s.store.List(s.db.WithContext(ctx), f)
	if err != nil {
		return Page{}, err
	}
	p := Page{Data: rows, HasMore: more}
	if rows == nil {
		p.Data = []store.Row{}
	}
	if more {
		p.NextCursor = rows[len(rows)-1].ID
	}
	return p, nil
}

// Verify recomputes the hash chain (staff only).
func (s *Service) Verify(ctx context.Context) (store.VerifyResult, error) {
	res, err := s.store.Verify(s.db.WithContext(ctx), 2000)
	if err == nil && !res.Intact {
		s.log.Error("audit log integrity check failed", "alert", true, "broken_at", res.BrokenAt, "problem", res.Problem)
	}
	return res, err
}
