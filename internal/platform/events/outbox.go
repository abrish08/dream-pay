package events

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"paygate/internal/platform/bus"
	"paygate/internal/platform/reqctx"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the outbox/processed_events tables. Every module applies
// them into its own schema, so each module owns its outbox.
func Migrations() fs.FS {
	sub, _ := fs.Sub(migrationFiles, "migrations")
	return sub
}

type outboxRecord struct {
	ID            int64
	EventID       string
	Topic         string
	Payload       string
	Headers       string
	Attempts      int
	NextAttemptAt time.Time
	LastError     *string
	CreatedAt     time.Time
	PublishedAt   *time.Time
}

func (outboxRecord) TableName() string { return "outbox" }

// Outbox implements Publisher for one module.
type Outbox struct{}

func NewOutbox() *Outbox { return &Outbox{} }

func (o *Outbox) Publish(ctx context.Context, tx *gorm.DB, evt bus.Event) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if cid := reqctx.CorrelationID(ctx); cid != "" {
		headers["correlation_id"] = cid
	}
	hb, _ := json.Marshal(headers)
	now := time.Now().UTC()
	return tx.WithContext(ctx).Create(&outboxRecord{
		EventID:       uuid.NewString(),
		Topic:         evt.EventTopic(),
		Payload:       string(body),
		Headers:       string(hb),
		NextAttemptAt: now,
		CreatedAt:     now,
	}).Error
}

// Relay moves a module's unpublished outbox rows to the bus.
type Relay struct {
	db       *gorm.DB
	bus      bus.Bus
	source   string
	log      *slog.Logger
	interval time.Duration
	batch    int
}

func NewRelay(db *gorm.DB, b bus.Bus, source string, log *slog.Logger) *Relay {
	return &Relay{db: db, bus: b, source: source, log: log, interval: 200 * time.Millisecond, batch: 100}
}

func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		for {
			n, err := r.flush(ctx)
			if err != nil && ctx.Err() == nil {
				r.log.Error("outbox relay", "module", r.source, "err", err)
			}
			if err != nil || n < r.batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Relay) flush(ctx context.Context) (int, error) {
	var n int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var recs []outboxRecord
		// SKIP LOCKED lets several replicas run relays without double-sending.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("published_at IS NULL AND next_attempt_at <= ?", time.Now().UTC()).
			Order("id").Limit(r.batch).Find(&recs).Error; err != nil {
			return err
		}
		n = len(recs)
		for _, rec := range recs {
			env := bus.Envelope{
				ID:         rec.EventID,
				Topic:      rec.Topic,
				Source:     r.source,
				Payload:    json.RawMessage(rec.Payload),
				OccurredAt: rec.CreatedAt,
			}
			_ = json.Unmarshal([]byte(rec.Headers), &env.Headers)
			sendCtx := reqctx.WithCorrelationID(ctx, env.Headers["correlation_id"])

			if err := r.bus.Publish(sendCtx, env); err != nil {
				msg := truncate(err.Error(), 1000)
				r.log.Warn("event delivery failed, will retry",
					"module", r.source, "topic", rec.Topic, "event_id", rec.EventID, "attempt", rec.Attempts+1, "err", msg)
				if err := tx.Model(&outboxRecord{}).Where("id = ?", rec.ID).Updates(map[string]any{
					"attempts":        gorm.Expr("attempts + 1"),
					"next_attempt_at": time.Now().UTC().Add(bus.Backoff(rec.Attempts + 1)),
					"last_error":      msg,
				}).Error; err != nil {
					return err
				}
				continue
			}
			if err := tx.Model(&outboxRecord{}).Where("id = ?", rec.ID).
				Updates(map[string]any{"published_at": time.Now().UTC(), "last_error": nil}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// HandleOnce runs fn at most once per (event, consumer). The processed marker
// and fn's writes commit in the same transaction, so a crash or a failure in
// fn leaves the event unprocessed and it will be retried.
func HandleOnce(ctx context.Context, db *gorm.DB, consumer string, e bus.Envelope, fn func(tx *gorm.DB) error) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`INSERT INTO processed_events (event_id, consumer) VALUES (?, ?) ON CONFLICT DO NOTHING`, e.ID, consumer)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil // duplicate delivery
		}
		return fn(tx)
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
