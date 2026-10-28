package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/reconciliation/internal/domain"
	"paygate/internal/platform/apperror"
)

var ErrRunNotFound = apperror.NotFound("reconciliation_run_not_found", "reconciliation run not found")

const (
	runs  = "reconciliation_runs"
	items = "reconciliation_items"
)

type Store struct{}

func New() *Store { return &Store{} }

// InsertRun returns false if a daily run for the same period already exists.
func (s *Store) InsertRun(q *gorm.DB, r *domain.Run) (bool, error) {
	res := q.Table(runs).Clauses(clause.OnConflict{DoNothing: true}).Create(r)
	return res.RowsAffected == 1, res.Error
}

func (s *Store) GetRun(q *gorm.DB, id string) (*domain.Run, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrRunNotFound
	}
	var r domain.Run
	err := q.Table(runs).Where("id = ?", id).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRunNotFound
	}
	return &r, err
}

func (s *Store) ListRuns(q *gorm.DB, limit int) ([]domain.Run, error) {
	var out []domain.Run
	err := q.Table(runs).Order("created_at DESC").Limit(limit).Find(&out).Error
	return out, err
}

// ClaimQueued marks one queued run as running. SKIP LOCKED makes it safe
// with several replicas.
func (s *Store) ClaimQueued(q *gorm.DB, now time.Time) (*domain.Run, error) {
	var r domain.Run
	err := q.Transaction(func(tx *gorm.DB) error {
		err := tx.Table(runs).Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", domain.RunQueued).Order("created_at").Take(&r).Error
		if err != nil {
			return err
		}
		r.Status, r.StartedAt = domain.RunRunning, &now
		return tx.Table(runs).Where("id = ?", r.ID).Updates(map[string]any{"status": r.Status, "started_at": now}).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &r, err
}

func (s *Store) SaveRun(q *gorm.DB, r *domain.Run) error {
	return q.Table(runs).Where("id = ?", r.ID).Select("*").Updates(r).Error
}

func (s *Store) InsertItem(q *gorm.DB, it *domain.Item) error {
	return q.Table(items).Create(it).Error
}

func (s *Store) Items(q *gorm.DB, runID string) ([]domain.Item, error) {
	var out []domain.Item
	err := q.Table(items).Where("run_id = ?", runID).Order("id").Find(&out).Error
	return out, err
}
