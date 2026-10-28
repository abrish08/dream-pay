// Package bus is the single, central communication channel between modules.
//
// Modules never import each other. They only import message definitions from
// internal/contracts and talk through a Bus:
//
//	Requests (sync, one handler answers):  bus.Ask(ctx, b, merchants.GetMerchant{ID: id})
//	Events   (async, many subscribers):    published through the module's outbox, received with bus.On
//
// Two transports implement Bus:
//
//	Memory: every module in one process (monolith).
//	NATS:   modules spread over processes. Requests use NATS request-reply with
//	        queue groups (load balancing + discovery, no service URLs);
//	        events use JetStream (durable, at-least-once).
//
// Messages are always JSON-encoded, even in memory, so a module behaves
// exactly the same whether its peer is in-process or in another service.
package bus

import (
	"context"
	"encoding/json"
	"time"
)

// Envelope is an event as it travels on the bus.
type Envelope struct {
	ID         string            `json:"id"`
	Topic      string            `json:"topic"`
	Source     string            `json:"source"`
	Payload    json.RawMessage   `json:"payload"`
	Headers    map[string]string `json:"headers,omitempty"`
	OccurredAt time.Time         `json:"occurred_at"`
}

type EventHandler func(ctx context.Context, e Envelope) error

// RawHandler answers an encoded request with an encoded response. Use the
// typed helpers Handle and Ask instead of calling the raw methods directly.
type RawHandler func(ctx context.Context, req []byte) []byte

type Bus interface {
	// HandleRaw registers the handler for a request route (one per route per process).
	HandleRaw(route string, h RawHandler)
	// RequestRaw delivers a request to whichever process handles the route.
	RequestRaw(ctx context.Context, route string, req []byte) ([]byte, error)

	// Subscribe registers an event handler. consumer must be unique per handler;
	// it is the idempotency namespace and the durable consumer name.
	Subscribe(topic, consumer string, h EventHandler)
	// SubscribeAll registers a handler that runs in EVERY process (not load
	// balanced, no retries), e.g. to keep an in-memory cache fresh. A process
	// that starts later first replays the events of the last `replay` period.
	SubscribeAll(topic string, replay time.Duration, h EventHandler)
	// Publish delivers an event to all subscribers (called by outbox relays).
	Publish(ctx context.Context, e Envelope) error

	Start(ctx context.Context) error
	Close() error

	// Routes lists the request routes handled in this process.
	Routes() []string

	// SetVerifier sets how tokens carried by requests are verified.
	SetVerifier(v TokenVerifier)
}

// Backoff is the retry delay for a failed delivery attempt (1-based).
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second << min(attempt-1, 9) // 1s, 2s, 4s ... 512s
	return min(d, 5*time.Minute)
}
