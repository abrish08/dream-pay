package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/modules/webhook/internal/domain"
	"paygate/internal/platform/apperror"
	"paygate/internal/platform/database"
)

var ErrEndpointNotFound = apperror.NotFound("webhook_endpoint_not_found", "webhook endpoint not found")

type endpointRecord struct {
	ID              string
	MerchantID      string
	URL             string
	Events          database.StringList
	SecretEncrypted string
	Active          bool
	CreatedAt       time.Time
}

func (endpointRecord) TableName() string { return "webhook_endpoints" }

func (r endpointRecord) toDomain() domain.Endpoint {
	return domain.Endpoint{ID: r.ID, MerchantID: r.MerchantID, URL: r.URL, Events: r.Events,
		SecretEncrypted: r.SecretEncrypted, Active: r.Active, CreatedAt: r.CreatedAt}
}

const deliveries = "webhook_deliveries"

type Store struct{}

func New() *Store { return &Store{} }

func (s *Store) CreateEndpoint(q *gorm.DB, e domain.Endpoint) error {
	rec := endpointRecord{e.ID, e.MerchantID, e.URL, e.Events, e.SecretEncrypted, e.Active, e.CreatedAt}
	return q.Create(&rec).Error
}

func (s *Store) GetEndpoint(q *gorm.DB, id string) (domain.Endpoint, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Endpoint{}, ErrEndpointNotFound
	}
	var rec endpointRecord
	err := q.Where("id = ?", id).Take(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Endpoint{}, ErrEndpointNotFound
	}
	return rec.toDomain(), err
}

func (s *Store) ListEndpoints(q *gorm.DB, merchantID string, activeOnly bool) ([]domain.Endpoint, error) {
	q = q.Where("merchant_id = ?", merchantID)
	if activeOnly {
		q = q.Where("active")
	}
	var recs []endpointRecord
	if err := q.Order("created_at").Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]domain.Endpoint, len(recs))
	for i, r := range recs {
		out[i] = r.toDomain()
	}
	return out, nil
}

func (s *Store) DeactivateEndpoint(q *gorm.DB, merchantID, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrEndpointNotFound
	}
	res := q.Model(&endpointRecord{}).Where("id = ? AND merchant_id = ? AND active", id, merchantID).Update("active", false)
	if res.Error == nil && res.RowsAffected == 0 {
		return ErrEndpointNotFound
	}
	return res.Error
}

func (s *Store) InsertDelivery(q *gorm.DB, d *domain.Delivery) error {
	return q.Table(deliveries).Clauses(clause.OnConflict{DoNothing: true}).Create(d).Error
}

// ClaimDue locks due deliveries for this worker for a short lease, so several
// replicas never send the same delivery at the same time.
func (s *Store) ClaimDue(q *gorm.DB, now time.Time, limit int, lease time.Duration) ([]domain.Delivery, error) {
	var out []domain.Delivery
	err := q.Transaction(func(tx *gorm.DB) error {
		if err := tx.Table(deliveries).Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = 'pending' AND next_attempt_at <= ? AND (locked_until IS NULL OR locked_until < ?)", now, now).
			Order("next_attempt_at").Limit(limit).Find(&out).Error; err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		ids := make([]string, len(out))
		for i, d := range out {
			ids[i] = d.ID
		}
		return tx.Table(deliveries).Where("id IN ?", ids).Update("locked_until", now.Add(lease)).Error
	})
	return out, err
}

func (s *Store) SaveDelivery(q *gorm.DB, d *domain.Delivery) error {
	return q.Table(deliveries).Where("id = ?", d.ID).Select("*").Updates(d).Error
}

func (s *Store) ListDeliveries(q *gorm.DB, merchantID string, limit int) ([]domain.Delivery, error) {
	var out []domain.Delivery
	err := q.Table(deliveries).Where("merchant_id = ?", merchantID).Order("created_at DESC").Limit(limit).Find(&out).Error
	return out, err
}
