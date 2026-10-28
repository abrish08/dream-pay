package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"paygate/internal/platform/apperror"
	"paygate/internal/platform/reqctx"
)

const (
	streamName     = "PAYGATE_EVENTS"
	eventPrefix    = "paygate.events."
	requestPrefix  = "paygate.req."
	defaultTimeout = 10 * time.Second
	maxInFlight    = 64 // concurrent requests per route per process
)

// NATS carries requests and events between processes.
//
//   - Requests: NATS request-reply. Each route is a subject; every process that
//     runs the owning module joins a queue group on it, so NATS load-balances
//     between replicas and callers never need a service URL.
//   - Events: JetStream stream with one durable consumer per subscriber.
//
// Requests for a route handled in this same process skip the network.
type NATS struct {
	security
	nc  *nats.Conn
	js  jetstream.JetStream
	log *slog.Logger

	mu        sync.RWMutex
	handlers  map[string]RawHandler
	subs      []subscription
	all       []broadcast
	natsSubs  []*nats.Subscription
	consuming []jetstream.ConsumeContext
}

var _ Bus = (*NATS)(nil)

func NewNATS(ctx context.Context, url string, log *slog.Logger) (*NATS, error) {
	nc, err := nats.Connect(url, nats.Name("paygate"), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second))
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       streamName,
		Subjects:   []string{eventPrefix + ">"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     7 * 24 * time.Hour,
		Duplicates: 2 * time.Minute, // dedupe window keyed by event id
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("create stream: %w", err)
	}
	return &NATS{nc: nc, js: js, log: log, handlers: map[string]RawHandler{}}, nil
}

func (b *NATS) HandleRaw(route string, h RawHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, dup := b.handlers[route]; dup {
		panic("bus: duplicate handler for route " + route)
	}
	b.handlers[route] = h
}

func (b *NATS) RequestRaw(ctx context.Context, route string, req []byte) ([]byte, error) {
	b.mu.RLock()
	h, local := b.handlers[route]
	b.mu.RUnlock()
	if local {
		return h(ctx, req), nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	msg, err := b.nc.RequestWithContext(ctx, requestPrefix+route, req)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return nil, ErrNoHandler.WithMessage("no running service handles " + route)
	case err != nil:
		return nil, apperror.Unavailable("bus_request_failed", fmt.Sprintf("%s: %v", route, err))
	}
	return msg.Data, nil
}

func (b *NATS) Subscribe(topic, consumer string, h EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, subscription{topic, consumer, h})
}

type broadcast struct {
	topic   string
	replay  time.Duration
	handler EventHandler
}

func (b *NATS) SubscribeAll(topic string, replay time.Duration, h EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.all = append(b.all, broadcast{topic, replay, h})
}

func (b *NATS) Publish(ctx context.Context, e Envelope) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = b.js.Publish(ctx, eventPrefix+e.Topic, data, jetstream.WithMsgID(e.ID))
	return err
}

func (b *NATS) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for route, h := range b.handlers {
		route, h := route, h
		sem := make(chan struct{}, maxInFlight)
		sub, err := b.nc.QueueSubscribe(requestPrefix+route, "paygate."+route, func(m *nats.Msg) {
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				if err := m.Respond(h(ctx, m.Data)); err != nil {
					b.log.Warn("bus respond failed", "route", route, "err", err)
				}
			}()
		})
		if err != nil {
			return fmt.Errorf("serve %s: %w", route, err)
		}
		b.natsSubs = append(b.natsSubs, sub)
	}

	for _, s := range b.subs {
		s := s
		cons, err := b.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
			Durable:       durableName(s.consumer),
			FilterSubject: eventPrefix + s.topic,
			AckPolicy:     jetstream.AckExplicitPolicy,
			AckWait:       30 * time.Second,
			DeliverPolicy: jetstream.DeliverAllPolicy,
		})
		if err != nil {
			return fmt.Errorf("consumer %s: %w", s.consumer, err)
		}
		cc, err := cons.Consume(func(msg jetstream.Msg) { b.deliver(ctx, s, msg) })
		if err != nil {
			return fmt.Errorf("consume %s: %w", s.consumer, err)
		}
		b.consuming = append(b.consuming, cc)
	}
	// Broadcast subscriptions: an ordered (ephemeral, per-process) consumer, so
	// every replica receives every event, starting `replay` in the past.
	for _, s := range b.all {
		s := s
		start := time.Now().Add(-s.replay)
		cons, err := b.js.OrderedConsumer(ctx, streamName, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{eventPrefix + s.topic},
			DeliverPolicy:  jetstream.DeliverByStartTimePolicy,
			OptStartTime:   &start,
		})
		if err != nil {
			return fmt.Errorf("broadcast consumer %s: %w", s.topic, err)
		}
		cc, err := cons.Consume(func(msg jetstream.Msg) {
			var e Envelope
			if err := json.Unmarshal(msg.Data(), &e); err != nil {
				return
			}
			if err := safeEvent(ctx, s.handler, e); err != nil {
				b.log.Warn("broadcast handler failed", "topic", s.topic, "event_id", e.ID, "err", err)
			}
		})
		if err != nil {
			return fmt.Errorf("broadcast consume %s: %w", s.topic, err)
		}
		b.consuming = append(b.consuming, cc)
	}
	return b.nc.Flush()
}

func (b *NATS) deliver(ctx context.Context, s subscription, msg jetstream.Msg) {
	var e Envelope
	if err := json.Unmarshal(msg.Data(), &e); err != nil {
		b.log.Error("dropping malformed event", "consumer", s.consumer, "err", err)
		_ = msg.Term()
		return
	}
	hctx := reqctx.WithCorrelationID(ctx, e.Headers["correlation_id"])
	if err := safeEvent(hctx, s.handler, e); err != nil {
		attempt := 1
		if md, mdErr := msg.Metadata(); mdErr == nil {
			attempt = int(md.NumDelivered)
		}
		b.log.Warn("event handler failed, will retry",
			"consumer", s.consumer, "topic", e.Topic, "event_id", e.ID, "attempt", attempt, "err", err)
		_ = msg.NakWithDelay(Backoff(attempt))
		return
	}
	_ = msg.Ack()
}

func (b *NATS) Routes() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return sortedKeys(b.handlers)
}

func (b *NATS) Close() error {
	b.mu.Lock()
	for _, s := range b.natsSubs {
		_ = s.Unsubscribe()
	}
	for _, cc := range b.consuming {
		cc.Stop()
	}
	b.mu.Unlock()
	return b.nc.Drain()
}

func (b *NATS) logHandlerError(route string, err error) {
	b.log.Error("bus handler failed", "route", route, "err", err)
}

func durableName(consumer string) string {
	return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(consumer)
}
