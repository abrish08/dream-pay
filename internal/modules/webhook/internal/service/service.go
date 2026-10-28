package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"paygate/internal/modules/webhook/internal/domain"
	"paygate/internal/modules/webhook/internal/store"
	"paygate/internal/platform/bus"
	"paygate/internal/platform/events"
	"paygate/internal/platform/secrets"
)

type EndpointView struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

type CreatedEndpoint struct {
	EndpointView
	Secret  string `json:"secret"`
	Warning string `json:"warning"`
}

type DeliveryView struct {
	ID             string     `json:"id"`
	EndpointID     string     `json:"endpoint_id"`
	EventID        string     `json:"event_id"`
	EventType      string     `json:"event_type"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	LastStatusCode int        `json:"last_status_code"`
	LastError      string     `json:"last_error,omitempty"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	DeliveredAt    *time.Time `json:"delivered_at"`
}

type Service struct {
	db            *gorm.DB
	store         *store.Store
	box           *secrets.Box
	hc            *http.Client
	allowInsecure bool
	log           *slog.Logger
	now           func() time.Time
}

func New(db *gorm.DB, box *secrets.Box, allowInsecure bool, log *slog.Logger) *Service {
	return &Service{
		db: db, store: store.New(), box: box, allowInsecure: allowInsecure, log: log,
		now: func() time.Time { return time.Now().UTC() },
		hc: &http.Client{
			Timeout: 10 * time.Second,
			// Never follow redirects to other hosts (SSRF hygiene).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (s *Service) CreateEndpoint(ctx context.Context, merchantID, rawURL string, evts []string) (CreatedEndpoint, error) {
	if err := domain.ValidateURL(rawURL, s.allowInsecure); err != nil {
		return CreatedEndpoint{}, err
	}
	if err := domain.ValidateEvents(evts); err != nil {
		return CreatedEndpoint{}, err
	}
	tok, err := secrets.RandomToken(24)
	if err != nil {
		return CreatedEndpoint{}, err
	}
	secret := "whsec_" + tok
	enc, err := s.box.Encrypt(secret) // encrypted, not hashed: we need it to sign
	if err != nil {
		return CreatedEndpoint{}, err
	}
	e := domain.Endpoint{ID: uuid.NewString(), MerchantID: merchantID, URL: rawURL, Events: evts,
		SecretEncrypted: enc, Active: true, CreatedAt: s.now()}
	if err := s.store.CreateEndpoint(s.db.WithContext(ctx), e); err != nil {
		return CreatedEndpoint{}, err
	}
	return CreatedEndpoint{EndpointView: endpointView(e), Secret: secret,
		Warning: "Store this signing secret now. It will not be shown again."}, nil
}

func (s *Service) ListEndpoints(ctx context.Context, merchantID string) ([]EndpointView, error) {
	es, err := s.store.ListEndpoints(s.db.WithContext(ctx), merchantID, false)
	if err != nil {
		return nil, err
	}
	out := make([]EndpointView, len(es))
	for i, e := range es {
		out[i] = endpointView(e)
	}
	return out, nil
}

func (s *Service) DeleteEndpoint(ctx context.Context, merchantID, id string) error {
	return s.store.DeactivateEndpoint(s.db.WithContext(ctx), merchantID, id)
}

func (s *Service) ListDeliveries(ctx context.Context, merchantID string, limit int) ([]DeliveryView, error) {
	ds, err := s.store.ListDeliveries(s.db.WithContext(ctx), merchantID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DeliveryView, len(ds))
	for i, d := range ds {
		out[i] = DeliveryView{ID: d.ID, EndpointID: d.EndpointID, EventID: d.EventID, EventType: d.EventType,
			Status: d.Status, Attempts: d.Attempts, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
			NextAttemptAt: d.NextAttemptAt, DeliveredAt: d.DeliveredAt}
	}
	return out, nil
}

// OnEvent fans one internal event out to a delivery row per matching endpoint.
// No HTTP happens here; the delivery worker sends them.
func (s *Service) OnEvent(ctx context.Context, e bus.Envelope) error {
	eventType, ok := domain.PublicEventTypes[e.Topic]
	if !ok {
		return nil
	}
	var owner struct {
		MerchantID string `json:"merchant_id"`
	}
	if err := json.Unmarshal(e.Payload, &owner); err != nil || owner.MerchantID == "" {
		return fmt.Errorf("event %s has no merchant_id", e.ID)
	}
	return events.HandleOnce(ctx, s.db, "webhook.fanout", e, func(tx *gorm.DB) error {
		endpoints, err := s.store.ListEndpoints(tx, owner.MerchantID, true)
		if err != nil {
			return err
		}
		body, err := json.Marshal(map[string]any{
			"id": e.ID, "type": eventType, "created": e.OccurredAt.Unix(), "data": e.Payload,
		})
		if err != nil {
			return err
		}
		now := s.now()
		for _, ep := range endpoints {
			if !ep.Wants(eventType) {
				continue
			}
			if err := s.store.InsertDelivery(tx, &domain.Delivery{
				ID: uuid.NewString(), EndpointID: ep.ID, MerchantID: ep.MerchantID, EventID: e.ID,
				EventType: eventType, Payload: string(body), Status: "pending", NextAttemptAt: now, CreatedAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeliveryWorker sends due deliveries with exponential backoff.
func (s *Service) DeliveryWorker(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		batch, err := s.store.ClaimDue(s.db.WithContext(ctx), s.now(), 20, 2*time.Minute)
		if err != nil {
			s.log.Error("claim webhook deliveries", "err", err)
			continue
		}
		for i := range batch {
			s.deliver(ctx, &batch[i])
		}
	}
}

func (s *Service) deliver(ctx context.Context, d *domain.Delivery) {
	code, sendErr := s.send(ctx, d)
	now := s.now()
	d.Attempts++
	d.LockedUntil = nil
	d.LastStatusCode = code
	switch {
	case sendErr == nil:
		d.Status, d.LastError, d.DeliveredAt = "delivered", "", &now
	case d.Attempts >= domain.MaxAttempts:
		d.Status, d.LastError = "failed", sendErr.Error()
	default:
		d.LastError = sendErr.Error()
		d.NextAttemptAt = now.Add(domain.NextAttempt(d.Attempts))
	}
	if err := s.store.SaveDelivery(s.db.WithContext(ctx), d); err != nil {
		s.log.Error("save webhook delivery", "delivery_id", d.ID, "err", err)
	}
}

func (s *Service) send(ctx context.Context, d *domain.Delivery) (int, error) {
	ep, err := s.store.GetEndpoint(s.db.WithContext(ctx), d.EndpointID)
	if err != nil {
		return 0, err
	}
	if !ep.Active {
		return 0, errors.New("endpoint disabled")
	}
	secret, err := s.box.Decrypt(ep.SecretEncrypted)
	if err != nil {
		return 0, fmt.Errorf("decrypt secret: %w", err)
	}
	body := []byte(d.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "paygate-webhooks/1.0")
	req.Header.Set("Paygate-Event-Id", d.EventID)
	req.Header.Set("Paygate-Event-Type", d.EventType)
	req.Header.Set("Paygate-Signature", domain.Sign(secret, s.now().Unix(), body))

	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func endpointView(e domain.Endpoint) EndpointView {
	return EndpointView{ID: e.ID, URL: e.URL, Events: e.Events, Active: e.Active, CreatedAt: e.CreatedAt}
}
