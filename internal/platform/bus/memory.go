package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Memory is the in-process bus used when every module runs in one process.
// Events are delivered synchronously; if a subscriber fails, the publishing
// relay retries later and idempotent consumers skip work already done.
type Memory struct {
	security
	mu       sync.RWMutex
	handlers map[string]RawHandler
	subs     map[string][]subscription
	log      *slog.Logger
}

type subscription struct {
	topic, consumer string
	handler         EventHandler
}

var _ Bus = (*Memory)(nil)

func NewMemory(log *slog.Logger) *Memory {
	return &Memory{handlers: map[string]RawHandler{}, subs: map[string][]subscription{}, log: log}
}

func (m *Memory) HandleRaw(route string, h RawHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.handlers[route]; dup {
		panic("bus: duplicate handler for route " + route)
	}
	m.handlers[route] = h
}

func (m *Memory) RequestRaw(ctx context.Context, route string, req []byte) ([]byte, error) {
	m.mu.RLock()
	h, ok := m.handlers[route]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNoHandler.WithMessage("no module handles " + route)
	}
	return h(ctx, req), nil
}

func (m *Memory) Subscribe(topic, consumer string, h EventHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[topic] = append(m.subs[topic], subscription{topic, consumer, h})
}

// SubscribeAll: in one process every subscriber already sees every event.
func (m *Memory) SubscribeAll(topic string, _ time.Duration, h EventHandler) {
	m.Subscribe(topic, "all:"+topic, h)
}

func (m *Memory) Publish(ctx context.Context, e Envelope) error {
	m.mu.RLock()
	subs := m.subs[e.Topic]
	m.mu.RUnlock()
	var errs []error
	for _, s := range subs {
		if err := safeEvent(ctx, s.handler, e); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.consumer, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Memory) Start(context.Context) error { return nil }
func (m *Memory) Close() error                { return nil }

func (m *Memory) Routes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sortedKeys(m.handlers)
}

func (m *Memory) logHandlerError(route string, err error) {
	m.log.Error("bus handler failed", "route", route, "err", err)
}

func safeEvent(ctx context.Context, h EventHandler, e Envelope) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return h(ctx, e)
}

func sortedKeys(m map[string]RawHandler) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
