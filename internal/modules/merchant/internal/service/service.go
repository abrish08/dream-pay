package service

import (
	"context"
	"time"

	"gorm.io/gorm"

	"paygate/internal/contracts/kyc"
	"paygate/internal/contracts/merchants"
	"paygate/internal/modules/merchant/internal/domain"
	"paygate/internal/modules/merchant/internal/store"
	"paygate/internal/platform/bus"
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

// Create: staff create active merchants; self-signup (via auth) creates pending ones.
func (s *Service) Create(ctx context.Context, in domain.NewMerchantInput) (merchants.Merchant, error) {
	m, err := domain.NewMerchant(in, s.now())
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
	return s.change(ctx, id, func(m *domain.Merchant) error { return m.SetStatus(status, s.now()) })
}

// SetFee changes the fee for future payments (past payments keep theirs).
func (s *Service) SetFee(ctx context.Context, id string, feeBPS int64) (merchants.Merchant, error) {
	return s.change(ctx, id, func(m *domain.Merchant) error { return m.SetFee(feeBPS, s.now()) })
}

type Page struct {
	Data       []merchants.Merchant `json:"data"`
	HasMore    bool                 `json:"has_more"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

func (s *Service) List(ctx context.Context, status, startingAfter string, limit int) (Page, error) {
	ms, more, err := s.store.List(ctx, status, startingAfter, limit)
	if err != nil {
		return Page{}, err
	}
	page := Page{Data: make([]merchants.Merchant, len(ms)), HasMore: more}
	for i := range ms {
		page.Data[i] = toAPI(&ms[i])
	}
	if more {
		page.NextCursor = ms[len(ms)-1].ID
	}
	return page, nil
}

func (s *Service) change(ctx context.Context, id string, fn func(*domain.Merchant) error) (merchants.Merchant, error) {
	var m *domain.Merchant
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		st := s.store.WithTx(tx)
		var err error
		if m, err = st.GetForUpdate(ctx, id); err != nil {
			return err
		}
		if err := fn(m); err != nil {
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

// OnKYCStatusChanged consumes kyc.status_changed: records it and activates a
// pending merchant whose verification was approved.
func (s *Service) OnKYCStatusChanged(ctx context.Context, e bus.Envelope, evt kyc.StatusChanged) error {
	return events.HandleOnce(ctx, s.db, "merchant.on_kyc_status_changed", e, func(tx *gorm.DB) error {
		st := s.store.WithTx(tx)
		m, err := st.GetForUpdate(ctx, evt.MerchantID)
		if err != nil {
			return err
		}
		if !m.OnKYC(evt.Status, s.now()) {
			return nil
		}
		if err := st.Save(ctx, m); err != nil {
			return err
		}
		return s.events.Publish(ctx, tx, updated(m))
	})
}

func updated(m *domain.Merchant) merchants.MerchantUpdated {
	return merchants.MerchantUpdated{ID: m.ID, Status: m.Status, KYCStatus: m.KYCStatus, FeeBPS: m.FeeBPS, UpdatedAt: m.UpdatedAt}
}

func toAPI(m *domain.Merchant) merchants.Merchant {
	return merchants.Merchant{ID: m.ID, Name: m.Name, Email: m.Email, Phone: m.Phone, Website: m.Website,
		Status: m.Status, FeeBPS: m.FeeBPS, KYCStatus: m.KYCStatus, ApprovedAt: m.ApprovedAt, CreatedAt: m.CreatedAt}
}
