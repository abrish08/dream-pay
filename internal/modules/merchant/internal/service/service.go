package service

import (
	"context"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/merchant/internal/domain"
	"paygate/internal/modules/merchant/internal/store"
	"paygate/internal/platform/events"
)

type Service struct {
	db     *gorm.DB
	store  *store.Store
	events events.Publisher
	now    func() time.Time
}

func New(db *gorm.DB, s *store.Store, pub events.Publisher) *Service {
	return &Service{db: db, store: s, events: pub, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, name, email string, feeBPS int64) (merchants.Merchant, error) {
	m, err := domain.NewMerchant(name, email, feeBPS, s.now())
	if err != nil {
		return merchants.Merchant{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.store.WithTx(tx).Create(ctx, m); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, updated(m))
	})
	if err != nil {
		return merchants.Merchant{}, err
	}
	return toAPI(m), nil
}

func (s *Service) GetMerchant(ctx context.Context, id string) (merchants.Merchant, error) {
	m, err := s.store.Get(ctx, id)
	if err != nil {
		return merchants.Merchant{}, err
	}
	return toAPI(m), nil
}

func (s *Service) SetStatus(ctx context.Context, id, status string) (merchants.Merchant, error) {
	var m *domain.Merchant
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		st := s.store.WithTx(tx)
		var err error
		if m, err = st.Get(ctx, id); err != nil {
			return err
		}
		if err := m.SetStatus(status, s.now()); err != nil {
			return err
		}
		if err := st.Save(ctx, m); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, updated(m))
	})
	if err != nil {
		return merchants.Merchant{}, err
	}
	return toAPI(m), nil
}

func updated(m *domain.Merchant) merchants.MerchantUpdated {
	return merchants.MerchantUpdated{ID: m.ID, Status: m.Status, FeeBPS: m.FeeBPS, UpdatedAt: m.UpdatedAt}
}

func toAPI(m *domain.Merchant) merchants.Merchant {
	return merchants.Merchant{ID: m.ID, Name: m.Name, Email: m.Email, Status: m.Status, FeeBPS: m.FeeBPS, CreatedAt: m.CreatedAt}
}
