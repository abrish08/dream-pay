package service

import (
	"errors"
	"slices"
	"sync"
	"time"

	"paygate/internal/contracts/providers"
	"paygate/internal/modules/provider/internal/gateway"
	"paygate/internal/platform/apperror"
)

// Router decides which provider processes a payment method.
//
//	routes:  method -> providers in order of preference (PROVIDER_ROUTES)
//	breaker: a provider that keeps failing is skipped for a while, so
//	         customers are sent to the next provider instead of waiting on a
//	         dead one; it is tried again after the cool-down.
type Router struct {
	routes    map[string][]string
	allowMock bool // live payments may use the sandbox (development only)

	mu       sync.Mutex
	breakers map[string]*breaker
	now      func() time.Time
}

const (
	breakerThreshold = 5                // consecutive unavailable errors
	breakerCooldown  = 30 * time.Second // then one trial request is let through
)

type breaker struct {
	failures  int
	openUntil time.Time
	lastError string
}

func NewRouter(routes map[string][]string, allowMock bool) *Router {
	return &Router{routes: routes, allowMock: allowMock, breakers: map[string]*breaker{}, now: time.Now}
}

// Candidates returns, in order, the providers to try for a checkout.
func (r *Router) Candidates(gws map[string]gateway.Gateway, mock gateway.Gateway, req providers.InitiateCheckout) ([]gateway.Gateway, error) {
	method := req.Method
	if req.Provider != "" { // explicit provider (legacy API / staff tooling)
		g, ok := gws[req.Provider]
		if !ok {
			return nil, providers.ErrUnknownProvider.WithMessage("unknown payment provider: " + req.Provider)
		}
		if method == "" {
			return []gateway.Gateway{g}, nil
		}
	}
	if !providers.ValidMethod(method) {
		return nil, apperror.Invalid("invalid_payment_method", "unknown payment method "+method)
	}
	if req.Environment == "test" {
		return []gateway.Gateway{mock}, nil // test money never reaches a real provider
	}
	var out []gateway.Gateway
	names := r.routes[method]
	if r.allowMock {
		names = append(slices.Clone(names), mock.Name())
	}
	for _, name := range names {
		g, ok := gws[name]
		if !ok || !slices.Contains(g.Methods(), method) || !slices.Contains(g.Currencies(), req.Currency) {
			continue
		}
		if req.Provider != "" && name != req.Provider {
			continue
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, providers.ErrMethodUnavailable.WithMessage(method + " is not available for " + req.Currency)
	}
	// Healthy providers first; open breakers last (still a chance if all are open).
	slices.SortStableFunc(out, func(a, b gateway.Gateway) int {
		return boolInt(r.open(a.Name())) - boolInt(r.open(b.Name()))
	})
	return out, nil
}

// Retryable: only "the provider is down" errors fail over. A decline or a
// validation error from one provider would come back from the next one too.
func Retryable(err error) bool {
	var ae *apperror.Error
	if !errors.As(err, &ae) {
		return true // network / unexpected error
	}
	return ae.Kind == apperror.KindUnavailable || ae.Kind == apperror.KindInternal
}

func (r *Router) Success(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.breakers, provider)
}

func (r *Router) Failure(provider string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.breakers[provider]
	if b == nil {
		b = &breaker{}
		r.breakers[provider] = b
	}
	b.failures++
	b.lastError = err.Error()
	if b.failures >= breakerThreshold {
		b.openUntil = r.now().Add(breakerCooldown)
	}
}

func (r *Router) open(provider string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.breakers[provider]
	return b != nil && r.now().Before(b.openUntil)
}

// Available: the method can be routed to at least one provider whose breaker is closed.
func (r *Router) Available(gws map[string]gateway.Gateway, mock gateway.Gateway, env, method, currency string) ([]string, bool) {
	cands, err := r.Candidates(gws, mock, providers.InitiateCheckout{Method: method, Environment: env, Currency: currency})
	if err != nil {
		return nil, false
	}
	names := make([]string, len(cands))
	healthy := false
	for i, g := range cands {
		names[i] = g.Name()
		healthy = healthy || !r.open(g.Name())
	}
	return names, healthy
}

type ProviderHealth struct {
	Provider   string     `json:"provider"`
	Methods    []string   `json:"methods"`
	Currencies []string   `json:"currencies"`
	Healthy    bool       `json:"healthy"`
	Failures   int        `json:"consecutive_failures"`
	OpenUntil  *time.Time `json:"skipped_until,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
}

func (r *Router) Health(gws map[string]gateway.Gateway) []ProviderHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ProviderHealth, 0, len(gws))
	for name, g := range gws {
		h := ProviderHealth{Provider: name, Methods: g.Methods(), Currencies: g.Currencies(), Healthy: true}
		if b := r.breakers[name]; b != nil {
			h.Failures, h.LastError = b.failures, b.lastError
			if r.now().Before(b.openUntil) {
				t := b.openUntil
				h.Healthy, h.OpenUntil = false, &t
			}
		}
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b ProviderHealth) int {
		if a.Provider < b.Provider {
			return -1
		}
		return 1
	})
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
